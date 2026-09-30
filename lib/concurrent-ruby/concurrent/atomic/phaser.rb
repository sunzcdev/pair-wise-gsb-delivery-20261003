require 'concurrent/synchronization/lockable_object'
require 'concurrent/utility/native_integer'

module Concurrent

  # A reusable synchronization primitive, similar in spirit to a
  # {CyclicBarrier} but with a <em>dynamically sized set of parties</em>.
  #
  # Where a {CyclicBarrier} is created for a fixed number of parties, a
  # +Phaser+ allows parties to join (register) and leave (deregister) at any
  # time. A batch of work which converges is called a {#phase phase} (a
  # generation); whenever every registered party of the current phase has
  # arrived the phaser advances to the next phase, optionally running a phase
  # action, and all threads waiting for that phase are released.
  #
  # A phaser can also be {#terminate force terminated}, after which every
  # blocked thread is released with the terminal phase and no further
  # registrations or arrivals are accepted.
  #
  # == Attribution of an operation to a phase
  #
  # The boundary between two phases is a critical region:
  #
  # - When a party {#arrive arrives} it is always attributed to <em>one</em>
  #   phase. The arrival that completes a phase does not also count in the
  #   following phase; every party has to arrive again in the new phase.
  # - A {#register registration} or {#arrive arrival} issued while the phaser
  #   is advancing to the next phase blocks until the boundary completes and is
  #   then attributed to the <em>new</em> phase. It can never be lost or
  #   applied to the phase that just ended.
  # - {#arrive} and {#arrive_and_deregister} return the number of the phase to
  #   which the arrival was attributed. That number is a credential that can be
  #   handed to {#await_advance}.
  # - {#await_advance} only waits. Passing the credential of a phase that has
  #   already finished (an old credential) returns immediately and never
  #   decrements or otherwise distorts the arrival count of the current phase.
  #
  # These rules mean a stray/duplicate arrival belonging to an old phase, or an
  # old credential reused against a later phase, can never wrongly reduce the
  # unarrived count of the current phase.
  #
  # @example waiting for a variable number of jobs across several rounds
  #   phaser = Concurrent::Phaser.new(1) # the coordinator is registered
  #
  #   workers = 4.times.map do
  #     phaser.register                 # each worker joins before a round
  #     Thread.new do
  #       3.times do
  #         do_work
  #         phaser.arrive_and_wait      # arrive then wait for the round to end
  #       end
  #       phaser.arrive_and_deregister  # leave after the last round
  #     end
  #   end
  #
  #   3.times { phaser.arrive_and_wait } # coordinator drives each round
  #   phaser.arrive_and_deregister       # coordinator leaves; nobody is left
  #   workers.each(&:join)
  #   phaser.terminated?                 # => true (default action terminated it)
  class Phaser < Synchronization::LockableObject

    # Create a new +Phaser+.
    #
    # @param [Integer] parties the number of parties initially registered
    # @yield optional phase action run by the thread that advances the phaser,
    #   after the last party of a phase has arrived but before waiting threads
    #   are released. It is called with the number of the phase that just ended
    #   and the number of registered parties carried into the next phase. When
    #   the block returns a truthy value the phaser is terminated; when it
    #   returns a falsy value the phaser continues. When no block is given the
    #   phaser terminates automatically once the number of registered parties
    #   drops to zero.
    #
    # @raise [ArgumentError] if +parties+ is not a non-negative integer
    def initialize(parties = 0, &action)
      Utility::NativeInteger.ensure_integer_and_bounds parties
      Utility::NativeInteger.ensure_positive parties

      super(&nil)
      synchronize { ns_initialize parties, &action }
    end

    # Add parties to this phaser.
    #
    # If the phaser is currently advancing to the next phase the caller waits
    # until the boundary completes and is then registered into the new phase.
    #
    # @param [Integer] parties the number of parties to add
    # @return [Integer] the phase number the registered parties belong to; a
    #   negative value if the phaser is (or becomes while waiting) terminated
    #
    # @raise [ArgumentError] if +parties+ is not a positive integer
    def register(parties = 1)
      Utility::NativeInteger.ensure_integer_and_bounds parties
      Utility::NativeInteger.ensure_positive_and_no_zero parties

      synchronize do
        ns_wait_for_boundary
        return ns_negative_phase if @terminated

        @registered += parties
        @unarrived  += parties
        @phase
      end
    end

    # Indicate that the current party has arrived at this phase. Does not wait.
    #
    # The arrival is attributed to exactly one phase; the arrival that
    # completes a phase does not count in the following phase.
    #
    # @return [Integer] the phase number to which this arrival was attributed
    #   (a credential suitable for {#await_advance}); a negative value if the
    #   phaser is terminated
    def arrive
      do_arrive(1, false)
    end

    # Indicate arrival at this phase and deregister the party from future
    # phases.
    #
    # If this was the last registered party the phase (and the phaser) is
    # completed. Does not wait.
    #
    # @return [Integer] the phase number to which this arrival was attributed;
    #   a negative value if the phaser is terminated
    def arrive_and_deregister
      do_arrive(1, true)
    end

    # Arrive at this phase and block until the phaser advances past it (or is
    # terminated or the optional timeout expires).
    #
    # @param [Numeric, nil] timeout maximum seconds to wait, +nil+ to wait
    #   indefinitely
    # @return [Integer] the number of the phase reached after waiting (the
    #   terminal phase number if terminated); +nil+ if the wait timed out
    def arrive_and_wait(timeout = nil)
      phase = do_arrive(1, false)
      await_advance(phase, timeout)
    end

    # Block until the phaser advances past the given phase or is terminated.
    #
    # Does not arrive and never changes the arrival count. If +phase+ belongs
    # to a phase that has already finished (or is older than the current
    # phase) the method returns immediately; an old credential therefore
    # cannot disturb the current phase.
    #
    # @param [Integer] phase the credential returned by an arrival
    # @param [Numeric, nil] timeout maximum seconds to wait, +nil+ to wait
    #   indefinitely
    # @return [Integer, nil] the phase number reached after waiting (the
    #   terminal phase number if terminated); +nil+ if the wait timed out
    def await_advance(phase, timeout = nil)
      synchronize do
        advanced = ns_wait_for_advance(phase, timeout)
        return nil if timeout && !advanced

        @terminated ? ns_negative_phase : @phase
      end
    end

    # Force the phaser into its terminated state and release every waiting
    # thread.
    #
    # After termination all arrivals and registrations return a negative phase
    # without changing any count, and {#await_advance} returns immediately.
    #
    # @return [nil]
    def terminate
      synchronize do
        unless @terminated
          @terminated = true
          @advancing  = false
          ns_broadcast
        end
      end
      nil
    end

    # @return [Integer] the current phase number; the terminal phase number if
    #   the phaser is terminated
    def phase
      synchronize { @phase }
    end

    # @return [Integer] the number of parties currently registered
    def registered_parties
      synchronize { @registered }
    end

    # @return [Integer] the number of registered parties that have not yet
    #   arrived at the current phase
    def unarrived_parties
      synchronize { @unarrived }
    end

    # @return [Integer] the number of parties that have arrived at the current
    #   phase
    def arrived_parties
      synchronize { @registered - @unarrived }
    end

    # @return [Integer] the number of threads currently blocked in this phaser
    def number_waiting
      synchronize { @waiting }
    end

    # @return [Boolean] +true+ if the phaser has been terminated
    def terminated?
      synchronize { @terminated }
    end

    protected

    def ns_initialize(parties, &action)
      @action       = action
      @action_error = nil
      @phase        = 0
      @registered   = parties
      @unarrived    = parties
      @terminated   = false
      @advancing    = false
      @waiting      = 0
      self
    end

    # Block while a phase boundary is being committed. Used inside a
    # synchronized block.
    def ns_wait_for_boundary
      while @advancing
        @waiting += 1
        ns_wait_until { !@advancing || @terminated }
        @waiting -= 1
      end
    end

    # Shared implementation of arrive / arrive_and_deregister. It detects a
    # completed phase while holding the lock, runs the (external) phase action
    # with the lock released so the boundary remains observable, then
    # re-acquires the lock to commit the boundary.
    def do_arrive(parties, deregister)
      trip = synchronize do
        ns_wait_for_boundary
        next :terminated if @terminated

        if @unarrived < parties
          raise IllegalOperationError,
                'cannot arrive when there are no unarrived parties in this phase'
        end

        @arriving_phase = @phase
        @unarrived     -= parties
        @registered    -= parties if deregister

        next nil unless @unarrived.zero?

        # Mark the boundary and remember who is carried into the next phase,
        # then leave the lock to run the external action.
        @advancing = true
        @registered
      end

      return ns_negative_phase if trip == :terminated
      return @arriving_phase if trip.nil?

      # The lock is released here: other registrations/arrivals observe the
      # advancing boundary and wait, while inspectors remain readable.
      should_terminate = run_action(@arriving_phase, trip)

      synchronize { finish_advance(should_terminate) }
      @arriving_phase
    end

    # Runs the phase action. Called while the boundary is marked as advancing
    # and with the lock released, so other threads can observe the boundary and
    # the action may safely call back into the phaser. Must be followed by
    # #finish_advance (which re-synchronizes).
    def run_action(arriving_phase, next_registered)
      action = @action
      if action
        begin
          result        = action.call(arriving_phase, next_registered)
          @action_error = nil
          !!result
        rescue => error
          @action_error = error
          true
        end
      else
        next_registered.zero?
      end
    end

    # Commits the boundary: advances to the next phase (or terminates), resets
    # the per-phase counter and releases every waiting thread.
    def finish_advance(should_terminate)
      if should_terminate
        @terminated = true
      else
        @phase    += 1
        @unarrived = @registered
      end
      @advancing = false
      ns_broadcast

      raise @action_error if @action_error
    end

    # Block while the given phase has not finished and the phaser is not
    # terminated. Used inside a synchronized block.
    #
    # @return [Boolean] +true+ if the phase finished (or the phaser
    #   terminated), +false+ if the timeout elapsed first
    def ns_wait_for_advance(phase, timeout)
      return true if phase.nil? || phase < 0

      if timeout
        deadline = Concurrent.monotonic_time + timeout
        until @terminated || @phase > phase
          remaining = deadline - Concurrent.monotonic_time
          break if remaining <= 0

          @waiting += 1
          ns_wait(remaining)
          @waiting -= 1
        end
        @terminated || @phase > phase
      else
        until @terminated || @phase > phase
          @waiting += 1
          ns_wait_until { @terminated || @phase > phase }
          @waiting -= 1
        end
        true
      end
    end

    # Convention borrowed from java.util.concurrent.Phaser: a negative value
    # denotes the terminated state.
    def ns_negative_phase
      -(@phase + 1)
    end
  end
end
