# frozen_string_literal: true

require_relative 'options_validator'

module StateMachines
  # Represents a collection of state machines for a class
  class MachineCollection < Hash
    # Initializes the state of each machine in the given object.  This can allow
    # states to be initialized in two groups: static and dynamic.  For example:
    #
    #   machines.initialize_states(object) do
    #     # After static state initialization, before dynamic state initialization
    #   end
    #
    # If no block is provided, then all states will still be initialized.
    #
    # Valid configuration options:
    # * <tt>:static</tt> - Whether to initialize static states. Unless set to
    #   false, the state will be initialized regardless of its current value.
    #   Default is true.
    # * <tt>:dynamic</tt> - Whether to initialize dynamic states.  If set to
    #   :force, the state will be initialized regardless of its current value.
    #   Default is true.
    # * <tt>:to</tt> - A hash to write the initialized state to instead of
    #   writing to the object.  Default is to write directly to the object.
    def initialize_states(object, options = {}, attributes = {})
      StateMachines::OptionsValidator.assert_valid_keys!(options, :static, :dynamic, :to)
      options = { static: true, dynamic: true }.merge(options)

      result = yield if block_given?

      if options[:static]
        each_value do |machine|
          unless machine.dynamic_initial_state?
            force = options[:static] == :force || !attributes.keys.map(&:to_sym).include?(machine.attribute)
            machine.initialize_state(object, force: force, to: options[:to])
          end
        end
      end

      if options[:dynamic]
        each_value do |machine|
          machine.initialize_state(object, force: options[:dynamic] == :force, to: options[:to]) if machine.dynamic_initial_state?
        end
      end

      result
    end

    # Builds a JSON-compatible snapshot of the current state of every machine
    # in this collection for the given object.  The snapshot is keyed by
    # machine name and captures each machine's definition version and state
    # value, so it can be persisted (e.g. via JSON.generate) and restored
    # after a restart.
    #
    # == Example
    #
    #   Vehicle.state_machines.snapshot(vehicle)
    #   # => {"state"=>{"version"=>1, "value"=>"parked"}, "alarm_state"=>{"version"=>2, "value"=>"active"}}
    def snapshot(object)
      each_with_object({}) do |(name, machine), result|
        result[name.to_s] = machine.snapshot(object)
      end
    end

    # Restores machine states previously exported via #snapshot onto the
    # given object.
    #
    # Every machine is fully validated -- including version migrations and
    # state lookups -- *before* anything is written, so a broken snapshot
    # raises StateMachines::InvalidSnapshot and leaves the object completely
    # untouched.  States are written directly to the machine attributes:
    # no events are fired and no callbacks or actions are run.
    #
    # The snapshot must be JSON-compatible data (e.g. the result of
    # JSON.parse); arbitrary Ruby objects are not accepted.
    def restore(object, snapshot)
      unless snapshot.is_a?(Hash) && StateMachines::Snapshot.json_compatible?(snapshot)
        raise InvalidSnapshot, 'Snapshot must be a JSON-compatible hash of machine snapshots'
      end

      unknown = snapshot.keys - keys.map(&:to_s)
      raise InvalidSnapshot, "Snapshot contains unknown state machines: #{unknown.join(', ')}" if unknown.any?

      # Validate every machine and compute the values to write *before*
      # modifying the object, so a broken snapshot can never leave the
      # object partially restored
      writes = map do |name, machine|
        entry = snapshot[name.to_s]
        raise InvalidSnapshot, "Snapshot is missing an entry for #{name.inspect}" if entry.nil?

        [machine, machine.restore_value(entry)]
      end

      writes.each { |machine, value| machine.write(object, :state, value) }
      object
    end

    # Runs one or more events in parallel on the given object.  See
    # StateMachines::InstanceMethods#fire_events for more information.
    def fire_events(object, *events)
      run_action = [true, false].include?(events.last) ? events.pop : true

      # Generate the transitions to run for each event
      transitions = events.collect do |event_name|
        # Find the actual event being run
        event = nil
        detect { |_name, machine| event = machine.events[event_name, :qualified_name] }

        raise(InvalidEvent.new(object, event_name)) unless event

        # Get the transition that will be performed for the event
        unless (transition = event.transition_for(object))
          event.on_failure(object)
        end
        transition
      end.compact

      # Run the events in parallel only if valid transitions were found for
      # all of them
      if events.length == transitions.length
        TransitionCollection.new(transitions, { use_transactions: resolve_use_transactions, actions: run_action }).perform
      else
        false
      end
    end

    # Builds the collection of transitions for all event attributes defined on
    # the given object.  This will only include events whose machine actions
    # match the one specified.
    #
    # These should only be fired as a result of the action being run.
    def transitions(object, action, options = {})
      transitions = map do |_name, machine|
        machine.events.attribute_transition_for(object, true) if machine.action == action
      end

      AttributeTransitionCollection.new(transitions.compact, { use_transactions: resolve_use_transactions }.merge(options))
    end

    protected

    def resolve_use_transactions
      use_transactions = nil
      each_value do |machine|
        # Determine use_transactions setting for this set of transitions.  If from multiple state_machines, the settings must match.
        raise 'Encountered mismatched use_transactions configurations for multiple state_machines' if !use_transactions.nil? && use_transactions != machine.use_transactions

        use_transactions = machine.use_transactions
      end
      use_transactions
    end
  end
end
