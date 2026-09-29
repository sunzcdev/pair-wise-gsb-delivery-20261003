require_relative '../function'
require 'dentaku/units'

module Dentaku
  module AST
    class If < Function
      attr_reader :predicate, :left, :right

      def self.min_param_count
        3
      end

      def self.max_param_count
        3
      end

      def initialize(predicate, left, right)
        @predicate = predicate
        @left      = left
        @right     = right
      end

      def args
        [predicate, left, right]
      end

      def value(context = {})
        taken = predicate.value(context)
        result = (taken ? left : right).value(context)
        check_branch_units(result, taken ? right : left, context)
        result
      end

      def node_type
        :condition
      end

      def type
        left.type
      end

      def dependencies(context = {})
        return all_arg_dependencies(context) if static_mode?(context) || !predicate.pure?

        predicate.value(context) ? left.dependencies(context) : right.dependencies(context)
      rescue Dentaku::Error
        all_arg_dependencies(context)
      end

      private

      # in units mode, both branches must agree on their result dimension;
      # the untaken branch is only evaluated for the check when it is
      # side-effect free
      def check_branch_units(result, other_branch, context)
        return unless context[Dentaku::Units::CONTEXT_KEY] && other_branch.pure?

        Dentaku::Units.assert_compatible!(result, other_branch.value(context), node: self)
      rescue Dentaku::UnitError
        raise
      rescue Dentaku::Error
        # the untaken branch cannot be evaluated in this context
        # (e.g. unbound variables pruned from the dependencies); skip
        nil
      end

      def all_arg_dependencies(context)
        args.flat_map { |arg| arg.dependencies(context) }.uniq
      end
    end
  end
end

Dentaku::AST::Function.register_class(:if, Dentaku::AST::If)
