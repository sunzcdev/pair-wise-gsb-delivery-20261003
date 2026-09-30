module Dentaku
  # The result of Calculator#compile: a parsed expression plus the
  # function-registry generation it was compiled against.
  #
  # The compile mode decides which definition a compiled call uses when a
  # function is (re)registered after compilation:
  #
  # [:pinned]  the AST keeps calling the definitions captured at compile
  #            time; later registrations are ignored.
  # [:latest]  before each use, the effective registry (instance functions
  #            layered over the global defaults), the aliases, and the
  #            parsing options are compared with the compile-time identity;
  #            any change recompiles from source, so argument counts and
  #            declared return types are validated against the current
  #            definitions before the new AST is used.
  class CompiledExpression
    attr_reader :source, :mode, :registry_generation

    def initialize(calculator, source, node, mode)
      @calculator = calculator
      @source = source
      @node = node
      @mode = mode
      capture_identity
    end

    def pinned?
      @mode == :pinned
    end

    def latest?
      @mode == :latest
    end

    # true when the function registry, aliases, or parsing options no
    # longer match the compile-time identity; only :latest expressions
    # act on this
    def stale?
      @calculator.compile_identity != @identity
    end

    # the AST to use right now; recompiles first when stale in :latest mode
    def node
      refresh! if latest? && stale?
      @node
    end

    def value(context = {})
      node.value(context)
    end

    def dependencies(context = {})
      node.dependencies(context)
    end

    # recompile from source against the current definitions, re-running
    # arity and type validation
    def refresh!
      @node = @calculator.ast(@source)
      capture_identity
      self
    end

    private

    def capture_identity
      @identity = @calculator.compile_identity
      @registry_generation = @calculator.function_registry_generation
    end
  end
end
