# frozen_string_literal: true
module JMESPath
  # A named set of JMESPath expressions that is compiled into a single
  # shared execution plan and evaluated in one traversal of the data.
  #
  #   set = JMESPath::QuerySet.new
  #   set.add(:names,   'people[*].name')
  #   set.add(:adults,  'people[?age > `18`].name')
  #   set.add(:first,   'people[0].name')
  #
  #   result = set.search(data)
  #   result[:names] #=> [...]
  #
  # ## Shared prefixes
  #
  # At compile time each expression is parsed and optimized into a chain
  # of evaluation steps. Steps are compared by a *semantic* fingerprint
  # (node type + structure + parameters), never by source string, so:
  #
  # * `a.b | c` and `a.b.c` compile to the same step sequence and share
  #   evaluation, while `foo[*].bar | [0]` and `foo[*].bar[0]` compile to
  #   different steps (a pipe stops the projection) and do not.
  # * Filter conditions and expression references (`&expr`) are part of
  #   the fingerprint: `items[?a == `1`]` and `items[?a == `2`]` never
  #   share a step, and `sort_by(@, &a)` never merges with
  #   `sort_by(@, &b)`.
  #
  # Chains with a common step prefix are evaluated once and fanned out.
  # Additionally, sibling projections over the *same* target (e.g.
  # several `items[?...]` filters with different conditions) are fused:
  # the target is resolved once and the array is scanned a single time,
  # applying every projection to each element as it is visited.
  #
  # ## Errors
  #
  # `search` accepts `:on_error`:
  #
  # * `:fail` (default) - the first error (parse or evaluation) raises
  #   and the whole group fails, mirroring {JMESPath.search}.
  # * `:partial` - failing queries are reported in `Result#errors` while
  #   the other queries still return their values. An error on a shared
  #   prefix fails every query that depends on it, and no other query.
  #
  # ## Counting
  #
  # Pass `count: true` to `search` and inspect `Result#stats` to obtain
  # `:step_visits` (chain steps evaluated) and `:element_visits`
  # (projection target elements scanned). Comparing these numbers with
  # the sum of running each query on its own demonstrates the effect of
  # merging.
  class QuerySet
    # Computes structural fingerprints for AST nodes. Two nodes with the
    # same fingerprint are guaranteed to be the same kind of node with
    # recursively equal parameters, so they evaluate identically.
    # Source text is never consulted.
    module Fingerprint
      module_function

      # @param [Nodes::Node] node
      # @return [Array] a hashable, structurally comparable fingerprint
      def node(node)
        parts = node.instance_variables.sort_by(&:to_s).map do |ivar|
          value(node.instance_variable_get(ivar))
        end
        [node.class.name, *parts]
      end

      def value(value)
        case value
        when Nodes::Node then node(value)
        when Array then value.map { |v| value(v) }
        when Hash
          value.map { |k, v| [value(k), value(v)] }.sort_by(&:inspect)
        else value
        end
      end
    end

    # One node of the compiled plan trie. Shared prefixes are shared
    # nodes; `names` holds the queries whose result is the value
    # produced at this point of the plan.
    class TrieNode
      attr_reader :step, :children, :names

      def initialize(step = nil)
        @step = step
        @children = {}
        @names = []
      end

      def each_name(&block)
        @names.each(&block)
        @children.each_value { |child| child.each_name(&block) }
      end
    end

    # Access counts collected during a single {#search} with
    # `count: true`.
    class Stats
      # @return [Integer] number of chain steps evaluated. A step shared
      #   by N queries is counted once instead of N times.
      attr_reader :step_visits

      # @return [Integer] number of projection target elements scanned.
      #   Fused projections scan the target array a single time no
      #   matter how many queries filter it.
      attr_reader :element_visits

      def initialize
        @step_visits = 0
        @element_visits = 0
      end

      # @api private
      def step!
        @step_visits += 1
      end

      # @api private
      def elements!(count)
        @element_visits += count
      end

      def to_h
        { step_visits: @step_visits, element_visits: @element_visits }
      end
    end

    # The outcome of a {QuerySet#search} call.
    class Result
      # @return [Hash<Symbol,Object>] values of the queries that
      #   evaluated successfully.
      attr_reader :values

      # @return [Hash<Symbol,Exception>] errors per failed query. Always
      #   empty unless the search ran with `on_error: :partial`.
      attr_reader :errors

      # @return [Stats,nil] access counts when the search ran with
      #   `count: true`.
      attr_reader :stats

      # @api private
      def initialize(values, errors, stats)
        @values = values
        @errors = errors
        @stats = stats
      end

      # @param [Symbol] name
      # @return [Object] the value of a successful query.
      def [](name)
        @values[name]
      end

      # @param [Symbol] name
      # @return [Object] the value of a successful query.
      # @raise [KeyError] if the query is unknown or failed.
      def fetch(name)
        @values.fetch(name)
      end

      # @return [Boolean] true when every query produced a value.
      def success?
        @errors.empty?
      end

      # @return [Hash<Symbol,Object>] the successful values.
      def to_h
        @values.dup
      end
    end

    # @option options [Boolean] :cache_expressions (true)
    # @option options [Boolean] :disable_visit_errors (false)
    # @option options [Parser,CachingParser] :parser
    def initialize(options = {})
      @parser = options[:parser] || CachingParser.new(options)
      @expressions = {}
      @plan = nil
      @parse_errors = {}
    end

    # Adds (or replaces) a named query. Invalidates the compiled plan;
    # the set is recompiled lazily on the next {#search}.
    #
    # @param [Symbol,String] name
    # @param [String] expression a JMESPath expression
    # @return [QuerySet] self, so calls can be chained
    def add(name, expression)
      @expressions[name.to_sym] = expression
      @plan = nil
      self
    end

    # @param [Symbol,String] name
    # @return [String,nil] the expression registered under `name`
    def [](name)
      @expressions[name.to_sym]
    end

    # @return [Array<Symbol>] the registered query names
    def names
      @expressions.keys
    end

    # Compiles every expression and builds the shared plan. Called
    # lazily by {#search}; call it explicitly to surface parse errors
    # before searching.
    #
    # With `on_error: :partial`, expressions that fail to parse are
    # collected instead of raised and reported on every search.
    #
    # @return [QuerySet] self
    def compile(on_error: :fail)
      @plan = TrieNode.new
      @parse_errors = {}
      @expressions.each do |name, expression|
        begin
          ast = @parser.parse(expression).optimize
        rescue StandardError => e
          if on_error == :fail
            @plan = nil # leave the set uncompiled so a later call retries
            raise
          end
          @parse_errors[name] = e
          next
        end
        insert(@plan, steps(ast), name)
      end
      self
    end

    # @return [Array] a nested, inspectable description of the compiled
    #   plan showing which steps are shared by which queries.
    def plan
      compile unless @plan
      describe(@plan)
    end

    # Evaluates every query against `data` in a single shared traversal.
    #
    # @param [Hash] data
    # @param [Symbol] on_error `:fail` (default) raises on the first
    #   error; `:partial` keeps the results of the unaffected queries.
    # @param [Boolean] count when true, `result.stats` contains access
    #   counts proving how much traversal was shared.
    # @return [Result]
    def search(data, on_error: :fail, count: false)
      compile(on_error: on_error) unless @plan
      unless @parse_errors.empty?
        @parse_errors.each_value { |e| raise e } if on_error == :fail
      end
      stats = count ? Stats.new : nil
      values = {}
      errors = @parse_errors.dup
      walk(@plan, data, values, errors, stats, on_error)
      Result.new(values, errors, stats)
    end

    private

    # Decomposes an optimized AST into the sequence of steps evaluated
    # by Chain#visit. ChainedField is expanded into its individual keys
    # so that field prefixes (a.b.c vs a.b.d) become shareable.
    def steps(ast)
      chain = ast.is_a?(Nodes::Chain) ? ast.children : [ast]
      chain.flat_map do |step|
        if step.is_a?(Nodes::ChainedField)
          step.keys.map { |key| Nodes::Field.new(key) }
        else
          [step]
        end
      end
    end

    def insert(root, steps, name)
      node = steps.reduce(root) do |current, step|
        fingerprint = Fingerprint.node(step)
        current.children[fingerprint] ||= TrieNode.new(step)
      end
      node.names << name
    end

    # -- evaluation ---------------------------------------------------

    def walk(trie, value, values, errors, stats, on_error)
      trie.names.each { |name| values[name] = value }
      groups = trie.children.values.group_by { |child| fusion_key(child.step) }
      groups.each do |key, children|
        if key
          walk_projection(children, value, values, errors, stats, on_error)
        else
          children.each do |child|
            walk_step(child, value, values, errors, stats, on_error)
          end
        end
      end
    end

    def walk_step(trie, value, values, errors, stats, on_error)
      stats&.step!
      begin
        result = trie.step.visit(value)
      rescue StandardError => e
        return fail_subtree(trie, e, errors, on_error)
      end
      walk(trie, result, values, errors, stats, on_error)
    end

    # Evaluates one or more projection steps that share the same target
    # with a single target resolution and a single scan of the target
    # elements. With one child this is simply a projection step; with
    # several children it is the fused case.
    def walk_projection(children, value, values, errors, stats, on_error)
      step = children.first.step
      stats&.step!
      begin
        targets = step.extract_targets(step.target.visit(value))
      rescue StandardError => e
        children.each { |c| fail_subtree(c, e, errors, on_error) }
        return
      end
      if targets.nil?
        children.each { |c| walk(c, nil, values, errors, stats, on_error) }
        return
      end
      stats&.elements!(targets.size)
      lists = children.map { [] }
      active = children.each_with_index.to_a
      targets.each do |element|
        failed = nil
        active.each do |child, index|
          begin
            projected = child.step.projection.visit(element)
            lists[index] << projected unless projected.nil?
          rescue StandardError => e
            fail_subtree(child, e, errors, on_error)
            (failed ||= []) << child
          end
        end
        next unless failed
        active.delete_if { |child, _| failed.include?(child) }
        break if active.empty?
      end
      active.each do |child, index|
        walk(child, lists[index], values, errors, stats, on_error)
      end
    end

    # Projections over structurally equal targets are fusible. The key
    # distinguishes array projections from object projections (they
    # extract their targets differently) and includes the semantic
    # fingerprint of the target, so projections over *different* arrays
    # never fuse.
    def fusion_key(step)
      case step
      when Nodes::ArrayProjection
        [:array, Fingerprint.node(step.target)]
      when Nodes::ObjectProjection
        [:object, Fingerprint.node(step.target)]
      end
    end

    def fail_subtree(trie, error, errors, on_error)
      raise error if on_error == :fail
      trie.each_name { |name| errors[name] = error }
    end

    # -- plan inspection ----------------------------------------------

    def describe(trie)
      {
        step: describe_step(trie.step),
        names: trie.names,
        children: trie.children.values.map { |child| describe(child) }
      }
    end

    def describe_step(step)
      case step
      when nil then nil
      when Nodes::Field then "Field(#{step.keys.join('.')})"
      when Nodes::Projection
        "#{step.class.name.split('::').last}(#{describe_step(step.target)})"
      else
        step.class.name.split('::').last
      end
    end
  end
end
