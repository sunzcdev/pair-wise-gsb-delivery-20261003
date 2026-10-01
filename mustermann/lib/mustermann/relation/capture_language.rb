# frozen_string_literal: true

module Mustermann
  module Relation
    # Maps the *built-in* capture constraints understood by Mustermann
    # (+capture:+ option symbols/classes and built-in converters such as
    # +:integer+) to the exact regular expression source the compiler emits.
    #
    # These strings are kept in lockstep with Mustermann::AST::CONVERTERS.
    # Anything not listed here (in particular user supplied Regexp objects or
    # custom converters) leads to an "unknown" result rather than a guess.
    #
    # @api private
    module CaptureLanguage
      # Regular expression sources for the value converters.
      CONVERTER_SOURCES = {
        'Integer'       => '-?\\d+',
        'Symbol'        => '\\w+',
        'String'        => nil,
        'Float'         => '-?\\d+(?:\\.\\d+)?',
        'Date'          => '\\d{4}-\\d{2}-\\d{2}',
        'Gem::Version'  => Gem::Version::VERSION_PATTERN,
        locale:           '(?:[A-Za-z]{2,3}|i)(?:-[A-Za-z0-9]{1,8})*',
        slug:             '[a-z0-9]+(?:-[a-z0-9]+)*',
        uuid:             '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}',
      }.freeze

      CLASS_SOURCES = {
        'Integer'      => CONVERTER_SOURCES['Integer'],
        'Symbol'       => CONVERTER_SOURCES['Symbol'],
        'String'       => CONVERTER_SOURCES['String'],
        'Float'        => CONVERTER_SOURCES['Float'],
        'Date'         => CONVERTER_SOURCES['Date'],
        'Gem::Version' => CONVERTER_SOURCES['Gem::Version'],
      }.freeze

      SYMBOL_ALIASES = {
        integer: 'Integer',
        symbol:  'Symbol',
        string:  'String',
        float:   'Float',
        date:    'Date',
        version: 'Gem::Version',
        locale:  :locale,
        slug:    :slug,
        uuid:    :uuid,
      }.freeze

      # Source for a POSIX character class symbol such as +:word+, or nil if
      # the symbol names a converter instead.
      POSIX_SOURCES = {
        word:    '[[:word:]]',
        alpha:   '[[:alpha:]]',
        alnum:   '[[:alnum:]]',
        digit:   '[[:digit:]]',
        space:   '[[:space:]]',
        lower:   '[[:lower:]]',
        upper:   '[[:upper:]]',
        xdigit:  '[[:xdigit:]]',
      }.freeze

      # Resolves a single +capture:+ option value to a regex source.
      #
      # @return [String, nil] nil means "default capture body"
      # @raise [AnalysisError] for constraints that cannot be analyzed
      def self.source(value)
        case value
        when nil
          nil
        when String
          value
        when Symbol
          symbol_source(value)
        when Class
          class_source(value)
        when Regexp
          raise AnalysisError, "custom Regexp constraint cannot be analyzed"
        else
          raise AnalysisError, "unsupported capture constraint #{value.inspect}"
        end
      end

      def self.symbol_source(symbol)
        return POSIX_SOURCES.fetch(symbol) if POSIX_SOURCES.key?(symbol)
        target = SYMBOL_ALIASES.fetch(symbol) do
          raise AnalysisError, "unsupported capture constraint #{symbol.inspect}"
        end
        CONVERTER_SOURCES.fetch(target)
      end

      def self.class_source(klass)
        CLASS_SOURCES.fetch(klass.name) do
          raise AnalysisError, "unsupported capture converter #{klass.name}"
        end
      end
    end
  end
end
