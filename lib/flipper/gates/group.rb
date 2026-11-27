module Flipper
  module Gates
    class Group < Gate
      # Internal: The name of the gate. Used for instrumentation, etc.
      def name
        :group
      end

      # Internal: Name converted to value safe for adapter.
      def key
        :groups
      end

      def data_type
        :set
      end

      def enabled?(value)
        !value.empty?
      end

      # Internal: Checks if the gate is open for a thing.
      #
      # Returns true if gate open for thing, false if not.
      def open?(context)
        unless context.actors?
          context.report&.skip_gate(name, :no_actors)
          return false
        end

        group_names = context.values.groups

        if group_names.empty?
          context.report&.record_gate(name, false, groups: Set.new, checks: [])
          return false
        end

        checks = []
        result = group_names.any? do |group_name|
          group = Flipper.group(group_name)
          actor_checks = context.actors.map do |actor|
            matched = group.match?(actor, context)
            check = {
              group: group_name,
              actor: actor.value,
              result: !!matched
            }
            checks << check
            matched
          end
          actor_checks.any?
        end

        context.report&.record_gate(
          name,
          result,
          groups: group_names,
          checks: checks
        )
        result
      end

      def wrap(thing)
        Types::Group.wrap(thing)
      end

      def protects?(thing)
        thing.is_a?(Types::Group) || thing.is_a?(Symbol)
      end
    end
  end
end
