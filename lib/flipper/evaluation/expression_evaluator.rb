module Flipper
  class EvaluationReport
    # Internal: Evaluates an expression tree with Ruby's real short-circuit
    # semantics while recording every node that was actually evaluated. A node
    # skipped by an Any/All short circuit is never called and never recorded, so
    # inputs (properties, Now, Random) are only read when a real evaluation
    # would have read them. The recorded result of every node is the value the
    # real Expression#evaluate would have produced with the same inputs.
    class ExpressionEvaluator
      # Internal: A single evaluated expression node.
      Node = Struct.new(:name, :args, :result, keyword_init: true)

      attr_reader :nodes

      def initialize(context:, clock: nil, random: nil)
        @context = context
        @clock = clock
        @random = random
        @nodes = []
      end

      # Public: Evaluate an Expression or Constant, recording the walk.
      #
      # Returns the final value of the tree.
      def evaluate(expression)
        case expression
        when Expression
          evaluate_expression(expression)
        when Expression::Constant
          expression.evaluate
        else
          expression
        end
      end

      private

      def evaluate_expression(expression)
        result =
          case expression.name
          when "Now"
            now_value
          when "Random"
            random_value(expression.args)
          else
            call_function(expression)
          end

        @nodes << Node.new(
          name: expression.name,
          args: expression.args,
          result: result
        )
        result
      end

      # Internal: Mirrors Expression#evaluate's argument protocol exactly,
      # including Any/All short circuiting, except args are evaluated lazily
      # through this evaluator so skipped branches never run.
      def call_function(expression)
        function = expression.function

        case expression.name
        when "Any", "All"
          evaluate_group(expression)
        else
          if call_with_context?(function)
            function.call(*evaluated_args(expression), context: @context)
          else
            function.call(*evaluated_args(expression))
          end
        end
      end

      def evaluate_group(expression)
        results = []

        expression.args.each do |arg|
          value = evaluate(arg)
          results << value

          if expression.name == "Any"
            # Enumerable#any? treats only genuinely truthy values as a match,
            # so false and nil keep scanning; a truthy value short-circuits.
            return true if value
          elsif !value
            # Enumerable#all? stops at the first falsey value.
            return false
          end
        end

        # Match Enumerable#any?/all? on the fully-collected args, including the
        # empty case (empty Any => false, empty All => true).
        if expression.name == "Any"
          results.any?
        else
          results.all?
        end
      end

      def evaluated_args(expression)
        expression.args.map { |arg| evaluate(arg) }
      end

      def call_with_context?(function)
        # Reuse the exact protocol detection Expression#evaluate relies on, so
        # context-passing functions (Property, FeatureEnabled,
        # PercentageOfActors) stay in sync with the normal evaluation path.
        Expression.function_takes_context?(function)
      end

      def now_value
        @clock ? @clock.call : Expressions::Now.call
      end

      def random_value(args)
        max = args.empty? ? 0 : evaluate(args.first)

        if @random
          @random.call(max)
        else
          Expressions::Random.call(max)
        end
      rescue TypeError, RangeError
        nil
      end
    end
  end
end
