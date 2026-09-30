require 'concurrent/synchronization/lockable_object'
require 'concurrent/utility/native_integer'

module Concurrent

  # A reusable synchronization aid, similar to {CyclicBarrier} and
  # `java.util.concurrent.Phaser`, in which the number of parties waiting for
  # a phase to complete may change over time.
  #
  # A `Phaser` may be created with an initial number of registered parties or
  # with none at all. Parties can join later using {#register} and leave using
  # {#arrive_and_deregister} (which deregisters the arriving party), so unlike
  # a {CyclicBarrier} the number of parties is not fixed for the lifetime of
  # the object.
  #
  # A generation of a phaser is called a *phase*. Phase numbers start at zero
  # and advance by one whenever all currently registered parties have arrived.
  # An arrival token is the number of the phase in which the arrival happened,
  # it can be passed to {#await_advance} to wait for that phase to complete.
  #
  # @example fixed number of parties, reusable across phases
  #   phaser = Concurrent::Phaser.new(3)
  #   threads = 3.times.map do
  #     Thread.new do
  #       2.times do
  #         # do the work of the current phase ...
  #         phaser.arrive_and_await_advance
  #       end
  #     end
  #   end
  #   threads.each(&:join)
  #
  # @example parties joining and leaving while the phaser is running
  #   phaser = Concurrent::Phaser.new(1)
  #
  #   worker = Thread.new do
  #     phaser.arrive_and_deregister # will not join the next phase
  #   end
  #
  #   phase = phaser.register # a new party joins the current phase
  #   phaser.await_advance(phase)
  #
  # @!macro internal_implementation_note
  class Phaser < Synchronization::LockableObject

    # Create a new `Phaser`.
    #
    # @param [Fixnum] parties the number of parties initially registered, may
    #   be zero
    # @yield optional phase action executed by the party which triggers the
    #   phase advance, between the phase tripping and its rollover; waiting
    #   parties are released after the action finishes
    # @yieldparam [Fixnum] phase the number of the phase that just completed
    # @yieldparam [Fixnum] registered_parties the number of parties registered
    #   for the next phase
    # @yieldreturn [Boolean] a truthy result terminates the phaser, a falsey
    #   result lets it advance to the next phase; without a block the phaser
    #   terminates automatically once all parties have deregistered
    #
    # @raise [ArgumentError] if `parties` is not an integer or is negative
    def initialize(parties = 0, &block)
      Utility::NativeInteger.ensure_integer_and_bounds parties
      Utility::NativeInteger.ensure_positive parties

      super(&nil)
      synchronize { ns_initialize parties, &block }
    end

    # @return [Fixnum] the current phase number, frozen at the number of the
    #   phase in which the phaser was terminated
    def phase
      synchronize { @phase }
    end

    # @return [Fixnum] the number of parties registered for the current phase
    def registered_parties
      synchronize { @registered }
    end

    # @return [Fixnum] the number of parties which have not yet arrived in the
    #   current phase (zero while a tripped phase is running its action)
    def unarrived_parties
      synchronize { @unarrived }
    end

    # @return [Fixnum] the number of parties which have already arrived in the
    #   current phase
    def arrived_parties
      synchronize { @registered - @unarrived }
    end

    # @return [Boolean] `true` if the phaser has been terminated
    def terminated?
      synchronize { @terminated }
    end

    # Adds a new unarrived party to this phaser.
    #
    # The generation to which the party belongs is decided atomically at the
    # phase advancement boundary:
    # - if the current phase is still open (at least one registered party has
    #   not arrived yet) the party belongs to the current phase and has to
    #   arrive for the phase to complete;
    # - if every party has already arrived and the phase action / rollover is
    #   in progress, the party is added to the next phase instead.
    #
    # @return [Fixnum] the phase number to which the party was registered
    #   (the next phase number when the current one has tripped but not
    #   advanced yet); the frozen phase number when the phaser is already
    #   terminated (no party is added in that case)
    def register
      bulk_register 1
    end

    # Adds `parties` new unarrived parties to this phaser, see {#register}.
    #
    # @param [Fixnum] parties the number of parties to add
    # @return [Fixnum] the phase number to which the parties were registered
    # @raise [ArgumentError] if `parties` is not an integer or is less than one
    def bulk_register(parties)
      Utility::NativeInteger.ensure_integer_and_bounds parties
      Utility::NativeInteger.ensure_positive_and_no_zero parties

      synchronize do
        return @phase if @terminated

        unless @started
          @started    = true
          @registered = @unarrived = parties
          return @phase
        end

        tripped = @unarrived.zero? && @registered > 0
        @registered += parties
        # while a phase is tripped the new party waits for the rollover with
        # everyone else, otherwise it has to arrive for the current phase
        @unarrived += parties unless tripped
        tripped ? @phase + 1 : @phase
      end
    end

    # Arrives at the phaser without waiting for the other parties.
    #
    # @return [Fixnum] the phase number to which the arrival applied
    # @raise [Concurrent::IllegalOperationError] if the phaser is terminated or
    #   if no party is registered
    def arrive
      do_arrive false
    end

    # Arrives at the phaser and deregisters from it, the party will not take
    # part in future phases.
    #
    # Deregistering the last registered party terminates the phaser and all
    # waiting threads are released with the terminal state.
    #
    # @return [Fixnum] the phase number to which the arrival applied
    # @raise [Concurrent::IllegalOperationError] if the phaser is terminated or
    #   if no party is registered
    def arrive_and_deregister
      do_arrive true
    end

    # Arrives at the phaser and waits for the arrival phase to complete.
    #
    # @param [Fixnum] timeout the number of seconds to wait or `nil` to block
    #   indefinitely
    # @return [Fixnum, Boolean, nil] the number of the phase which completed,
    #   `false` on timeout, `nil` if the phaser was terminated while waiting
    # @raise [Concurrent::IllegalOperationError] if the phaser is terminated or
    #   if no party is registered
    def arrive_and_await_advance(timeout = nil)
      arrival_phase = do_arrive false
      await_advance arrival_phase, timeout
    end

    # Waits for the given phase to complete without arriving.
    #
    # This method never changes any counter, in particular it does not arrive
    # for the caller. Passing a token of an already completed phase (an "old"
    # phase) returns immediately and can never decrement the unarrived count of
    # the current phase.
    #
    # @param [Fixnum] arrival_phase the phase to wait for, typically the return
    #   value of {#arrive} or {#register}
    # @param [Fixnum] timeout the number of seconds to wait or `nil` to block
    #   indefinitely
    # @return [Fixnum, Boolean, nil] the current phase number if the awaited
    #   phase has already completed (including tokens of future phases),
    #   `false` on timeout, `nil` if the phaser is terminated
    def await_advance(arrival_phase, timeout = nil)
      Utility::NativeInteger.ensure_integer_and_bounds arrival_phase

      synchronize do
        return nil if @terminated
        return @phase if @phase != arrival_phase

        advanced = ns_wait_until(timeout) { @phase != arrival_phase || @terminated }
        return false unless advanced

        @terminated ? nil : @phase
      end
    end

    # Forces the phaser into the terminated state. All waiting threads are
    # released and receive the terminal state. Registered parties are
    # preserved, but no further arrivals or registrations are accepted.
    #
    # @return [Fixnum] the frozen phase number in which the phaser terminated
    def force_termination
      synchronize do
        ns_terminate
        @phase
      end
    end

    protected

    # The phase action, executed by the arriving party which trips the phase,
    # after the phase has tripped and before it advances.
    #
    # Subclasses can override this method instead of passing a block.
    #
    # @param [Fixnum] phase the phase which just completed
    # @param [Fixnum] registered_parties the number of parties taking part in
    #   the next phase
    # @return [Boolean] a truthy result terminates the phaser
    def on_advance(phase, registered_parties)
      if @action
        @action.call phase, registered_parties
      else
        registered_parties == 0
      end
    end

    private

    def ns_initialize(parties, &block)
      @phase      = 0
      @registered = @unarrived = parties
      @started    = parties > 0
      @terminated = false
      @action     = block
    end

    def do_arrive(deregister)
      arrival_phase = nil
      tripped = false
      parties_after_arrival = nil

      synchronize do
        if @terminated
          raise IllegalOperationError, 'cannot arrive at a terminated phaser'
        elsif !@started || @registered.zero?
          raise IllegalOperationError, 'no party is registered on the phaser'
        end

        # while the tripping party runs the phase action every other arrival
        # waits for the rollover instead of touching the new generation
        if @unarrived.zero?
          ns_wait_until { @unarrived > 0 || @terminated }
          raise IllegalOperationError, 'cannot arrive at a terminated phaser' if @terminated
        end

        arrival_phase = @phase
        @unarrived -= 1
        @registered -= 1 if deregister
        parties_after_arrival = @registered
        tripped = @unarrived.zero?
      end

      if tripped
        begin
          terminate_phaser = on_advance arrival_phase, parties_after_arrival
        rescue StandardError
          synchronize { ns_terminate }
          raise
        end

        synchronize do
          if terminate_phaser
            ns_terminate
          else
            @phase += 1
            @unarrived = @registered
            ns_broadcast
          end
        end
      elsif deregister && parties_after_arrival.zero?
        synchronize { ns_terminate }
      end

      arrival_phase
    end

    def ns_terminate
      @terminated = true
      @unarrived  = 0
      ns_broadcast
    end
  end
end
