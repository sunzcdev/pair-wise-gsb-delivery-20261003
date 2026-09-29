# frozen_string_literal: true

require_relative 'options_validator'

module StateMachines
  # Represents an ordered sequence of events to be fired on the same object.
  #
  # Unlike StateMachines::TransitionCollection (which runs transitions for
  # *different* attributes in parallel), an event sequence fires each event
  # one after another on the *same* object, so each event sees the state left
  # behind by the previous event.  The configured machine action (the
  # persistence commit, e.g. +save+) is only invoked once *all* events have
  # transitioned successfully.  If any step fails, every machine's state is
  # rolled back to its original value and any event attributes that were
  # staged mid-sequence are restored.
  #
  # External side effects (e.g. records created in callbacks, API calls) are
  # *not* rolled back automatically.  Callers can provide their own
  # transaction boundary by passing a block to #perform:
  #
  #   sequence.perform do |run|
  #     ActiveRecord::Base.transaction { run.call }
  #   end
  #
  # == Failure attribution
  #
  # #perform returns a StateMachines::EventSequence::Result instead of a
  # plain boolean.  The result exposes which step failed and why:
  #
  #   result = vehicle.fire_event_sequence(:ignite, :shift_up, :park)
  #   result.success?        # => false
  #   result.failed_event    # => :shift_up
  #   result.failure_reason  # => :invalid_transition
  class EventSequence
    # A single event in the sequence, tracking its execution status
    class Step
      # The name of the event to fire
      attr_reader :event_name

      # The machine that owns the event (resolved when the sequence is built)
      attr_reader :machine

      # The event to fire for this step (resolved when the sequence is built)
      attr_reader :event

      # The transition that was built for this step (nil until performed)
      attr_reader :transition

      # Why this step failed (:invalid_transition or :callback_halted)
      attr_reader :failure_reason

      def initialize(event_name) # :nodoc:
        @event_name = event_name
        @status = :pending
      end

      # The current status: :pending, :success, :failed, or :rolled_back
      attr_reader :status

      def pending? # :nodoc:
        @status == :pending
      end

      # Did this step's transition run and get committed?
      def success?
        @status == :success
      end

      # Did this step cause the sequence to fail?
      def failed?
        @status == :failed
      end

      # Did this step run but get rolled back due to a later failure?
      def rolled_back?
        @status == :rolled_back
      end

      def resolve(machine, event) # :nodoc:
        @machine = machine
        @event = event
      end

      def build_transition(object) # :nodoc:
        @transition = @event.transition_for(object)
        @transition
      end

      def succeed! # :nodoc:
        @status = :success
      end

      def fail!(reason) # :nodoc:
        @status = :failed
        @failure_reason = reason
      end

      def roll_back! # :nodoc:
        @status = :rolled_back unless failed?
      end

      def inspect # :nodoc:
        "#<#{self.class} event=#{event_name.inspect} status=#{status}>"
      end
    end

    # The outcome of performing an event sequence.  Provides per-step failure
    # attribution instead of a plain boolean.
    class Result
      # The ordered steps, each with its own status / failure reason
      attr_reader :steps

      def initialize(sequence) # :nodoc:
        @sequence = sequence
        @steps = sequence.steps
      end

      # Did every event in the sequence fire and commit successfully?
      def success?
        @sequence.committed?
      end

      # The step that caused the failure, or nil if the sequence succeeded or
      # the commit action itself failed
      def failed_step
        steps.find(&:failed?)
      end

      # The name of the event whose step failed, or nil
      def failed_event
        failed_step&.event_name
      end

      # Why the sequence failed: :invalid_transition, :callback_halted, or
      # :action_failed (the commit action returned a falsey value).  Nil when
      # the sequence succeeded.
      def failure_reason
        @sequence.failure_reason
      end

      # The names of the events that were successfully committed
      def completed_events
        steps.select(&:success?).map(&:event_name)
      end
    end

    # The object being transitioned
    attr_reader :object

    # The ordered steps in this sequence
    attr_reader :steps

    # Why the sequence failed (nil when it succeeded)
    attr_reader :failure_reason

    # Creates a new sequence of events to be fired in order on the given
    # object.  Each event is resolved against the object's state machines;
    # unknown events raise StateMachines::InvalidEvent immediately.
    #
    # Configuration options:
    # * <tt>:actions</tt> - Whether to run the machine action(s) as the final
    #   commit.  Default is true.
    # * <tt>:use_transactions</tt> - Whether to wrap the sequence in the
    #   machine's transaction (integration-dependent).  Default is true.
    def initialize(object, events, options = {})
      StateMachines::OptionsValidator.assert_valid_keys!(options, :actions, :use_transactions)
      options = { actions: true, use_transactions: true }.merge(options)

      @object = object
      @skip_actions = !options[:actions]
      @use_transactions = options[:use_transactions]
      @committed = false
      @rolled_back = false

      @steps = events.map { |event_name| Step.new(event_name) }
      resolve_events
    end

    # Fires each event in order, committing the machine action(s) only if
    # every event transitions successfully.  Returns a Result describing the
    # outcome of each step.
    #
    # If a block is given, it receives a callable that runs the sequence;
    # the block is responsible for invoking it.  This allows callers to
    # provide their own transaction boundary around any external side
    # effects, which are never rolled back automatically:
    #
    #   sequence.perform { |run| ActiveRecord::Base.transaction { run.call } }
    #
    # Sequences cannot be nested: firing another sequence on the same object
    # from within a running sequence (e.g. in a transition callback) raises
    # StateMachines::InvalidContext.
    def perform
      if object.instance_variable_get(:@_state_machine_event_sequence)
        raise InvalidContext.new(object, 'Cannot fire an event sequence from within another event sequence on the same object')
      end

      object.instance_variable_set(:@_state_machine_event_sequence, self)
      begin
        machines.each { |machine| machine.reset(object) }
        snapshot_event_attributes

        run = -> { within_transaction { execute } }
        block_given? ? yield(run) : run.call
      ensure
        object.instance_variable_set(:@_state_machine_event_sequence, nil)
      end

      Result.new(self)
    end

    # Did every step succeed and the commit action complete?
    def committed?
      @committed
    end

    private

    # The machines involved in this sequence, in step order
    def machines
      @machines ||= steps.map(&:machine).uniq
    end

    # Resolves each event name to the machine / event that defines it
    def resolve_events
      state_machines = object.class.state_machines

      steps.each do |step|
        event = nil
        machine = nil
        state_machines.detect do |_name, candidate|
          machine = candidate if (event = candidate.events[step.event_name, :qualified_name])
        end

        raise(InvalidEvent.new(object, step.event_name)) unless event

        step.resolve(machine, event)
      end
    end

    # Runs each step in order, then rolls everything back unless the whole
    # sequence committed.  Exceptions always roll back and are re-raised.
    def execute
      @committed = run_step(0)
      rollback unless @committed
    rescue StandardError
      rollback
      raise
    end

    # Runs the step at the given index, nesting subsequent steps inside this
    # step's callbacks so that:
    # * each step's transition is built *after* the previous step persisted
    #   its state, and
    # * after callbacks only run once the commit action has completed.
    def run_step(index)
      step = steps[index]
      return commit unless step

      unless step.build_transition(object)
        step.event.on_failure(object)
        step.fail!(:invalid_transition)
        @failure_reason = :invalid_transition
        return false
      end

      inner_success = nil
      callbacks_completed = step.transition.run_callbacks(after: true) do
        step.transition.persist
        inner_success = run_step(index + 1)
        { result: inner_success, success: !!inner_success }
      end

      if callbacks_completed && inner_success
        step.succeed!
        true
      else
        unless callbacks_completed
          step.fail!(:callback_halted)
          @failure_reason = :callback_halted
        end
        false
      end
    end

    # Commits the sequence by invoking each involved machine's action exactly
    # once.  This is the only point at which state persistence is finalized;
    # a falsey result from any action fails the entire sequence.
    def commit
      return true if @skip_actions

      actions = machines.map(&:action).compact.uniq
      committed = actions.all? { |action| object.send(action) }
      @failure_reason = :action_failed unless committed
      committed
    end

    # Reverts every persisted transition (in reverse order) and restores any
    # event attributes that were staged while the sequence was running
    def rollback
      return if @rolled_back

      @rolled_back = true
      steps.reverse_each do |step|
        next unless step.transition

        step.transition.rollback
        step.roll_back!
      end
      restore_event_attributes
    end

    # Snapshots the event / event transition attributes of every machine on
    # the object so they can be restored if the sequence fails
    def snapshot_event_attributes
      @event_attribute_snapshot = object.class.state_machines.values.to_h do |machine|
        [machine, [machine.read(object, :event, true), machine.read(object, :event_transition, true)]]
      end
    end

    def restore_event_attributes
      @event_attribute_snapshot.each do |machine, (event, event_transition)|
        machine.write(object, :event, event, true)
        machine.write(object, :event_transition, event_transition, true)
      end
    end

    # Runs the block within the first involved machine's transaction.  If
    # transactions are disabled, this is a no-op.
    def within_transaction
      if @use_transactions && (machine = machines.first)
        machine.within_transaction(object) { yield }
      else
        yield
      end
    end
  end
end
