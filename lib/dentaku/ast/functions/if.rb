require_relative '../function'

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
        taken, untaken = predicate.value(context) ? [left, right] : [right, left]
        result = taken.value(context)
        check_branch_units(result, untaken, context)
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

      # in units mode both branches must agree on dimension; the untaken
      # branch is only probed when it is side-effect free, and a branch that
      # fails to evaluate (e.g. a guarded division) skips the check
      def check_branch_units(result, other_branch, context)
        return unless context[Dentaku::Units::MODE_KEY]
        return unless other_branch.pure?

        other_value =
          begin
            other_branch.value(context)
          rescue Dentaku::Error
            return
          end

        Dentaku::Units.assert_compatible!(result, other_value, node: self)
      end

      def all_arg_dependencies(context)
        args.flat_map { |arg| arg.dependencies(context) }.uniq
      end
    end
  end
end

Dentaku::AST::Function.register_class(:if, Dentaku::AST::If)
