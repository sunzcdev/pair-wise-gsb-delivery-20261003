require "set"
require "flipper/actor"
require "flipper/gate_values"

module Flipper
  module Adapters
    class Sync
      # Internal: Given a feature, local gate values and remote gate values,
      # makes the local equal to the remote.
      #
      # It accepts both kinds of update the sync sees:
      #
      # * A complete mirror, given as Flipper::GateValues or as a Hash carrying
      #   every gate. The local feature is overwritten to match, so gates the
      #   remote no longer has are cleared. This is the original sync contract.
      # * An increment, given as a Hash carrying only the gates it mentions.
      #   Every gate it does not mention keeps whatever the local adapter
      #   already holds. Removing a gate is always explicit: the gate is
      #   mentioned with a nil value (or an empty set for set gates), or named
      #   under :removed_gates.
      #
      #   FeatureSynchronizer.new(feature, feature.gate_values, {actors: Set["2"]}).call
      #   # => actors becomes Set["2"], every other gate is left alone
      #
      # Passing :deleted => true (or :remove => true) removes the whole feature.
      class FeatureSynchronizer
        extend Forwardable

        # Internal: Every gate a feature can have.
        GATE_KEYS = Set[
          :boolean,
          :expression,
          :actors,
          :groups,
          :percentage_of_actors,
          :percentage_of_time,
        ].freeze

        def_delegator :@local_gate_values, :expression, :local_expression
        def_delegator :@local_gate_values, :boolean, :local_boolean
        def_delegator :@local_gate_values, :actors, :local_actors
        def_delegator :@local_gate_values, :groups, :local_groups
        def_delegator :@local_gate_values, :percentage_of_actors,
                      :local_percentage_of_actors
        def_delegator :@local_gate_values, :percentage_of_time,
                      :local_percentage_of_time

        def_delegator :@remote_gate_values, :expression, :remote_expression
        def_delegator :@remote_gate_values, :boolean, :remote_boolean
        def_delegator :@remote_gate_values, :actors, :remote_actors
        def_delegator :@remote_gate_values, :groups, :remote_groups
        def_delegator :@remote_gate_values, :percentage_of_actors,
                      :remote_percentage_of_actors
        def_delegator :@remote_gate_values, :percentage_of_time,
                      :remote_percentage_of_time

        # Internal: Initializes a new feature synchronizer.
        #
        # feature - The Flipper::Feature to write through.
        # local_gate_values - The Flipper::GateValues currently stored locally.
        # remote_gate_values - The Flipper::GateValues (complete mirror) or
        #                      Hash (complete when it carries every gate,
        #                      increment otherwise) to settle locally.
        # options - The Hash of options.
        #           :full - Forces a Hash to be treated as a complete mirror.
        #           :incremental - Forces a Hash to be treated as an increment.
        #           :changed_gates - The Array of gate keys the update mentions.
        #           :removed_gates - The Array of gate keys the update clears.
        def initialize(feature, local_gate_values, remote_gate_values, options = {})
          @feature = feature
          @local_gate_values = local_gate_values
          @deleted = false

          if remote_gate_values.is_a?(Hash)
            initialize_from_hash(remote_gate_values, options)
          else
            @remote_gate_values = remote_gate_values
            @removed_gates = Set.new(Array(options[:removed_gates]).map(&:to_sym))
            if options.key?(:changed_gates) && options[:changed_gates]
              @changed_gates = Set.new(Array(options[:changed_gates]).map(&:to_sym))
            else
              @changed_gates = nil
            end
          end
        end

        # Public: Settles the update against the feature.
        def call
          if @deleted
            @feature.remove
          elsif @changed_gates
            sync_changed_gates
          else
            sync_all_gates
          end
        end

        private

        def initialize_from_hash(hash, options)
          values = {}
          removed = Set.new
          deleted = false

          hash.each do |key, value|
            name = key.respond_to?(:to_sym) ? key.to_sym : key

            case name
            when :deleted, :remove
              deleted ||= !!value
            when :removed_gates, :removed
              Array(value).each { |gate| removed << gate.to_sym }
            else
              values[name] = value
            end
          end

          if options.key?(:removed_gates)
            Array(options[:removed_gates]).each { |gate| removed << gate.to_sym }
          end

          @deleted = deleted
          @remote_gate_values = GateValues.new(values)

          changed = options[:changed_gates]
          full = if options.key?(:full)
            options[:full]
          elsif options.key?(:incremental)
            !options[:incremental]
          else
            # A hash naming every gate is a complete mirror. Anything less is
            # an increment and must not touch the gates it left out.
            GATE_KEYS.subset?(Set.new(values.keys))
          end

          if full
            @changed_gates = nil
            @removed_gates = Set.new
          else
            @changed_gates = Set.new(Array(changed).map(&:to_sym))
            @changed_gates.merge(values.keys.map(&:to_sym))
            @changed_gates.merge(removed)
            @removed_gates = removed
          end
        end

        # Internal: Complete mirror. Overwrites the local feature.
        def sync_all_gates
          if remote_disabled?
            return if local_disabled?
            @feature.disable
          elsif remote_boolean_enabled?
            return if local_boolean_enabled?
            @feature.enable
          else
            @feature.disable if local_boolean_enabled?
            sync_groups
            sync_actors
            sync_expression
            sync_percentage_of_actors
            sync_percentage_of_time
          end
        end

        # Internal: Increment. Only the changed gates are settled; the rest keep
        # their local value.
        def sync_changed_gates
          if changed_gate?(:boolean) && local_boolean_enabled? != remote_boolean_enabled?
            # Enabling and disabling the boolean gate both clear every other
            # gate, so the whole feature is rewritten from the values this
            # update settles to: the mentioned gates take the remote value and
            # every other gate keeps the local one.
            remote_boolean_enabled? ? @feature.enable : @feature.disable
            restore_settled_gates
            return
          end

          sync_groups if changed_gate?(:groups)
          sync_actors if changed_gate?(:actors)
          sync_expression if changed_gate?(:expression)
          sync_percentage_of_actors if changed_gate?(:percentage_of_actors)
          sync_percentage_of_time if changed_gate?(:percentage_of_time)
        end

        # Internal: Did this increment mention the gate?
        def changed_gate?(gate)
          @changed_gates.include?(gate)
        end

        # Internal: Rewrites every gate other than boolean after the boolean
        # gate cleared them, using the value this update settles that gate to.
        def restore_settled_gates
          settled_actors.each { |flipper_id| @feature.enable_actor Actor.new(flipper_id) }
          settled_groups.each { |group_name| @feature.enable_group group_name }

          if (expression = settled_expression)
            @feature.__send__ :enable_expression_from_sync, expression
          end

          if (percentage = settled_percentage_of_actors) > 0
            @feature.enable_percentage_of_actors percentage
          end

          if (percentage = settled_percentage_of_time) > 0
            @feature.enable_percentage_of_time percentage
          end
        end

        def settled_actors
          changed_gate?(:actors) ? remote_actors : local_actors
        end

        def settled_groups
          changed_gate?(:groups) ? remote_groups : local_groups
        end

        def settled_expression
          changed_gate?(:expression) ? remote_expression : local_expression
        end

        def settled_percentage_of_actors
          changed_gate?(:percentage_of_actors) ? remote_percentage_of_actors : local_percentage_of_actors
        end

        def settled_percentage_of_time
          changed_gate?(:percentage_of_time) ? remote_percentage_of_time : local_percentage_of_time
        end

        # Internal: Settles the expression gate, including clearing it.
        def sync_expression
          return if local_expression == remote_expression

          if remote_expression.nil?
            @feature.disable_expression
          else
            # Remote adapters can hold legacy empty-group expressions, which
            # must be mirrored faithfully without weakening public enables.
            @feature.__send__ :enable_expression_from_sync, remote_expression
          end
        end

        def sync_actors
          remote_actors_added = remote_actors - local_actors
          remote_actors_added.each do |flipper_id|
            @feature.enable_actor Actor.new(flipper_id)
          end

          remote_actors_removed = local_actors - remote_actors
          remote_actors_removed.each do |flipper_id|
            @feature.disable_actor Actor.new(flipper_id)
          end
        end

        def sync_groups
          remote_groups_added = remote_groups - local_groups
          remote_groups_added.each do |group_name|
            @feature.enable_group group_name
          end

          remote_groups_removed = local_groups - remote_groups
          remote_groups_removed.each do |group_name|
            @feature.disable_group group_name
          end
        end

        def sync_percentage_of_actors
          return if local_percentage_of_actors == remote_percentage_of_actors

          @feature.enable_percentage_of_actors remote_percentage_of_actors
        end

        def sync_percentage_of_time
          return if local_percentage_of_time == remote_percentage_of_time

          @feature.enable_percentage_of_time remote_percentage_of_time
        end

        def default_config
          @default_config ||= @feature.adapter.default_config
        end

        def default_gate_values
          @default_gate_values ||= GateValues.new(default_config)
        end

        def default_gate_values?(gate_values)
          gate_values == default_gate_values
        end

        def local_disabled?
          default_gate_values? @local_gate_values
        end

        def remote_disabled?
          default_gate_values? @remote_gate_values
        end

        def local_boolean_enabled?
          local_boolean
        end

        def remote_boolean_enabled?
          remote_boolean
        end
      end
    end
  end
end
