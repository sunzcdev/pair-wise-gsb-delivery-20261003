module Flipper
  module Gates
    class Actor < Gate
      # Internal: The name of the gate. Used for instrumentation, etc.
      def name
        :actor
      end

      # Internal: Name converted to value safe for adapter.
      def key
        :actors
      end

      def data_type
        :set
      end

      def enabled?(value)
        !value.empty?
      end

      # Internal: Checks if the gate is open for an actor.
      #
      # Returns true if gate open for actor, false if not.
      def open?(context)
        unless context.actors?
          context.report&.skip_gate(name, :no_actors)
          return false
        end

        enabled_actors = context.values.actors

        if enabled_actors.empty?
          context.report&.record_gate(name, false, enabled_actors: Set.new)
          return false
        end

        matches = context.actors.select do |actor|
          enabled_actors.include?(actor.value)
        end

        result = !matches.empty?
        context.report&.record_gate(
          name,
          result,
          enabled_actors: enabled_actors,
          matched_actors: matches.map(&:value)
        )
        result
      end

      def wrap(actor)
        Types::Actor.wrap(actor)
      end

      def protects?(actor)
        Types::Actor.wrappable?(actor)
      end
    end
  end
end
