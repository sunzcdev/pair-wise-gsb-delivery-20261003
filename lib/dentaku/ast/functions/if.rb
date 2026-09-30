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
        predicate.value(context) ? left.value(context) : right.value(context)
      end

      def node_type
        :condition
      end

      def partial(evaluator, guarded)
        # the predicate is always required, so it may be folded normally
        reduced_predicate = evaluator.reduce(predicate, guarded: guarded)

        if evaluator.constant?(reduced_predicate)
          chosen = reduced_predicate.value ? left : right
          return evaluator.embed(evaluator.reduce(chosen, guarded: guarded))
        end

        # predicate not known: neither branch is guaranteed to run, so
        # both are kept and reduced without invoking any functions
        reduced_left = evaluator.reduce(left, guarded: true)
        reduced_right = evaluator.reduce(right, guarded: true)

        return self if reduced_predicate.equal?(predicate) &&
                       reduced_left.equal?(left) &&
                       reduced_right.equal?(right)

        evaluator.rebuild(self.class, reduced_predicate, reduced_left, reduced_right) || self
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

      def all_arg_dependencies(context)
        args.flat_map { |arg| arg.dependencies(context) }.uniq
      end
    end
  end
end

Dentaku::AST::Function.register_class(:if, Dentaku::AST::If)
