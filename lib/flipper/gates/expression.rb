require "flipper/expression"
require "flipper/evaluation_report"
require "flipper/evaluation/expression_evaluator"

module Flipper
  module Gates
    class Expression < Gate
      # Internal: The name of the gate. Used for instrumentation, etc.
      def name
        :expression
      end

      # Internal: Name converted to value safe for adapter.
      def key
        :expression
      end

      def data_type
        :json
      end

      def enabled?(value)
        !value.nil? && !value.empty?
      end

      # Internal: Checks if the gate is open for a thing.
      #
      # Returns true if gate open for thing, false if not.
      def open?(context)
        data = context.values.expression

        if data.nil? || data.empty?
          context.report&.record_gate(name, false, expression: nil)
          return false
        end

        expression = Flipper::Expression.build(data)

        if context.actors.nil? || context.actors.empty?
          result, nodes = evaluate_without_actor(context, expression)
          context.report&.record_gate(
            name,
            result,
            expression: expression,
            actor_results: [],
            nodes: nodes
          )
          result
        else
          actor_results = evaluate_for_actors(context, expression)
          result = actor_results.any? { |actor_result| actor_result[:result] }
          context.report&.record_gate(
            name,
            result,
            expression: expression,
            actor_results: actor_results
          )
          result
        end
      rescue Flipper::Expression::UnknownFunction
        # The expression uses a function from a newer Flipper (e.g. one Cloud
        # synced before this app upgraded). Keep the gate closed instead of
        # raising on every check.
        context.report&.record_gate(name, false, reason: :unknown_function)
        false
      end

      def protects?(thing)
        thing.is_a?(Flipper::Expression) || thing.is_a?(Hash)
      end

      def wrap(thing)
        Flipper::Expression.build(thing)
      end

      private

      # Internal
      DEFAULT_PROPERTIES = {}.freeze

      def evaluate_without_actor(context, expression)
        if context.report
          evaluator = EvaluationReport::ExpressionEvaluator.new(
            context: context.report.expression_context(
              feature_name: context.feature_name,
              properties: DEFAULT_PROPERTIES,
              actor: nil
            ),
            clock: context.report.clock,
            random: context.report.random
          )
          result = !!evaluator.evaluate(expression)
          [result, evaluator.nodes]
        else
          result = !!expression.evaluate(feature_name: context.feature_name, properties: DEFAULT_PROPERTIES, actor: nil)
          [result, nil]
        end
      end

      def evaluate_for_actors(context, expression)
        context.actors.map do |actor|
          actor_properties = properties(actor)

          if context.report
            evaluator = EvaluationReport::ExpressionEvaluator.new(
              context: context.report.expression_context(
                feature_name: context.feature_name,
                properties: actor_properties,
                actor: actor
              ),
              clock: context.report.clock,
              random: context.report.random
            )
            result = !!evaluator.evaluate(expression)
            {
              actor: actor.value,
              result: result,
              properties: actor_properties,
              nodes: evaluator.nodes
            }
          else
            result = !!expression.evaluate(
              feature_name: context.feature_name,
              properties: actor_properties,
              actor: actor
            )
            { actor: actor.value, result: result }
          end
        end
      end

      def properties(actor)
        return DEFAULT_PROPERTIES if actor.nil?

        properties = {}

        if actor.respond_to?(:flipper_properties)
          properties.update(actor.flipper_properties)
        else
          warn "#{actor.inspect} does not respond to `flipper_properties` but should."
        end

        properties.transform_keys!(&:to_s)

        if actor.respond_to?(:flipper_id)
          properties["flipper_id".freeze] = actor.flipper_id
        end

        properties
      end
    end
  end
end
