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

    # Searches data in diagnostic mode. Unlike {#search}, a missing
    # key/index is tracked internally with a sentinel and reported
    # separately from an explicit `null` value.
    #
    # @param [String<JMESPath>] expression
    # @param [Hash] data
    # @return [Hash] A JSON serializable report with two keys:
    #
    #   * `"value"` - the search result, with missing values as `nil`.
    #   * `"diagnostics"` - an array of entries, each with a `"path"`,
    #     a `"reason"` (`"missing_key"`, `"index_out_of_bounds"`,
    #     `"not_traversable"` or `"function_argument"`) and an optional
    #     `"detail"`.
    def diagnose(expression, data)
      optimized_expression = @parser.parse(expression).optimize
      result, context = DiagnosticContext.activate do
        optimized_expression.visit(data)
      end
      {
        'value' => DiagnosticContext.sanitize(result),
        'diagnostics' => context.entries
      }
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
