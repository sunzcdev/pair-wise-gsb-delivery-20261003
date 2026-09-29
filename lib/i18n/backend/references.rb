# frozen_string_literal: true

# I18n fragment references allow a translation to embed the value of another
# translation (a "fragment") at explicitly marked positions, instead of
# replacing the whole value the way a Symbol alias does.
#
# To enable fragment references include the References module into your
# backend:
#
#   I18n::Backend::Simple.include(I18n::Backend::References)
#
# A reference is explicitly marked with the %{ref:key} syntax, so plain
# strings are never scanned for references:
#
#   en:
#     product: "Acme"
#     tagline: "%{ref:product} - %{ref:product} for everyone"
#
#   I18n.t(:tagline) # => "Acme - Acme for everyone"
#
# Each reference can carry its own interpolation arguments in parentheses.
# Literal values are used as given (integer looking values are converted to
# Integers so they can drive pluralization) and %{var} is resolved against
# the interpolation values of the enclosing translation:
#
#   en:
#     inbox:
#       one: "one message"
#       other: "%{count} messages"
#     summary: "Inbox: %{ref:inbox(count=2)}, Sent: %{ref:inbox(count=%{sent})}"
#
# References are resolved through I18n.translate using the locale of the
# current translation, so the configured fallback strategy applies to
# referenced fragments as well.
#
# Expansion is guarded by I18n::Backend::References.max_depth and
# I18n::Backend::References.max_length. Circular references raise
# I18n::CircularReference and missing fragments raise I18n::MissingReference,
# both reporting the whole reference chain.
module I18n
  module Backend
    module References
      # Matches explicitly marked references: %{ref:some.key} optionally
      # followed by per-reference interpolation arguments in parentheses.
      # A reference escaped as %%{ref:some.key} is left untouched.
      REFERENCE_PATTERN = /(?<!%)%\{ref:\s*([^()\s}]+?)\s*(?:\(((?:[^}{]|%\{[^}]*\})*)\))?\s*\}/

      DEFAULT_MAX_DEPTH = 5
      DEFAULT_MAX_LENGTH = 10_000

      class << self
        attr_writer :max_depth, :max_length

        # Maximum nesting depth of reference expansion.
        def max_depth
          @max_depth ||= DEFAULT_MAX_DEPTH
        end

        # Maximum length in characters of an expanded translation value.
        def max_length
          @max_length ||= DEFAULT_MAX_LENGTH
        end
      end

      I18n.reserve_key(:reference_chain)

      def translate(locale, key, options = EMPTY_HASH)
        return super if options[:skip_interpolation]

        chain = options[:reference_chain] || []
        key_name = reference_key_name(key, options[:scope])
        chain = chain + [key_name] unless key_name.empty?

        entry = super(locale, key, options.merge(:skip_interpolation => true))
        entry = expand_references(locale, entry, options, chain)

        deep_interpolation = options[:deep_interpolation]
        values = Utils.except(options, *RESERVED_KEYS) unless options.empty?
        if values && !values.empty?
          entry = deep_interpolation ? deep_interpolate(locale, entry, values) : interpolate(locale, entry, values)
        elsif entry.is_a?(String) && entry =~ I18n.reserved_keys_pattern
          raise ReservedInterpolationKey.new($1.to_sym, entry)
        end
        entry
      end

      protected

        def expand_references(locale, entry, options, chain)
          case entry
          when ::String
            expand_references_in_string(locale, entry, options, chain)
          when ::Array
            entry.map { |element| expand_references(locale, element, options, chain) }
          when ::Hash
            entry.each_with_object({}) do |(key, value), result|
              result[key] = expand_references(locale, value, options, chain)
            end
          else
            entry
          end
        end

        def expand_references_in_string(locale, string, options, chain)
          return string unless string.include?('%{ref:')

          expanded = string.gsub(REFERENCE_PATTERN) do
            expand_reference(locale, $1, $2, options, chain)
          end

          if expanded.length > References.max_length
            raise ReferenceExpansionTooLong.new(chain, References.max_length)
          end

          expanded
        end

        def expand_reference(locale, key, arguments, options, chain)
          key_name = reference_key_name(key, nil)
          reference_chain = chain + [key_name]

          raise CircularReference.new(reference_chain) if chain.include?(key_name)

          if chain.length > References.max_depth
            raise ReferenceDepthExceeded.new(reference_chain, References.max_depth)
          end

          values = reference_arguments(arguments, options, chain)
          result = catch(:exception) do
            I18n.translate(key, **values.merge(
              :locale => options[:fallback_original_locale] || locale,
              :throw => true,
              :reference_chain => chain
            ))
          end

          if result.is_a?(MissingTranslation)
            raise MissingReference.new(key_name, reference_chain, result.message)
          end

          case result
          when ::String
            if result.length > References.max_length
              raise ReferenceExpansionTooLong.new(reference_chain, References.max_length)
            end
            result
          when ::Hash, ::Array
            raise InvalidReference.new("reference #{key_name.inspect} resolves to #{result.inspect} which can not be embedded", reference_chain)
          else
            result.to_s
          end
        end

        def reference_arguments(arguments, options, chain)
          return EMPTY_HASH if arguments.nil? || arguments.strip.empty?

          values = options.empty? ? EMPTY_HASH : Utils.except(options, *RESERVED_KEYS)

          arguments.split(',').each_with_object({}) do |pair, args|
            name, value = pair.split('=', 2)
            if value.nil?
              raise InvalidReference.new("malformed reference argument #{pair.inspect}, expected name=value", chain)
            end
            args[name.strip.to_sym] = reference_argument_value(value.strip, values)
          end
        end

        def reference_argument_value(value, values)
          case value
          when /\A%\{(\w+)\}\z/
            key = $1.to_sym
            if values.key?(key)
              values[key]
            else
              I18n.config.missing_interpolation_argument_handler.call(key, values, value)
            end
          when /\A[-+]?\d+\z/
            value.to_i
          else
            value
          end
        end

        def reference_key_name(key, scope)
          I18n.normalize_keys(nil, key, scope).join('.')
        end
    end
  end
end
