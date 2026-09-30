require 'dentaku/bulk_expression_solver'
require 'dentaku/compilation'
require 'dentaku/dependency_resolver'
require 'dentaku/exceptions'
require 'dentaku/flat_hash'
require 'dentaku/parser'
require 'dentaku/string_casing'
require 'dentaku/token'

module Dentaku
  class Calculator
    include StringCasing

    FUNCTION_RESOLUTION_MODES = [:pinned, :latest].freeze

    attr_reader :result, :memory, :tokenizer, :case_sensitive,
                :nested_data_support, :ast_cache, :raw_date_literals,
                :function_resolution

    def initialize(case_sensitive: false, aliases: nil, nested_data_support: true,
                   raw_date_literals: true, ast_cache: {},
                   cache_ast: nil, cache_dependency_order: nil,
                   function_resolution: :pinned)
      clear
      @tokenizer = Tokenizer.new
      @case_sensitive = case_sensitive
      @aliases = aliases
      @nested_data_support = nested_data_support
      @raw_date_literals = raw_date_literals
      @function_resolution = normalize_function_resolution(function_resolution)
      @ast_cache = ast_cache
      @cache_ast = cache_ast
      @cache_dependency_order = cache_dependency_order
      @disable_ast_cache = false
      @function_registry = Dentaku::AST::FunctionRegistry.new
    end

    # explicitly configured aliases win; otherwise the module-level default is
    # resolved lazily so it can be set after this calculator was created
    def aliases
      @aliases || Dentaku.aliases
    end

    def self.add_function(name, type, body, callback = nil, volatile: false)
      Dentaku::AST::FunctionRegistry.default.register(name, type, body, callback, volatile: volatile)
    end

    def self.add_functions(functions)
      functions.each { |(name, type, body, callback, volatile)| add_function(name, type, body, callback, volatile: !!volatile) }
    end

    def add_function(name, type, body, callback = nil, volatile: false)
      @function_registry.register(name, type, body, callback, volatile: volatile)
      self
    end

    def add_functions(functions)
      functions.each { |(name, type, body, callback, volatile)| add_function(name, type, body, callback, volatile: !!volatile) }
      self
    end

    def disable_cache
      @disable_ast_cache = true
      yield(self) if block_given?
    ensure
      @disable_ast_cache = false
    end

    def evaluate(expression, data = {}, &block)
      context = evaluation_context(data, :permissive)
      return evaluate_array(expression, context, &block) if expression.is_a?(Array)

      evaluate!(expression, context)
    rescue Dentaku::Error => ex
      block.call(expression, ex) if block_given?
    end

    private def evaluate_array(expression, data = {}, &block)
      expression.map { |e| evaluate(e, data, &block) }
    end

    def evaluate!(expression, data = {}, &block)
      context = evaluation_context(data, :strict)
      return evaluate_array!(expression, context, &block) if expression.is_a? Array

      store(context) do
        node = ast(expression)
        unbound = node.dependencies(memory)

        unless unbound.empty?
          raise UnboundVariableError.new(unbound),
                "no value provided for variables: #{unbound.uniq.join(', ')}"
        end

        node.value(memory)
      end
    end

    private def evaluate_array!(expression, data = {}, &block)
      expression.map { |e| evaluate!(e, data, &block) }
    end

    def solve!(expression_hash)
      BulkExpressionSolver.new(expression_hash, self).solve!
    end

    def solve(expression_hash, &block)
      BulkExpressionSolver.new(expression_hash, self).solve(&block)
    end

    def dependencies(expression, context = {})
      # dup inside the block: `store` now restores in place, so the caller
      # needs its own copy of the merged context rather than the live memory
      test_context = context.nil? ? {} : store(context) { memory.dup }

      case expression
      when Dentaku::AST::Node
        expression.dependencies(test_context)
      when Array
        expression.flat_map { |e| dependencies(e, context) }
      else
        ast(expression).dependencies(test_context)
      end
    end

    # every identifier the expression could reference, regardless of
    # branching: purely syntactic, ignores stored memory, and never
    # evaluates guards or functions
    def identifiers(expression)
      case expression
      when Dentaku::AST::Node
        expression.dependencies(AST::Node::STATIC_CONTEXT).uniq
      when Array
        expression.flat_map { |e| identifiers(e) }.uniq
      else
        ast(expression).dependencies(AST::Node::STATIC_CONTEXT).uniq
      end
    end

    def ast(expression)
      return expression if expression.is_a?(AST::Node)
      return expression.node if expression.is_a?(Compilation)
      return expression.map { |e| ast(e) } if expression.is_a? Array

      cached_ast(expression) || compile(expression).node
    end

    # Compile +expression+ against the current function registry and return a
    # Dentaku::Compilation. The +functions+ option selects which definitions
    # the resulting AST calls when a custom function is re-registered later:
    # :pinned keeps the original definitions, :latest re-validates against
    # (and calls) the newest ones. Defaults to the calculator's
    # +function_resolution+.
    def compile(expression, functions: nil)
      mode = normalize_function_resolution(functions || @function_resolution)
      Compilation.new(self, expression, parse_expression(expression), mode).tap do |compilation|
        cache_compilation(expression, compilation) if cache_ast?
      end
    end

    # Generation of the function registries this calculator compiles against:
    # [instance, global]. Bumped on every (re-)registration, including an
    # instance function shadowing a global one, so a Dentaku::Compilation can
    # tell whether the registry has moved since it was built.
    def registry_generation
      [
        @function_registry.generation,
        AST::FunctionRegistry.default.generation
      ]
    end

    def load_cache(ast_cache)
      @ast_cache = ast_cache
    end

    def clear_cache(pattern = :all)
      case pattern
      when :all
        @ast_cache = {}
      when String
        @ast_cache.delete(pattern)
      when Regexp
        @ast_cache.delete_if { |k, _| k =~ pattern }
      else
        raise ::ArgumentError
      end
    end

    def evaluation_context(data, evaluation_mode)
      data.key?(:__evaluation_mode) ? data : data.merge(__evaluation_mode: evaluation_mode)
    end

    def store(key_or_hash, value = nil)
      pairs = pairs_to_store(key_or_hash, value)

      unless block_given?
        pairs.each { |key, val| memory[key] = val }
        return self
      end

      # `evaluate!` routes every call through here, so snapshotting the whole
      # memory hash makes each evaluation scale with the number of stored
      # variables (#336). Undoing just the keys this call touched is O(pairs)
      # instead of O(memory) -- but it is only equivalent while nothing else
      # writes into memory during the block. The identifier cache does exactly
      # that, so when it is enabled we still need the wholesale snapshot to
      # keep cached values scoped to a single evaluation.
      if Dentaku.cache_identifier?
        restore = Hash[memory]
        pairs.each { |key, val| memory[key] = val }

        begin
          yield
        ensure
          @memory = restore
        end
      else
        undo = pairs.map { |key, _| [key, memory.key?(key), memory[key]] }
        pairs.each { |key, val| memory[key] = val }

        begin
          yield
        ensure
          undo.each { |key, present, val| present ? memory[key] = val : memory.delete(key) }
        end
      end
    end
    alias_method :bind, :store

    private def pairs_to_store(key_or_hash, value)
      if value.nil?
        key_or_hash = FlatHash.from_hash_with_intermediates(key_or_hash) if nested_data_support
        key_or_hash.map { |key, val| [standardize_case(key.to_s), val] }
      else
        [[standardize_case(key_or_hash.to_s), value]]
      end
    end

    def store_formula(key, formula)
      store(key, ast(formula))
    end

    def clear
      @memory = {}
    end

    def empty?
      memory.empty?
    end

    def cache_ast?
      return false if @disable_ast_cache

      @cache_ast.nil? ? Dentaku.cache_ast? : @cache_ast
    end

    def cache_dependency_order?
      @cache_dependency_order.nil? ? Dentaku.cache_dependency_order? : @cache_dependency_order
    end

    private def parse_expression(expression)
      options = {
        aliases: aliases,
        case_sensitive: case_sensitive,
        function_registry: @function_registry,
        raw_date_literals: raw_date_literals
      }

      tokens = tokenizer.tokenize(expression, options)
      Parser.new(tokens, options).parse
    end

    private def cached_ast(expression)
      case entry = @ast_cache[expression]
      when nil then nil
      when AST::Node then entry # pre-warmed/legacy entries are trusted as-is
      when Hash
        compilation = entry[cache_environment]
        compilation.node if compilation
      end
    end

    private def cache_compilation(expression, compilation)
      entry = @ast_cache[expression]
      return if entry.is_a?(AST::Node) # a pre-warmed node stays authoritative

      entry = @ast_cache[expression] = {} unless entry.is_a?(Hash)
      entry[cache_environment] = compilation
    end

    # Identity of everything besides the expression text that determines how
    # the expression compiles: which registries resolve its functions (so two
    # calculators sharing a cache never see each other's same-named
    # functions), the aliases applied during tokenization, and the tokenizer
    # flags. Evaluation data is never part of the environment, so cached
    # entries stay independent of the inputs of any single call.
    private def cache_environment
      [
        @function_registry.object_id,
        AST::FunctionRegistry.default.object_id,
        aliases.map { |key, vals| [key.to_s, Array(vals).map(&:to_s).sort] }.sort,
        case_sensitive,
        raw_date_literals
      ]
    end

    private def normalize_function_resolution(mode)
      mode = mode.to_sym
      return mode if FUNCTION_RESOLUTION_MODES.include?(mode)

      raise ::ArgumentError,
            "unknown function resolution mode: #{mode.inspect} (expected :pinned or :latest)"
    end
  end
end
