require 'dentaku/bulk_expression_solver'
require 'dentaku/compiled_expression'
require 'dentaku/dependency_resolver'
require 'dentaku/exceptions'
require 'dentaku/flat_hash'
require 'dentaku/parser'
require 'dentaku/string_casing'
require 'dentaku/token'
require 'digest'

module Dentaku
  class Calculator
    include StringCasing
    attr_reader :result, :memory, :tokenizer, :case_sensitive,
                :nested_data_support, :ast_cache, :raw_date_literals

    def initialize(case_sensitive: false, aliases: nil, nested_data_support: true,
                   raw_date_literals: true, ast_cache: {},
                   cache_ast: nil, cache_dependency_order: nil)
      clear
      @tokenizer = Tokenizer.new
      @case_sensitive = case_sensitive
      @aliases = aliases
      @nested_data_support = nested_data_support
      @raw_date_literals = raw_date_literals
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
      return expression.node if expression.is_a?(CompiledExpression)
      return expression if expression.is_a?(AST::Node)
      return expression.map { |e| ast(e) } if expression.is_a? Array

      cached = @ast_cache[expression]
      # pre-warmed or externally managed entries carry no compile identity,
      # so they are trusted as-is
      return cached if cached.is_a?(AST::Node)

      identity = nil
      if cached.is_a?(Hash)
        identity = compile_identity
        node = cached[identity]
        return node if node
      end

      parse_expression(expression).tap do |node|
        next unless cache_ast?

        # one slot per compile identity: calculators that share an external
        # cache but register different definitions never see each other's
        # ASTs, so results cannot depend on which of them evaluated first.
        # Only the expression and the identity key the cache -- never the
        # data being evaluated.
        identity ||= compile_identity
        slot = cached.is_a?(Hash) ? cached : {}
        slot[identity] = node
        @ast_cache[expression] = slot
      end
    end

    # Compile an expression once and choose how the result treats functions
    # that are (re)registered afterwards:
    #
    # functions: :pinned (default) -- the compiled AST keeps calling the
    #   definitions captured at compile time.
    # functions: :latest -- each use checks the registry generation and
    #   recompiles (re-validating argument counts and declared return
    #   types) when any function was re-registered, an instance function to
    #   override a global one was added, or the aliases changed.
    def compile(expression, functions: :pinned)
      unless expression.is_a?(String)
        raise ::ArgumentError, "can only compile String expressions"
      end

      unless %i[pinned latest].include?(functions)
        raise ::ArgumentError, "functions must be :pinned or :latest"
      end

      CompiledExpression.new(self, expression, ast(expression), functions)
    end

    # identity of everything that shapes a compiled AST: the effective
    # function registry (instance functions layered over the global
    # defaults), the aliases, and the parsing options. Two calculators
    # share AST cache entries only when their identities match.
    def compile_identity
      [
        @function_registry.fingerprint,
        aliases_fingerprint,
        case_sensitive,
        raw_date_literals
      ]
    end

    # monotonic generation of the effective function registry, including
    # the global defaults this calculator falls back to
    def function_registry_generation
      @function_registry.generation
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

    private def aliases_fingerprint
      canonical = aliases.map { |key, values|
        "#{key}=#{Array(values).map(&:to_s).sort.join(',')}"
      }.sort.join(';')
      Digest::SHA256.hexdigest(canonical)
    end
  end
end
