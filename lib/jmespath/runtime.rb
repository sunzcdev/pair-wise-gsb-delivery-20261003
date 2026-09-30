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

    # Statically validate `expression` against a {Shape} and infer the output
    # shape. Does not execute the query. See JMESPath.check.
    #
    # @return [StaticTypeChecker::Result]
    def check(expression, input_shape)
      StaticTypeChecker.new(input_shape, parser: plain_parser).check(expression)
    end

    # @raise [Errors::StaticTypeError]
    # @return [StaticTypeChecker::Result]
    def check!(expression, input_shape)
      result = check(expression, input_shape)
      raise Errors::StaticTypeError, result unless result.ok?
      result
    end

    private

    def default_parser(options)
      if options[:cache_expressions] == false
        Parser.new(options)
      else
        DEFAULT_PARSER.new(options)
      end
    end

    # Static analysis must walk the raw AST; the caching parser would hand
    # back the same tree, which is fine, but a dedicated plain parser keeps
    # checker state isolated from search caching.
    def plain_parser
      @plain_parser ||= Parser.new
    end
  end
end
