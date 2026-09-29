# frozen_string_literal: true
module JMESPath
  # @api private
  class Runtime
    # @api private
    DEFAULT_PARSER = CachingParser

    # Constructs a new runtime object for evaluating JMESPath expressions.
    #
    #     runtime = JMESPath::Runtime.new
    #     runtime.search(expression, data)
    #     #=> ...
    #
    # ## Caching
    #
    # When constructing a {Runtime}, the default parser caches expressions.
    # This significantly speeds up calls to {#search} multiple times
    # with the same expression but different data. To disable caching, pass
    # `:cache_expressions => false` to the constructor or pass a custom
    # `:parser`.
    #
    # @example Re-use a Runtime, caching enabled by default
    #
    #   runtime = JMESPath::Runtime.new
    #   runtime.parser
    #   #=> #<JMESPath::CachingParser ...>
    #
    # @example Disable caching
    #
    #   runtime = JMESPath::Runtime.new(cache_expressions: false)
    #   runtime.parser
    #   #=> #<JMESPath::Parser ...>
    #
    # @option options [Boolean] :cache_expressions (true) When `false`, a non
    #   caching parser will be used. When `true`, a shared instance of
    #   {CachingParser} is used.  Defaults to `true`.
    #
    # @option options [Boolean] :disable_visit_errors (false) When `true`,
    #   no errors will be raised during runtime processing. Parse errors
    #   will still be raised, but unexpected data sent to visit will
    #   result in nil being returned.
    #
    # @option options [Parser,CachingParser] :parser
    #
    def initialize(options = {})
      @parser = options[:parser] || default_parser(options)
    end

    # @return [Parser, CachingParser]
    attr_reader :parser

    # @param [String<JMESPath>] expression
    # @param [Hash] data
    # @return [Mixed,nil]
    def search(expression, data)
      optimized_expression = @parser.parse(expression).optimize
      optimized_expression.visit(data)
    end

    # Searches `data` using the diagnostic mode. Keys that are absent
    # from the data are tracked internally as {Missing} sentinels and
    # reported in the result as serializable hashes of the shape
    # `{ '$missing' => 'path.to.key' }`. Explicit `null` values in the
    # data continue to be returned as `nil`.
    #
    # @param [String<JMESPath>] expression
    # @param [Hash] data
    # @return [Mixed,nil] A result free of {Missing} sentinels.
    def diagnostic_search(expression, data)
      Diagnostic.activate do
        Diagnostic.sanitize(search(expression, data))
      end
    end

    private

    def default_parser(options)
      if options[:cache_expressions] == false
        Parser.new(options)
      else
        DEFAULT_PARSER.new(options)
      end
    end
  end
end
