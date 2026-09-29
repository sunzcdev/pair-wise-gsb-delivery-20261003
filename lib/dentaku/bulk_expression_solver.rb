require 'dentaku/dependency_resolver'
require 'dentaku/exceptions'
require 'dentaku/flat_hash'
require 'dentaku/parser'
require 'dentaku/tokenizer'

module Dentaku
  class BulkExpressionSolver
    class StrictEvaluator
      def initialize(calculator)
        @calculator = calculator
      end

      def evaluate(*args, &block)
        @calculator.evaluate!(*args, &block)
      end
    end

    class PermissiveEvaluator
      def initialize(calculator, block)
        @calculator = calculator
        @block = block || ->(*) { :undefined }
      end

      def evaluate(*args, &block)
        handler = block || ->(_expr, ex) { @block.call(ex) }
        @calculator.evaluate(*args, &handler)
      end
    end

    def initialize(expressions, calculator)
      @expression_hash = FlatHash.from_hash(expressions)
      @calculator = calculator
    end

    def solve!
      @evaluator = StrictEvaluator.new(calculator)
      solve(&raise_exception_handler)
    end

    def solve(&block)
      @evaluator ||= PermissiveEvaluator.new(calculator, block)
      error_handler = block || return_undefined_handler
      results = load_results(&error_handler)

      FlatHash.expand(
        expression_hash.each_with_object({}) do |(k, v), r|
          default = v.nil? ? v : :undefined
          r[k] = results.fetch(k.to_s, default)
        end
      )
    end

    def dependencies
      Hash[expression_deps].tap do |d|
        d.values.each do |deps|
          unresolved = deps.reject { |ud| d.has_key?(ud) }
          unresolved.each { |u| add_dependencies(d, u) }
        end
      end
    end

    private

    def self.dependency_cache
      @dep_cache ||= {}
    end

    attr_reader :expression_hash, :calculator, :evaluator

    def return_undefined_handler
      ->(*) { :undefined }
    end

    def raise_exception_handler
      ->(ex) { raise ex }
    end

    def expression_with_exception_handler(var_name, &block)
      ->(_expr, ex) {
        ex.assigned_to = var_name
        block.call(ex)
      }
    end

    def load_results(&block)
      facts, _formulas = expressions.transform_keys { |k| normalized_name(k) }
                                    .transform_values { |v| calculator.ast(v) }
                                    .partition { |_, v| calculator.dependencies(v, nil).empty? }

      evaluated_facts = facts.to_h.each_with_object({}) do |(var_name, ast), h|
        with_rescues(var_name, h, block) do
          h[var_name] = ast.is_a?(Array) ? ast.map(&:value) : ast.value
        end
      end

      context = calculator.memory.merge(evaluated_facts)

      results = variables_in_resolve_order.each_with_object({}) do |var_name, results|
        next if expressions[var_name].nil?

        with_rescues(var_name, results, block) do
          results[var_name] = evaluated_facts[normalized_name(var_name)] || evaluator.evaluate(
            expressions[var_name],
            context.merge(results),
            &expression_with_exception_handler(var_name, &block)
          ).tap { |res|
            res.assigned_to = var_name if res.respond_to?(:assigned_to=)
            res
          }
        end
      end

      report_cycle_errors(results, &block)

      results
    end

    # every expression that is part of a cycle, or transitively depends on
    # one, gets its own CyclicDependencyError carrying the full diagnostic
    # picture; unaffected expressions have already been solved above
    def report_cycle_errors(results, &block)
      return unless cycle_error

      (cycle_error.cycles.flatten + cycle_error.blocked).each do |var_name|
        next unless expressions.has_key?(var_name)

        ex = CyclicDependencyError.new(cycle_error.cycles, cycle_error.blocked)
        ex.assigned_to = var_name
        results[var_name] = block.call(ex)

        stored_name = normalized_name(var_name)
        if results[var_name] == :undefined && calculator.memory.has_key?(stored_name)
          results[var_name] = calculator.memory[stored_name]
        end
      end
    end

    def with_rescues(var_name, results, block)
      yield
    rescue Dentaku::UnboundVariableError, Dentaku::ZeroDivisionError, Dentaku::ArgumentError => ex
      ex.assigned_to = var_name
      results[var_name] = block.call(ex)
    ensure
      stored_name = normalized_name(var_name)
      if results[var_name] == :undefined && calculator.memory.has_key?(stored_name)
        results[var_name] = calculator.memory[stored_name]
      end
    end

    # names are normalized the same way the calculator stores them, so that
    # facts and memory lookups line up with the identifiers the parser emits
    def normalized_name(variable_name)
      calculator.standardize_case(variable_name.to_s)
    end

    def expressions
      @expressions ||= Hash[expression_hash.map { |k, v| [k.to_s, v] }]
    end

    def expression_deps
      expressions.map do |var, expr|
        [var, calculator.dependencies(expr)]
      end
    end

    def add_dependencies(current_dependencies, variable)
      node = calculator.memory[variable]
      if node.respond_to?(:dependencies)
        current_dependencies[variable] = node.dependencies
        node.dependencies.each { |d| add_dependencies(current_dependencies, d) }
      end
    end

    def variables_in_resolve_order
      @ordered_deps ||= begin
        deps = dependencies
        cycles = DependencyResolver.find_cycles(deps, calculator.case_sensitive)

        if cycles.any?
          blocked = blocked_variables(deps, cycles)
          @cycle_error = CyclicDependencyError.new(cycles, blocked & expressions.keys)
          affected = cycles.flatten + blocked
          deps = deps.reject { |k, _| affected.include?(k) }
        end

        cache_key = "#{!!calculator.case_sensitive}|#{expressions.keys.map(&:to_s).sort.join("|")}|#{affected ? affected.sort.join("|") : ""}"
        self.class.dependency_cache.fetch(cache_key) {
          DependencyResolver.find_resolve_order(deps, calculator.case_sensitive).tap do |d|
            self.class.dependency_cache[cache_key] = d if calculator.cache_dependency_order?
          end
        }
      end
    end

    # all variables that transitively depend on a cycle member, following
    # the dependency edges backwards from each cycle; names are normalized
    # the same way the calculator normalizes them so case and aliasing
    # rules match the rest of the solver
    def blocked_variables(deps, cycles)
      normalized_deps = Hash[deps.map { |k, v| [normalized_name(k), v.map { |d| normalized_name(d) }] }]
      original_names = Hash[deps.keys.map { |k| [normalized_name(k), k] }]

      dependents = Hash.new { |h, k| h[k] = [] }
      normalized_deps.each do |var, var_deps|
        var_deps.each { |dep| dependents[dep] << var }
      end

      seen = {}
      blocked = []
      queue = cycles.flatten.map { |name| normalized_name(name) }
      queue.each { |name| seen[name] = true }

      until queue.empty?
        dependents[queue.shift].each do |dependent|
          next if seen[dependent]

          seen[dependent] = true
          blocked << dependent
          queue << dependent
        end
      end

      blocked.map { |name| original_names.fetch(name, name) }.sort
    end

    attr_reader :cycle_error
  end
end
