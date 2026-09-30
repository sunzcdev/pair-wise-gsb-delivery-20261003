require 'dentaku/ast'
require 'dentaku/exceptions'
require 'dentaku/print_visitor'

module Dentaku
  # Walks an AST with a (possibly incomplete) context and folds every
  # subtree that can be computed without ambiguity, while leaving the
  # rest as a residual expression that can be evaluated again later.
  #
  # Three kinds of "unknown" are kept strictly apart:
  #
  #   * an identifier missing from the context stays an Identifier node
  #     and is reported in +missing_variables+
  #   * an identifier explicitly bound to nil folds to a constant nil
  #     (NULL), exactly like a complete evaluation
  #   * a subtree that raises during evaluation is left untouched and the
  #     error is collected in +errors+
  #
  # Folding is conservative: volatile functions are never called, and
  # guarded mode forbids every function call so unselected IF branches and
  # short-circuited operands are never executed just to simplify them.
  class PartialEvaluator
    attr_reader :context

    def initialize(context = {})
      @context = context
      @errors = []
    end

    def evaluate(node)
      residual = reduce(node, guarded: false)
      residual = residual.node if residual.is_a?(AST::Grouping)

      PartialEvaluation.new(
        residual,
        missing_variables(residual),
        @errors
      )
    end

    # ---- reducer primitives used by AST nodes --------------------------

    def reduce(node, guarded: false)
      node.partial(self, guarded)
    end

    # A node whose value is already known without touching the context.
    def constant?(node)
      node.is_a?(AST::Constant) ||
        node.is_a?(AST::Literal) ||
        node.is_a?(AST::Nil)
    end

    # Evaluate a rebuilt, context-independent node. Errors are recorded
    # and returned to the caller, which is responsible for keeping the
    # original subtree in the residual.
    def evaluate_node(node)
      node.value(@context)
    rescue ::StandardError => e
      # custom functions may raise arbitrary Ruby errors; partial
      # evaluation reports them and keeps the subtree rather than
      # aborting the whole reduction
      record_error(e)
      e
    end

    def error_result?(value)
      value.is_a?(::Exception)
    end

    # Rebuild a node with reduced children. Constructor type checks are
    # static (they accept unresolved identifiers but reject a folded
    # constant of an incompatible type); such a mismatch is a runtime
    # error of the original expression, so it is recorded and the
    # original node is retained instead of raising during folding.
    def rebuild(node_class, *args)
      node_class.new(*args)
    rescue ::StandardError => e
      record_error(e)
      nil
    end

    def constant_for(value)
      AST::Constant.new(value)
    end

    def bound?(identifier)
      @context.key?(identifier)
    end

    def bound_value(identifier)
      @context.fetch(identifier)
    end

    # Resolves a bound identifier through the same rules as a complete
    # evaluation (stored AST nodes are evaluated, Procs are called).
    def lookup(identifier)
      value = @context.fetch(identifier)

      case value
      when AST::Node
        value.value(@context)
      when Proc
        value.call
      else
        value
      end
    end

    def record_error(error)
      @errors << error unless @errors.include?(error)
    end

    # Wrap a replacement in parentheses when it becomes a standalone
    # branch/body and its own precedence would otherwise change the
    # meaning of the residual expression.
    def embed(node)
      needs_grouping?(node) ? AST::Grouping.new(node) : node
    end

    def needs_grouping?(node)
      node.is_a?(AST::Operation)
    end

    private

    # Structural scan over the residual: every identifier that could still
    # be needed to finish the computation. Never evaluates guards or
    # functions, so it performs no calls at all.
    def missing_variables(residual)
      residual
        .dependencies(AST::Node::STATIC_CONTEXT)
        .reject { |name| @context.key?(name) }
        .uniq
    end
  end
end
