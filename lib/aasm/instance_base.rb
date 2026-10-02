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
    end

    def enter_initial_state
      state_name = determine_state_name(@instance.class.aasm(@name).initial_state)
      state_object = state_object_for_name(state_name)

      state_object.fire_callbacks(:before_enter, @instance)
      self.current_state = state_name
      state_object.fire_callbacks(:after_enter, @instance)

      state_entered(state_name)

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

    def fire(event_name, *args, **kwargs, &block)
      event_exists?(event_name)
      @instance.send(event_method_name(event_name), *args, **kwargs, &block)
    end

    def fire!(event_name, *args, **kwargs, &block)
      event_exists?(event_name, true)
      bang_event_name = "#{event_method_name(event_name)}!".to_sym
      @instance.send(bang_event_name, *args, **kwargs, &block)
    end

    def set_current_state_with_persistence(state)
      save_success = @instance.aasm_write_state(state, @name)
      self.current_state = state if save_success
      save_success
    end

    # --- state timeouts -------------------------------------------------

    # Monotonic schedule holding the deadline of the state currently occupied
    # by this state machine instance.
    def timeout_schedule
      @timeout_schedule ||= AASM::Core::TimeoutSchedule.new
    end

    # Hook invoked exactly once for every successful state entry, including
    # the initial state. The deadline is a property of this particular entry:
    # entering a state voids the deadline of the state being left (also for
    # self-transitions), and re-entering a state arms a brand new deadline with
    # a fresh token.
    def state_entered(state_name, _previous_state_name = nil)
      new_state = state_object_for_name(state_name)
      timeout_schedule.clear
      return unless new_state.timeout?

      timeout_schedule.arm(
        state_name,
        monotonic_now,
        new_state.timeout,
        new_state.timeout_event
      )
    end

    # Advances the state machine against the caller supplied monotonic time.
    #
    # Only deadlines already due at +now+ when the call starts are handled;
    # deadlines armed by the resulting transition (e.g. a zero-wait target
    # state) are left for the next advance, so this never spins.
    #
    # Returns a single AASM::TimeoutResult (or one per due entry as an array in
    # the general case). Reading time never triggers events; this is the only
    # entry point that fires a timeout event.
    def time_out(now = monotonic_now, *args, **kwargs)
      # Entering the initial state for the first time arms its deadline using
      # the object's own monotonic clock. Do this before taking the due
      # snapshot, so an (even zero-wait) initial deadline is only due on a
      # later advance rather than firing immediately.
      ensure_initial_state_entered
      due_entries = timeout_schedule.due(now)
      return AASM::TimeoutResult.new(status: :none) if due_entries.empty?

      results = due_entries.map { |entry| handle_due_entry(entry, now, *args, **kwargs) }
      results.size == 1 ? results.first : results
    end
    alias_method :advance_timeouts, :time_out

    # Explicitly retries the outstanding deadline after a previous rejection or
    # error. Does nothing implicitly: a failed deadline is never re-fired just
    # because time is read or advanced again.
    def retry_timeout(now = monotonic_now, *args, **kwargs)
      ensure_initial_state_entered
      entry = timeout_schedule.entry
      return AASM::TimeoutResult.new(status: :none) unless live?(entry)

      unless entry.due?(now)
        raise AASM::InvalidTimeoutOperation,
              "cannot retry timeout for state ':#{entry.state}' before its deadline"
      end

      handle_due_entry(entry, now, *args, **kwargs)
    end

    # Voids the outstanding deadline without leaving the current state.
    def cancel_timeout
      ensure_initial_state_entered
      entry = timeout_schedule.entry
      return AASM::TimeoutResult.new(status: :none) unless live?(entry)

      timeout_schedule.disarm(entry.token)
      AASM::TimeoutResult.new(
        status: :cancelled,
        state: entry.state,
        event: entry.event,
        from_state: current_state,
        token: entry.token
      )
    end

    # Remaining monotonic time of the current deadline, floored at zero, or nil
    # when the current state has no outstanding deadline. Purely observational:
    # it never fires a timeout event.
    def time_remaining(now = monotonic_now)
      ensure_initial_state_entered
      entry = timeout_schedule.entry
      return nil unless live?(entry)

      entry.remaining(now)
    end

    # Whether the current deadline is due at +now+. Purely observational.
    def timeouts_due?(now = monotonic_now)
      ensure_initial_state_entered
      entry = timeout_schedule.entry
      !!(entry && live?(entry) && entry.due?(now))
    end

    private

    def ensure_initial_state_entered
      current_state
    end

    def monotonic_now
      AASM::Core::MonotonicClock.now_or(@instance.aasm_monotonic_now)
    end

    def live?(entry)
      !!entry && current_state == entry.state
    end

    def handle_due_entry(entry, now, *args, **kwargs)
      from_state = current_state

      # Defensive: only a deadline belonging to the state currently occupied
      # can drive an event. A stale deadline is voided silently.
      unless live?(entry)
        timeout_schedule.disarm(entry.token)
        return AASM::TimeoutResult.new(status: :none)
      end

      unless @instance.class.aasm(@name).state_machine.events[entry.event]
        error = AASM::InvalidTimeoutOperation.new(
          "Event ':#{entry.event}' declared as the timeout event for state ':#{entry.state}' doesn't exist"
        )
        return AASM::TimeoutResult.new(
          status: :error,
          state: entry.state,
          event: entry.event,
          from_state: from_state,
          token: entry.token,
          error: error
        )
      end

      fired, error = AASM::Core::MonotonicClock.with_now(now) do
        begin
          [fire(entry.event, *args, **kwargs), nil]
        rescue AASM::InvalidTransition => e
          # No allowed transition (guards rejected or no transition from this
          # state). This is a rejection, never an unexpected error.
          [false, e]
        rescue StandardError => e
          [nil, e]
        end
      end

      build_result(entry, from_state, fired, error)
    end

    def build_result(entry, from_state, fired, error)
      status =
        if error.is_a?(AASM::InvalidTransition)
          :rejected
        elsif error.nil? && fired
          :fired
        elsif error
          :error
        else
          :rejected
        end

      AASM::TimeoutResult.new(
        status: status,
        state: entry.state,
        event: entry.event,
        from_state: from_state,
        to_state: current_state,
        token: entry.token,
        error: error
      )
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
