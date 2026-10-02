# frozen_string_literal: true

module JMESPath
  require 'jmespath/query_set/ast'
  require 'jmespath/query_set/normalizer'
  require 'jmespath/query_set/planner'
  require 'jmespath/query_set/executor'

  # A named collection of JMESPath expressions compiled together so that
  # structurally shared AST prefixes are evaluated with a single traversal.
  #
  # Sharing is decided at compile time from the *AST*, never by slicing
  # expression strings. In particular:
  #
  # * a pipe boundary (`a | b`) is distinct from a dotted boundary (`a.b`);
  # * an expression reference (`&x`) is an opaque value and is only shared
  #   with an identical reference, never fused into an ordinary prefix;
  # * filter projections carry their predicate, so `[?x==`1`]` and
  #   `[?x==`2`]` do not share the filter step, although they share every
  #   preceding step and the source array is still iterated only once.
  #
  # @example extract several fields that filter one big array
  #
  #   qs = JMESPath::QuerySet.new(
  #     adults:  "people[?age > `18`].name",
  #     emails:  "people[?age > `18`].email",
  #     seniors: "people[?age > `65`].name",
  #   )
  #   result = qs.search(data)
  #   result[:adults]   #=> ...
  #   result.stats.fused.array_iterations   #=> 1 (one traversal of `people`)
  #
  class QuerySet
    # Outcome of {QuerySet#search}. Behaves as a plain mapping of name =>
    # value and also carries {#errors} and access {#stats}.
    class Result
      include Enumerable

      def initialize(values, errors, stats, names_in_order)
        @values = values
        @errors = errors
        @stats = stats
        @names_in_order = names_in_order
      end

      # @return [Object, nil] the value extracted for `name`
      def [](name)
        @values[name]
      end

      def fetch(name, *args, &block)
        @values.fetch(name, *args, &block)
      end

      def key?(name)
        @values.key?(name)
      end
      alias has_key? key?

      def keys
        @names_in_order.select { |n| @values.key?(n) }
      end

      def each
        return enum_for(:each) unless block_given?

        @names_in_order.each { |n| yield(n, @values[n]) if @values.key?(n) }
        self
      end

      def to_h
        @values.dup
      end

      # @return [Hash{Symbol => Exception}] query names that failed
      #   (populated for `:collect` and `:nil` policies)
      attr_reader :errors

      # @return [QuerySet::Stats] visit/iteration counters
      attr_reader :stats

      def successful?(name)
        !@errors.key?(name)
      end
    end

    # Access counters comparing the fused run with separate searches.
    class Stats
      def initialize(fused:, separate:)
        @fused = fused
        @separate = separate
      end

      # @return [Executor::Stats]
      attr_reader :fused, :separate

      def to_h
        {
          fused: {
            visits: @fused.visits,
            array_iterations: @fused.array_iterations,
            object_iterations: @fused.object_iterations
          },
          separate: {
            visits: @separate.visits,
            array_iterations: @separate.array_iterations,
            object_iterations: @separate.object_iterations
          }
        }
      end

      def visits_saved
        @separate.visits - @fused.visits
      end

      def iterations_saved
        @separate.total_iterations - @fused.total_iterations
      end
    end

    VALID_ON_ERROR = [:raise, :collect, :nil].freeze

    # @param queries [Hash{Symbol => String}] ordered name => expression map
    # @option options [Symbol] :on_error (:raise) one of:
    #
    #   * `:raise`   - any error fails the whole search (the error is raised)
    #   * `:collect` - failing names are omitted from the result and recorded
    #                  in `Result#errors`; other queries keep their values
    #   * `:nil`     - like `:collect` but failing names are present with a
    #                  `nil` value
    #
    # @option options [Boolean] :cache_expressions (true)
    # @option options [Parser] :parser
    # @option options [Boolean] :disable_visit_errors
    OPTION_KEYS = [:on_error, :cache_expressions, :parser, :disable_visit_errors].freeze

    def initialize(queries = nil, on_error: :raise, **options, &block)
      query_hash = coerce_queries(queries, options)
      raise ArgumentError, 'expected a block or a name => expression hash' if query_hash.empty? && !block
      unless VALID_ON_ERROR.include?(on_error)
        raise ArgumentError, "invalid on_error #{on_error.inspect}, expected one of #{VALID_ON_ERROR.map(&:inspect).join(', ')}"
      end

      @on_error = on_error
      parser_option = options[:parser]
      @parser = parser_option || build_parser(options)
      @queries = {}
      @names_in_order = []
      @errors = {}

      add_queries(query_hash)
      block.call(self) if block

      compile
    end

    # Accepts both an explicit hash and inline keyword names
    # (e.g. QuerySet.new(adults: "...", on_error: :collect)).
    def coerce_queries(queries, options)
      hash = {}
      hash.merge!(queries) if queries.is_a?(Hash)
      options.each_pair do |key, value|
        hash[key] = value unless OPTION_KEYS.include?(key)
      end
      hash
    end

    # Register another named expression (only available before the first
    # search when using the block form).
    def add(name, expression)
      name = name.to_sym
      @names_in_order.delete(name)
      @names_in_order << name
      begin
        @queries[name] = @parser.parse(expression)
        @errors.delete(name)
      rescue Errors::Error => e
        # In :raise mode compile problems fail construction immediately.
        # For :collect/:nil they are deferred to (and reported by) #search.
        raise e if @on_error == :raise

        @errors[name] = e
      end
      self
    end
    alias register add

    # @return [Array<Symbol>]
    attr_reader :names

    # Evaluate every registered expression against `data` in one fused pass.
    #
    # With `on_error: :collect` or `:nil` a block may be given and is called
    # as `|name, error|` for every failed query.
    #
    # @return [Result] the fused result (behaves as a name => value map and
    #   also exposes {Result#errors} and {Result#stats})
    def search(data, &error_block)
      run(data, &error_block)
    end

    private

    def add_queries(queries)
      queries.each { |name, expression| add(name, expression) }
    end

    def build_parser(options)
      parser_options = {}
      parser_options[:disable_visit_errors] = options[:disable_visit_errors] unless options[:disable_visit_errors].nil?
      if options[:cache_expressions] == false
        Parser.new(parser_options)
      else
        CachingParser.new(parser_options)
      end
    end

    def compile
      @names = @names_in_order.dup

      # Solo plans provide the same-ruler baseline (separate per-query runs)
      # and are also used to evaluate deferred parse failures.
      @solo_plans = {}
      @names.each do |name|
        next unless @queries[name]

        planner = Planner.new
        entry = Planner::Entry.new(name, [name], Normalizer.to_steps(@queries[name]))
        @solo_plans[name] = planner.compile([entry])
      end

      # Fused plan over every expression that parsed successfully.
      planner = Planner.new
      entries = @names.map do |name|
        next nil unless @queries[name]

        Planner::Entry.new(name, [name], Normalizer.to_steps(@queries[name]))
      end.compact
      @plan = planner.compile(entries)
    end

    def run(data, &error_block)
      # Pre-existing compile errors are surfaced through the same policy.
      compile_errors = @errors.dup

      if @on_error == :raise && compile_errors.any?
        raise compile_errors.values.first
      end

      executor = Executor.new(on_error: @on_error)
      raw_values = executor.run(@plan, data)
      errors = executor.errors.merge(compile_errors)

      # Baseline: the same expressions evaluated independently, counted
      # with the identical instrumented interpreter.
      separate = Executor::Stats.new(0, 0, 0)
      @names.each do |name|
        next unless @queries[name]

        solo = Executor.new(on_error: :collect)
        solo.run(@solo_plans[name], data)
        separate.add!(solo.stats)
      end

      finalize(raw_values, errors, executor.stats, separate, &error_block)
    end

    def finalize(raw_values, errors, fused_stats, separate_stats, &error_block)
      values = {}
      @names.each do |name|
        if errors.key?(name)
          values[name] = nil if @on_error == :nil
        else
          values[name] = raw_values[name]
        end
      end

      errors.each_key { |name| error_block&.call(name, errors[name]) }

      stats = Stats.new(fused: fused_stats, separate: separate_stats)
      Result.new(values, errors, stats, @names)
    end
  end
end
