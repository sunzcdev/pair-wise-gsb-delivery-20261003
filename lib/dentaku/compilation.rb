module Dentaku
  # The result of compiling an expression against a calculator's function
  # registry (see Calculator#compile). A compilation records the registry
  # generation at compile time and follows one of two contracts:
  #
  # [:pinned] the AST keeps calling the exact function definitions it was
  #           compiled against, even if those functions are re-registered
  #           later. This is the default.
  # [:latest] whenever the registry generation has moved since compilation,
  #           the expression is tokenized and parsed again before use, so
  #           arity, parameter, and return types are re-validated against
  #           the new definitions (a mismatch raises Dentaku::ParseError)
  #           and the AST calls the newest registered definitions.
  #
  # A compilation captures the compile-time environment only -- expression,
  # registry generation, and resolution mode -- never evaluation data, so it
  # is safe to cache and reuse across calls with different inputs.
  class Compilation
    attr_reader :expression, :mode, :registry_generation

    def initialize(calculator, expression, node, mode)
      @calculator = calculator
      @expression = expression
      @node = node
      @mode = mode
      @registry_generation = calculator.registry_generation
    end

    def pinned?
      @mode == :pinned
    end

    def latest?
      @mode == :latest
    end

    # true when a function (re-)registration -- instance or global, including
    # an instance function shadowing a global one -- has happened since this
    # compilation was built
    def stale?
      @registry_generation != @calculator.registry_generation
    end

    # The AST for this compilation. In :latest mode a stale compilation is
    # recompiled first, so the returned node always reflects the newest
    # registered definitions; in :pinned mode it is always the original node.
    def node
      recompile! if latest? && stale?
      @node
    end

    def recompile!
      @node = @calculator.send(:parse_expression, expression)
      @registry_generation = @calculator.registry_generation
      @node
    end
  end
end
