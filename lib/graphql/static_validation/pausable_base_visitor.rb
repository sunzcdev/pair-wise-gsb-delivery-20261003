# frozen_string_literal: true
module GraphQL
  module StaticValidation
    # Included LAST into a pausable visitor class so its visit hooks sit above
    # every rule module in the method lookup chain. Each hook charges one unit
    # of checking work to the {ValidationSession} *before* the rule callbacks
    # for that AST node run.
    #
    # It only changes when the visitor pauses, never what the rules do: a
    # document validated in many small advances produces exactly the same
    # errors, in the same order, as one uninterrupted validation.
    #
    # @see ValidationSession
    module PausableWork
      # One visit hook per AST node type. Defining these once up front keeps the
      # per-node overhead at one method call plus one integer decrement.
      GraphQL::Language::StaticVisitor.instance_methods(false).each do |visit_method|
        # Wrap only the per-node `on_<node>(node, parent)` hooks, never the
        # `on_<node>_children(node)` traversal helpers.
        next unless visit_method.start_with?("on_") && !visit_method.end_with?("_children")

        module_eval(<<-RUBY, __FILE__, __LINE__ + 1)
          def #{visit_method}(node, parent)
            # Suspends the driving fiber once this advance's budget is spent, or
            # unwinds when the session was cancelled; then runs the real rules
            # for this node via `super`.
            @context.consume_work(1)
            super
          end
        RUBY
      end
    end

    # Builds visitor classes which cooperate with a {ValidationSession}.
    # Mirrors {BaseVisitor.including_rules}, but {PausableWork} is mixed in
    # last so work is charged before any rule callback.
    class PausableBaseVisitor < BaseVisitor
      # @param rules [Array<Module, Class>]
      # @return [Class]
      def self.including_pausable_rules(rules)
        if rules.empty?
          # No rules, but walking the AST still counts as work.
          @empty_rules_visitor ||= Class.new(self) { include(PausableWork) }
        elsif rules == ALL_RULES
          @all_rules_visitor ||= Class.new(InterpreterVisitor) { include(PausableWork) }
        else
          visitor_class = Class.new(self) do
            include(GraphQL::StaticValidation::DefinitionDependencies)
          end

          rules.reverse_each do |r|
            # If it's a class, it gets attached later (same as BaseVisitor).
            visitor_class.include(r) unless r.is_a?(Class)
          end

          visitor_class.include(ContextMethods)
          # Included last so it is the first hook reached during dispatch.
          visitor_class.include(PausableWork)
          visitor_class
        end
      end
    end
  end
end
