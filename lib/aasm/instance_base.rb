module AASM
  class InstanceBase
    attr_accessor :from_state, :to_state, :current_event

    def initialize(instance, name=:default) # instance of the class including AASM, name of the state machine
      @instance = instance
      @name = name
    end

    def current_state
      @instance.aasm_read_state(@name)
    end

    def current_state=(state)
      @instance.aasm_write_state_without_persistence(state, @name)
      arm_state_timeout(state)
    end

    def enter_initial_state
      state_name = determine_state_name(@instance.class.aasm(@name).initial_state)
      state_object = state_object_for_name(state_name)

      state_object.fire_callbacks(:before_enter, @instance)
      self.current_state = state_name
      state_object.fire_callbacks(:after_enter, @instance)

      state_name
    end

    def human_state
      state_object_for_name(current_state).display_name
    end

    def states(options={}, *args)
      if options.has_key?(:permitted)
        selected_events = events({:permitted => options[:permitted]}, *args)
        # An array of arrays. Each inner array represents the transitions that
        # transition from the current state for an event
        event_transitions = selected_events.map {|e| e.transitions_from_state(current_state) }

        # An array of :to transition states
        to_state_names = event_transitions.map do |transitions|
          return nil if transitions.empty?

          # Return the :to state of the first transition that is allowed (or not) or nil
          if options[:permitted]
            transition = transitions.find { |t| t.allowed?(@instance, *args) }
          else
            transition = transitions.find { |t| !t.allowed?(@instance, *args) }
          end
          transition ? transition.to : nil
        end.flatten.compact.uniq

        # Select states that are in to_state_names
        @instance.class.aasm(@name).states.select {|s| to_state_names.include?(s.name)}
      else
        @instance.class.aasm(@name).states
      end
    end

    def events(options={}, *args)
      state = options[:state] || current_state
      events = @instance.class.aasm(@name).events.select {|e| e.transitions_from_state?(state) }

      options[:reject] = Array(options[:reject])
      events.reject! { |e| options[:reject].include?(e.name) }

      if options.has_key?(:permitted)
        # filters the results of events_for_current_state so that only those that
        # are really currently possible (given transition guards) are shown.
        if options[:permitted]
          events.select! { |e| @instance.send("may_#{event_method_name(e.name)}?", *args) }
        else
          events.select! { |e| !@instance.send("may_#{event_method_name(e.name)}?", *args) }
        end
      end

      events
    end

    def permitted_transitions
      events(permitted: true).flat_map do |event|
        available_transitions = event.transitions_from_state(current_state)
        allowed_transitions = available_transitions.select { |t| t.allowed?(@instance) }

        allowed_transitions.map do |transition|
          { event: event.name, state: transition.to }
        end
      end
    end

    def state_object_for_name(name)
      obj = @instance.class.aasm(@name).states.find {|s| s.name == name}
      raise AASM::UndefinedState, "State :#{name} doesn't exist" if obj.nil?
      obj
    end

    def determine_state_name(state)
      case state
        when Symbol, String
          state
        when Proc
          state.call(@instance)
        else
          raise NotImplementedError, "Unrecognized state-type given. Expected Symbol, String, or Proc."
      end
    end

    def may_fire_event?(name, *args)
      if event = @instance.class.aasm(@name).state_machine.events[name]
        !!event.may_fire?(@instance, *args)
      else
        false # unknown event
      end
    end

    # Caller-driven timeout advancement. The library never creates threads
    # or timers; the caller passes the current monotonic time (in the same
    # domain as the state machine's timeout clock).
    #
    # One call processes only the record that was already expired when the
    # call started. If firing the expire event enters another state whose
    # deadline is already due (e.g. a zero wait), that new record is left
    # for the next call, so advancement never spins.
    #
    # Returns an array of AASM::TimeoutResult (empty when nothing was due).
    # A rejected or raised expire event keeps its record armed, so the
    # caller can explicitly retry (call again) or cancel (cancel_timeout).
    def advance_timeouts(now = timeout_now)
      record = current_timeout
      return [] if record.nil? || !record.expired?(now)

      result =
        begin
          if fire(record.event)
            AASM::TimeoutResult.new(state_machine: @name, state: record.state,
                                    event: record.event, status: :fired)
          else
            AASM::TimeoutResult.new(state_machine: @name, state: record.state,
                                    event: record.event, status: :rejected)
          end
        rescue AASM::InvalidTransition => e
          AASM::TimeoutResult.new(state_machine: @name, state: record.state,
                                  event: record.event, status: :rejected, exception: e)
        rescue StandardError => e
          AASM::TimeoutResult.new(state_machine: @name, state: record.state,
                                  event: record.event, status: :error, exception: e)
        end
      [result]
    end

    # the timeout record armed for the current stay in the current state,
    # or nil when the current state declares no timeout
    def current_timeout
      record = @timeout_record
      return nil if record.nil? || record.state != current_state
      record
    end

    # seconds until the current stay expires; 0 when already overdue, nil
    # when no timeout is armed. Pure query: never fires any event.
    def timeout_remaining(now = timeout_now)
      record = current_timeout
      return nil if record.nil?
      remaining = record.deadline - now
      remaining.negative? ? 0 : remaining
    end

    # pure query: is the armed record due at the given time?
    def timeout_expired?(now = timeout_now)
      record = current_timeout
      !record.nil? && record.expired?(now)
    end

    # explicitly cancel the armed timeout without firing any event;
    # returns the cancelled record (or nil when nothing was armed)
    def cancel_timeout
      record = @timeout_record
      @timeout_record = nil
      record
    end

    # the current monotonic time of this state machine's timeout clock
    def timeout_now
      clock = @instance.class.aasm(@name).state_machine.config.timeout_clock
      clock ? clock.call : Process.clock_gettime(Process::CLOCK_MONOTONIC)
    end

    def fire(event_name, *args, &block)
      event_exists?(event_name)
      @instance.send(event_method_name(event_name), *args, &block)
    end

    def fire!(event_name, *args, &block)
      event_exists?(event_name, true)
      bang_event_name = "#{event_method_name(event_name)}!".to_sym
      @instance.send(bang_event_name, *args, &block)
    end

    def set_current_state_with_persistence(state)
      save_success = @instance.aasm_write_state(state, @name)
      self.current_state = state if save_success
      save_success
    end

    private

    # Arms a fresh deadline for a successful entry into +state+. Any record
    # of a previous stay is discarded: leaving a state voids its deadline,
    # and re-entering the same state starts a new one. Entering a state
    # without a timeout declaration clears the record.
    def arm_state_timeout(state)
      state_object = @instance.class.aasm(@name).states.find {|s| s.name == state}
      if state_object && state_object.expires_after
        now = timeout_now
        @timeout_record = AASM::TimeoutRecord.new(
          state: state_object.name,
          event: state_object.expire_event,
          armed_at: now,
          deadline: now + timeout_duration(state_object)
        )
      else
        @timeout_record = nil
      end
    end

    def timeout_duration(state_object)
      duration = state_object.expires_after
      case duration
      when Numeric then duration
      when Symbol then @instance.send(duration)
      when Proc then @instance.instance_exec(&duration)
      else raise ArgumentError, "invalid :expires_after value #{duration.inspect} for state :#{state_object.name}"
      end
    end

    def event_method_name(event_name)
      config = @instance.class.aasm(@name).state_machine.config
      if config.namespace
        ns = (config.namespace == true) ? @name : config.namespace
        "#{event_name}_#{ns}"
      else
        event_name.to_s
      end
    end

    def event_exists?(event_name, bang = false)
      event = @instance.class.aasm(@name).state_machine.events[event_name.to_sym]
      return true if event

      event_error = bang ? "#{event_name}!" : event_name
      raise AASM::UndefinedEvent, "Event :#{event_error} doesn't exist" if event.nil?
    end
  end
end
