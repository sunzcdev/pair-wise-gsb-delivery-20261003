require 'bigdecimal'
require 'dentaku/ast'
require 'dentaku/token'

module Dentaku
  # Walks an AST with the values available so far and produces a residual
  # AST: every subtree whose value is already determined (and side-effect
  # free to compute) is folded into a literal, everything else is kept
  # as-is so the expression can be evaluated again once the missing data
  # is provided.
  #
  # Folding rules:
  # - unbound identifiers stay in the residual (they are the "missing" inputs)
  # - explicitly provided nil is a determined value and folds to NULL
  # - volatile functions are never called; pure functions are only called
  #   when every argument is determined, so a partial pass plus a final
  #   evaluation never calls any function more times than a single full
  #   evaluation would
  # - IF/CASE only fold the branch their (determined) predicate selects;
  #   the unselected branch is never touched
  # - AND/OR keep their runtime short-circuit behavior: a decisive operand
  #   discards the other side without evaluating it
  # - subtrees that are determined but raise while folding are kept in the
  #   residual and the error is reported separately
  class PartialEvaluator
    attr_reader :errors

    def initialize(context)
      @context = context
      @errors = []
    end

    def evaluate(node)
      case node
      when AST::Literal, AST::Nil
        node
      when AST::Identifier
        evaluate_identifier(node)
      when AST::If
        evaluate_if(node)
      when AST::Combinator
        evaluate_combinator(node)
      when AST::Case
        evaluate_case(node)
      when AST::Negation
        evaluate_unary(node, node.node)
      when AST::Access
        evaluate_access(node)
      when AST::Array
        AST::Array.new(*node.elements.map { |element| evaluate(element) })
      when AST::Function
        evaluate_function(node)
      when AST::Operation
        evaluate_operation(node)
      else
        node
      end
    end

    private

    attr_reader :context

    def evaluate_identifier(node)
      return node unless context.key?(node.identifier)

      case (value = context[node.identifier])
      when AST::Node
        evaluate(value)
      when Proc
        # a proc is user code: leave the identifier in the residual so it is
        # called exactly once, by the final evaluation
        node
      else
        fold(value)
      end
    end

    def evaluate_operation(node)
      left = evaluate(node.left)
      right = evaluate(node.right)

      if constant?(left) && constant?(right)
        fold_determined(node, left, right)
      else
        rebuild(node, left, right)
      end
    end

    def evaluate_unary(node, child)
      folded = evaluate(child)
      constant?(folded) ? fold_determined(node, folded) : rebuild(node, folded)
    end

    def evaluate_access(node)
      structure = evaluate(node.structure)
      index = evaluate(node.index)

      if constant?(structure) && constant?(index)
        fold_determined(node, structure, index)
      else
        rebuild(node, structure, index)
      end
    end

    def evaluate_function(node)
      args = node.args.map { |arg| evaluate(arg) }

      if node.pure? && args.all? { |arg| constant?(arg) }
        fold_determined(node, *args)
      else
        rebuild(node, *args)
      end
    end

    def evaluate_combinator(node)
      left = evaluate(node.left)

      if constant?(left)
        # short-circuit: a decisive left operand decides without the right,
        # a non-decisive one defers to it -- the other side is not touched
        return fold_value(left) if decisive?(node, value_of(left))

        return evaluate(node.right)
      end

      right = evaluate(node.right)

      # an unknown left is still overridden by a decisive right at runtime,
      # but only fold when the left is side-effect free to drop
      if constant?(right) && decisive?(node, value_of(right)) && left.pure?
        return fold_value(right)
      end

      # a non-decisive constant on the right adds nothing: x AND true == x
      return left if constant?(right)

      rebuild(node, left, right)
    end

    def evaluate_if(node)
      predicate = evaluate(node.predicate)

      if constant?(predicate)
        evaluate(value_of(predicate) ? node.left : node.right)
      else
        rebuild(node, predicate, evaluate(node.left), evaluate(node.right))
      end
    end

    def evaluate_case(node)
      switch = evaluate(node.switch.node)

      if constant?(switch)
        switch_value = value_of(switch)

        node.conditions.each do |condition|
          when_node = evaluate(condition.when.node)

          # an undetermined when-clause means the match is undecidable
          return rebuild_case(node, switch) unless constant?(when_node)

          return evaluate(condition.then.node) if value_of(when_node) == switch_value
        end

        if node.else
          evaluate(node.else.node)
        else
          @errors << Dentaku::ArgumentError.for(:invalid_value)
          node
        end
      else
        rebuild_case(node, switch)
      end
    end

    def rebuild_case(node, switch)
      AST::Case.new(
        AST::CaseSwitchVariable.new(switch),
        *node.conditions,
        *(node.else ? [node.else] : [])
      )
    rescue Dentaku::Error
      node
    end

    # the whole subtree is determined: compute it from the already-folded
    # children (evaluating the original node would re-run any pure function
    # calls it contains). A failure here is a real evaluation error, not a
    # missing input, so report it and keep the subtree in the residual
    def fold_determined(node, *children)
      fold(node.class.new(*children).value(context))
    rescue Dentaku::Error => e
      @errors << e
      rebuild(node, *children)
    end

    def rebuild(node, *children)
      node.class.new(*children)
    rescue Dentaku::Error
      node
    end

    def decisive?(combinator, value)
      combinator.send(:decisive?, value)
    end

    def constant?(node)
      case node
      when AST::Literal, AST::Nil
        true
      when AST::Array
        node.elements.all? { |element| constant?(element) }
      else
        false
      end
    end

    def value_of(node)
      node.value(context)
    end

    def fold_value(node)
      fold(node.value(context))
    end

    def fold(value)
      case value
      when AST::Node
        value
      when nil
        AST::Nil.new
      when true, false
        AST::Logical.new(Token.new(:logical, value))
      when BigDecimal
        # engineering notation ("0.25e1") does not re-tokenize, so keep a
        # plain decimal string as the printed representation
        AST::Numeric.new(Token.new(:numeric, value, value.to_s('F')))
      when Numeric
        AST::Numeric.new(Token.new(:numeric, value))
      when Symbol
        AST::String.new(Token.new(:string, value.to_s))
      when ::String
        AST::String.new(Token.new(:string, value))
      when ::Array
        AST::Array.new(*value.map { |element| fold(element) })
      else
        AST::Literal.new(Token.new(nil, value, literal_raw(value)))
      end
    end

    def literal_raw(value)
      value.respond_to?(:iso8601) ? value.iso8601 : value.to_s
    end
  end
end
