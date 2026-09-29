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
    # @option options [Boolean] :materialize (true) When streaming (see
    #   {#stream}), expressions that cannot be evaluated incrementally
    #   (e.g. `sort`, negative indices) are evaluated by materializing
    #   the source into an array. When `false`, such expressions raise
    #   {Errors::StreamingNotSupportedError} before the source is
    #   consumed.
    #
    # @option options [Parser,CachingParser] :parser
    #
    def initialize(options = {})
      @parser = options[:parser] || default_parser(options)
      @materialize = options.fetch(:materialize, true)
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

    # Streams the results of a JMESPath expression over a read-only
    # array source, producing results as elements are read.
    #
    # The source must respond to `#each`, yielding array elements in
    # order. If the source responds to `#close`, it is closed exactly
    # once: when iteration completes, when the consumer stops early, or
    # when an error is raised while evaluating the query.
    #
    # Only expressions that can be evaluated incrementally (filters,
    # projections, flatten, non-negative indices and slices, and chains
    # of these) stream element by element. Expressions that require the
    # full input up front (e.g. `sort`, negative indices) are evaluated
    # by materializing the source, unless the `:materialize` option is
    # `false`, in which case {Errors::StreamingNotSupportedError} is
    # raised before the source is consumed.
    #
    # @param [String<JMESPath>] expression
    # @param [#each] source A read-only, streaming array source.
    # @return [StreamingResult] A lazy, forward-only enumerable. Each
    #   element of the result array is yielded in order; expressions
    #   with a non-array result yield that value as a single element.
    def stream(expression, source)
      unless source.respond_to?(:each)
        raise Errors::InvalidTypeError, 'streaming sources must respond to #each'
      end
      optimized_expression = @parser.parse(expression).optimize
      if optimized_expression.streamable?
        StreamingResult.new(source) do |emit|
          optimized_expression.stream_each(elements_of(source)).each do |value|
            emit.call(value)
          end
        end
      elsif @materialize
        StreamingResult.new(source) do |emit|
          emit_result(optimized_expression.visit(materialize(source)), emit)
        end
      else
        raise Errors::StreamingNotSupportedError,
              'expression requires materializing the source, but ' \
              'materialization is disabled'
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

    def elements_of(source)
      Enumerator.new do |yielder|
        source.each { |element| yielder << element }
      end
    end

    def materialize(source)
      elements = []
      source.each { |element| elements << element }
      elements
    end

    def emit_result(result, emit)
      if result.respond_to?(:to_ary)
        result.to_ary.each { |value| emit.call(value) }
      else
        emit.call(result)
      end
    end
  end
end
