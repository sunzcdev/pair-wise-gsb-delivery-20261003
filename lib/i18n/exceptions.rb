# frozen_string_literal: true

module I18n
  class ExceptionHandler
    def call(exception, _locale, _key, _options)
      if exception.is_a?(MissingTranslation)
        exception.message
      else
        raise exception
      end
    end
  end

  class ArgumentError < ::ArgumentError; end

  class Disabled < ArgumentError
    def initialize(method)
      super(<<~MESSAGE)
        I18n.#{method} is currently disabled, likely because your application is still in its loading phase.

        This method is meant to display text in the user locale, so calling it before the user locale has
        been set is likely to display text from the wrong locale to some users.

        If you have a legitimate reason to access i18n data outside of the user flow, you can do so by passing
        the desired locale explicitly with the `locale` argument, e.g. `I18n.#{method}(..., locale: :en)`
      MESSAGE
    end
  end

  class InvalidLocale < ArgumentError
    attr_reader :locale
    def initialize(locale)
      @locale = locale
      super "#{locale.inspect} is not a valid locale"
    end
  end

  class InvalidLocaleData < ArgumentError
    attr_reader :filename
    def initialize(filename, exception_message)
      @filename, @exception_message = filename, exception_message
      super "can not load translations from #{filename}: #{exception_message}"
    end
  end

  class MissingTranslation < ArgumentError
    module Base
      PERMITTED_KEYS = [:scope, :default].freeze

      attr_reader :locale, :key, :options

      def initialize(locale, key, options = EMPTY_HASH)
        @key, @locale, @options = key, locale, options.slice(*PERMITTED_KEYS)
        options.each { |k, v| self.options[k] = v.inspect if v.is_a?(Proc) }
      end

      def keys
        @keys ||= I18n.normalize_keys(locale, key, options[:scope]).tap do |keys|
          keys << 'no key' if keys.size < 2
        end
      end

      def message
        if (default = options[:default]).is_a?(Array) && default.any?
          other_options = ([key, *default]).map { |k| normalized_option(k).prepend('- ') }.join("\n")
          "Translation missing. Options considered were:\n#{other_options}"
        else
          "Translation missing: #{keys.join('.')}"
        end
      end

      def normalized_option(key)
        I18n.normalize_keys(locale, key, options[:scope]).join('.')
      end

      alias :to_s :message

      def to_exception
        MissingTranslationData.new(locale, key, options)
      end
    end

    include Base
  end

  class MissingTranslationData < ArgumentError
    include MissingTranslation::Base
  end

  class InvalidPluralizationData < ArgumentError
    attr_reader :entry, :count, :key
    def initialize(entry, count, key)
      @entry, @count, @key = entry, count, key
      super "translation data #{entry.inspect} can not be used with :count => #{count}. key '#{key}' is missing."
    end
  end

  class MissingInterpolationArgument < ArgumentError
    attr_reader :key, :values, :string
    def initialize(key, values, string)
      @key, @values, @string = key, values, string
      super "missing interpolation argument #{key.inspect} in #{string.inspect} (#{values.inspect} given)"
    end
  end

  # Raised when a fragment reference (%{ref:key}) points to a key that can
  # not be resolved. The message includes the whole reference chain and the
  # underlying reason so the error is not reduced to a bare
  # "translation missing".
  class MissingReference < ArgumentError
    attr_reader :key, :chain
    def initialize(key, chain, reason = nil)
      @key, @chain = key, chain
      message = "missing reference #{key.inspect} in reference chain #{chain.join(' -> ')}"
      message << ": #{reason}" if reason
      super message
    end
  end

  # Raised when fragment references form a cycle. The reported chain lists
  # every key that was being expanded, ending with the key that closes the
  # loop.
  class CircularReference < ArgumentError
    attr_reader :chain
    def initialize(chain)
      @chain = chain
      super "circular reference detected: #{chain.join(' -> ')}"
    end
  end

  # Raised when reference expansion is nested deeper than
  # I18n::Backend::References.max_depth.
  class ReferenceDepthExceeded < ArgumentError
    attr_reader :chain, :max_depth
    def initialize(chain, max_depth)
      @chain, @max_depth = chain, max_depth
      super "reference chain #{chain.join(' -> ')} exceeds the maximum reference depth of #{max_depth}"
    end
  end

  # Raised when an expanded translation grows beyond
  # I18n::Backend::References.max_length characters.
  class ReferenceExpansionTooLong < ArgumentError
    attr_reader :chain, :max_length
    def initialize(chain, max_length)
      @chain, @max_length = chain, max_length
      super "reference expansion at #{chain.join(' -> ')} exceeds the maximum expansion length of #{max_length} characters"
    end
  end

  # Raised when a fragment reference is malformed or resolves to a value
  # that can not be embedded into a string (e.g. a subtree).
  class InvalidReference < ArgumentError
    attr_reader :chain
    def initialize(message, chain)
      @chain = chain
      super "#{message} (reference chain #{chain.join(' -> ')})"
    end
  end

  class ReservedInterpolationKey < ArgumentError
    attr_reader :key, :string
    def initialize(key, string)
      @key, @string = key, string
      super "reserved key #{key.inspect} used in #{string.inspect}"
    end
  end

  class UnknownFileType < ArgumentError
    attr_reader :type, :filename
    def initialize(type, filename)
      @type, @filename = type, filename
      super "can not load translations from #{filename}, the file type #{type} is not known"
    end
  end

  class UnsupportedMethod < ArgumentError
    attr_reader :method, :backend_klass, :msg
    def initialize(method, backend_klass, msg)
      @method = method
      @backend_klass = backend_klass
      @msg = msg
      super "#{backend_klass} does not support the ##{method} method. #{msg}"
    end
  end

  class InvalidFilenames < ArgumentError
    NUMBER_OF_ERRORS_SHOWN = 20
    def initialize(file_errors)
      super <<~MSG
        Found #{file_errors.count} error(s).
        The first #{[file_errors.count, NUMBER_OF_ERRORS_SHOWN].min} error(s):
        #{file_errors.map(&:message).first(NUMBER_OF_ERRORS_SHOWN).join("\n")}

        To use the LazyLoadable backend:
        1. Filenames must start with the locale.
        2. An underscore must separate the locale with any optional text that follows.
        3. The file must only contain translation data for the single locale.

        Example:
        "/config/locales/fr.yml" which contains:
        ```yml
          fr:
            dog:
              chien
        ```
      MSG
    end
  end
end
