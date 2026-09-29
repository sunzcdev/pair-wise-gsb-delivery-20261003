# frozen_string_literal: true

module I18n
  # Marker class for translations that contain fragment references.
  #
  # A fragment reference embeds another translation inside a message:
  #
  #   en:
  #     product_name: "Gadget"
  #     promo: "Deal of the day: ${ref:product_name}!"
  #
  # Only translations wrapped in this class (e.g. via I18n.fragment) have
  # their references expanded. Plain strings are returned untouched, so the
  # ${ref:...} syntax stays inert in regular translations.
  class FragmentString < String; end

  # Returns +string+ marked as containing fragment references.
  def self.fragment(string)
    FragmentString.new(string)
  end

  # Expands fragment references in translations marked as FragmentString.
  #
  # Each reference uses the current locale and the backend's fallback
  # strategy, and may carry its own interpolation parameters:
  #
  #   ${ref:greeting, name: Alice, count: 2}
  #
  # References are expanded recursively. Expansion is bounded by +max_depth+
  # and +max_expansion_length+, and circular references are reported with
  # the full reference chain.
  module FragmentReference
    # Matches ${ref:some.key} and ${ref:some.key, param: value, ...}
    # Parameter values containing "}" (e.g. "%{user}") must be quoted.
    REFERENCE_PATTERN = /\$\{ref:\s*([^,}\s]+)\s*(?:,((?:[^}"']|"[^"]*"|'[^']*')*))?\}/m
    PARAM_PATTERN = /(\w+)\s*:\s*("[^"]*"|'[^']*'|[^,]+)/

    DEFAULT_MAX_DEPTH = 8
    DEFAULT_MAX_EXPANSION_LENGTH = 16_384

    # Tracks the expansion state shared by a top-level translation and all
    # of its nested references: the reference chain (for cycle detection
    # and error reporting), the current nesting depth and the accumulated
    # expansion length.
    class State
      attr_reader :chain
      attr_accessor :depth

      def initialize
        @chain = []
        @depth = 0
        @length = 0
      end

      def enter(reference)
        raise FragmentReferenceCycle.new(@chain, reference) if @chain.include?(reference)

        @chain.push(reference)
      end

      def leave
        @chain.pop
      end

      def consume(length)
        @length += length
        if @length > FragmentReference.max_expansion_length
          raise FragmentReferenceExpansionTooLong.new(@chain, FragmentReference.max_expansion_length)
        end
      end
    end

    class << self
      attr_writer :max_depth, :max_expansion_length

      def max_depth
        @max_depth ||= DEFAULT_MAX_DEPTH
      end

      def max_expansion_length
        @max_expansion_length ||= DEFAULT_MAX_EXPANSION_LENGTH
      end

      # Expands all references in +string+. +reference+ identifies the
      # translation being expanded (e.g. "en.promo") and +state+ carries
      # the shared expansion state across nested references.
      def expand(string, locale, reference, state)
        state ||= State.new
        state.enter(reference)
        state.depth += 1
        raise FragmentReferenceDepthExceeded.new(state.chain, max_depth) if state.depth > max_depth

        begin
          expanded = string.gsub(REFERENCE_PATTERN) do
            expand_reference(Regexp.last_match(1), parse_params(Regexp.last_match(2)), locale, state)
          end
          state.consume(expanded.length)
          expanded
        ensure
          state.depth -= 1
          state.leave
        end
      end

      private

      def expand_reference(key, params, locale, state)
        result = catch(:exception) do
          I18n.translate(key, **params.merge(:locale => locale, :throw => true, :fragment_state => state))
        end

        if result.is_a?(I18n::MissingTranslation)
          raise MissingFragmentReference.new(state.chain, result)
        end

        unless result.is_a?(::String)
          raise FragmentReferenceError.new(state.chain + [key],
            "fragment reference #{key.inspect} resolved to #{result.class}, expected a String")
        end

        result
      end

      def parse_params(source)
        return EMPTY_HASH if source.nil? || source.strip.empty?

        params = {}
        source.scan(PARAM_PATTERN) do |key, raw|
          params[key.to_sym] = parse_value(raw)
        end
        params
      end

      def parse_value(raw)
        value = raw.strip
        case value
        when /\A"(.*)"\z/m, /\A'(.*)'\z/m then Regexp.last_match(1)
        when /\A[+-]?\d+\z/ then value.to_i
        when /\A[+-]?\d+\.\d+\z/ then value.to_f
        when 'true' then true
        when 'false' then false
        else value
        end
      end
    end
  end
end
