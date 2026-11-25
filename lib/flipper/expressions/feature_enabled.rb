require "set"

module Flipper
  module Expressions
    class FeatureEnabled
      EVALUATING_KEY = :flipper_evaluating_features

      # Public: Thread-local key for the stack of features whose expression
      # rules are currently being interpreted. Adapters that federate storage
      # across backends (e.g. Flipper::Adapters::Federated) read it to tell
      # which managed flag a nested reference originates from; ordinary
      # adapters ignore it.
      ORIGIN_STACK_KEY = :flipper_federated_origin_stack

      def self.call(feature_name, context:)
        evaluating = Thread.current[EVALUATING_KEY] ||= Set.new
        feature_name = feature_name.to_s
        current_feature = context[:feature_name].to_s

        # Track the current feature so A -> B -> A is caught
        added_current = evaluating.add?(current_feature)

        # Record whose rule is making this reference.
        origin_stack = Thread.current[ORIGIN_STACK_KEY] ||= []
        origin_stack.push(current_feature)

        begin
          # Circular dependency: return false to break the cycle
          return false if evaluating.include?(feature_name)

          evaluating.add(feature_name)
          actor = context[:actor]
          if actor
            Flipper.enabled?(feature_name, actor)
          else
            Flipper.enabled?(feature_name)
          end
        ensure
          origin_stack.pop
          Thread.current[ORIGIN_STACK_KEY] = nil if origin_stack.empty?
          evaluating.delete(feature_name)
          evaluating.delete(current_feature) if added_current
        end
      end
    end
  end
end
