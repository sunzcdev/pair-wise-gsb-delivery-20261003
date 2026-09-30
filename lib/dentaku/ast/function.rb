require_relative 'node'
require_relative 'function_registry'

module Dentaku
  module AST
    class Function < Node
      attr_reader :args

      # @return [Integer] with the number of significant decimal digits to use.
      DIG = Float::DIG + 1

      def initialize(*args)
        @args = args
      end

      def accept(visitor)
        visitor.visit_function(self)
      end

      def partial(evaluator, guarded)
        # Never invoke a function while simplifying a branch that a full
        # evaluation might not execute, and never invoke volatile
        # functions at all (their result may depend on side effects).
        return self if guarded

        if self.class.volatile?
          # arguments may only be simplified in guarded mode (no calls);
          # the function itself is never evaluated
          reduced_args = @args.map { |arg| evaluator.reduce(arg, guarded: true) }

          return self if reduced_args.each_with_index.all? { |arg, i| arg.equal?(@args[i]) }

          return evaluator.rebuild(self.class, *reduced_args) || self
        end

        reduced_args = @args.map { |arg| evaluator.reduce(arg, guarded: false) }

        if reduced_args.all? { |arg| evaluator.constant?(arg) }
          rebuilt = evaluator.rebuild(self.class, *reduced_args)
          return self if rebuilt.nil?
          value = evaluator.evaluate_node(rebuilt)
          return rebuilt if evaluator.error_result?(value)

          return evaluator.constant_for(value)
        end

        return self if reduced_args.each_with_index.all? { |arg, i| arg.equal?(@args[i]) }

        evaluator.rebuild(self.class, *reduced_args) || self
      end

      def dependencies(context = {})
        @args.each_with_index
             .flat_map { |a, _| a.dependencies(context) }
      end

      # volatile functions may not be evaluated during dependency
      # resolution; registry-generated classes override this per the
      # volatile: option passed at registration
      def self.volatile?
        false
      end

      def self.get(name)
        registry.get(name)
      end

      def self.register(name, type, implementation, volatile: false)
        registry.register(name, type, implementation, volatile: volatile)
      end

      def self.register_class(name, function_class)
        registry.register_class(name, function_class)
      end

      def self.registry
        @registry ||= FunctionRegistry.new
      end

      private

      def compute_pure?
        !self.class.volatile? && args.all?(&:pure?)
      end
    end
  end
end
