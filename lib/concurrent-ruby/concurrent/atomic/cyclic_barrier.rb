require 'concurrent/synchronization/lockable_object'
require 'concurrent/utility/native_integer'

module Concurrent

  # A synchronization aid that allows a set of threads to all wait for each
  # other to reach a common barrier point.
  # @example
  #   barrier = Concurrent::CyclicBarrier.new(3)
  #   jobs    = Array.new(3) { |i| -> { sleep i; p done: i } }
  #   process = -> (i) do
  #     # waiting to start at the same time
  #     barrier.wait
  #     # execute job
  #     jobs[i].call
  #     # wait for others to finish
  #     barrier.wait
  #   end
  #   threads = 2.times.map do |i|
  #     Thread.new(i, &process)
  #   end
  #
  #   # use main as well
  #   process.call 2
  #
  #   # here we can be sure that all jobs are processed
  class CyclicBarrier < Synchronization::LockableObject

    # A single trip of the barrier. Each call to {#reset} and every completed
    # trip starts a fresh generation with a monotonically increasing {#index}.
    # Waiting threads capture the generation they arrived in and only react to
    # its terminal status, so a wake-up belonging to an old generation can
    # never leak into a newer one.
    #
    # @!visibility private
    Generation = Struct.new(:index, :status)
    private_constant :Generation

    # Create a new `CyclicBarrier` that waits for `parties` threads
    #
    # @param [Fixnum] parties the number of parties
    # @yield an optional block that will be executed that will be executed after
    #  the last thread arrives and before the others are released
    #
    # @raise [ArgumentError] if `parties` is not an integer or is less than zero
    def initialize(parties, &block)
      Utility::NativeInteger.ensure_integer_and_bounds parties
      Utility::NativeInteger.ensure_positive_and_no_zero parties

      super(&nil)
      synchronize { ns_initialize parties, &block }
    end

    # @return [Fixnum] the number of threads needed to pass the barrier
    def parties
      synchronize { @parties }
    end

    # @return [Fixnum] the number of threads currently waiting on the barrier
    def number_waiting
      synchronize { @number_waiting }
    end

    # The identity of the barrier generation that is currently accepting waiters.
    #
    # A generation identifies one round (trip) of the barrier. It gets a new,
    # strictly increasing value whenever the barrier trips successfully or is
    # reset, and keeps its value while the current generation is broken.
    # Comparing the value observed before a wait with the one observed
    # afterwards makes the round a thread belonged to inspectable and
    # distinguishable from later rounds.
    #
    # @return [Fixnum] a non-negative identifier, increasing by one for every
    #   new generation
    def generation
      synchronize { @generation.index }
    end

    # Blocks on the barrier until the number of waiting threads is equal to
    # `parties` or until `timeout` is reached or `reset` is called
    # If a block has been passed to the constructor, it will be executed once by
    #  the last arrived thread before releasing the others
    # @param [Fixnum] timeout the number of seconds to wait for the counter or
    #  `nil` to block indefinitely. The timeout is measured against the
    #  monotonic clock, so it is unaffected by changes to the system clock.
    # @return [Boolean] `true` if the `count` reaches zero else false on
    #  `timeout` or on `reset` or if the barrier is broken
    # @raise [Exception] when the constructor block raises, the exception is
    #  re-raised in the single thread designated to run the action; every other
    #  waiter of the same generation observes the broken barrier and receives
    #  `false`
    def wait(timeout = nil)
      synchronize do
        generation = @generation

        # A latecomer can never join a generation that has already terminated,
        # regardless of whether it was fulfilled, reset or broken.
        return false unless generation.status == :waiting

        @number_waiting += 1

        if @number_waiting == @parties
          ns_trip_generation(generation)
        else
          ns_await_generation(generation, timeout)
        end
      end
    end

    # resets the barrier to its initial state
    # If there is at least one waiting thread, it will be woken up, the `wait`
    # method will return false and the barrier will be broken
    # If the barrier is broken, this method restores it to the original state
    #
    # Waiters of the previous generation are released against that generation;
    # they can never observe the freshly started generation, and threads
    # arriving after the reset only ever join the new generation.
    #
    # @return [nil]
    def reset
      synchronize { ns_finish_generation @generation, :reset }
    end

    # A barrier can be broken when:
    # - a thread called the `reset` method while at least one other thread was waiting
    # - at least one thread timed out on `wait` method
    # - the block passed to the constructor raised an exception
    #
    # A broken barrier can be restored using `reset` it's safer to create a new one
    # @return [Boolean] true if the barrier is broken otherwise false
    def broken?
      synchronize { @generation.status != :waiting }
    end

    protected

    # Last arriving party: run the barrier action exactly once, then fulfill the
    # generation and open the next one. If the action raises, the generation is
    # broken and all other waiters are released instead of being left blocked;
    # the action is never retried by another waiter, the exception propagates
    # only in the thread that was designated to run it.
    def ns_trip_generation(generation)
      begin
        @action.call if @action
      rescue Exception => error
        ns_break_generation(generation)
        raise error
      end

      ns_finish_generation generation, :fulfilled
      true
    end

    # Wait for the captured generation to terminate. A timeout breaks the whole
    # round so every other waiter observes the same failure.
    def ns_await_generation(generation, timeout)
      if ns_wait_until(timeout) { generation.status != :waiting }
        generation.status == :fulfilled
      else
        ns_break_generation(generation)
        false
      end
    end

    # Mark the current round as broken without opening a new generation: the
    # broken state stays visible to latecomers until an explicit {#reset}.
    def ns_break_generation(generation)
      return unless generation.status == :waiting
      generation.status = :broken
      @number_waiting = 0
      ns_broadcast
    end

    # Terminate a generation with a final status, wake every waiter and, unless
    # the round was broken, open the next generation.
    def ns_finish_generation(generation, status)
      generation.status = status
      ns_next_generation unless status == :broken
      ns_broadcast
    end

    def ns_next_generation
      @generation     = Generation.new(@generation.index + 1, :waiting)
      @number_waiting = 0
    end

    def ns_initialize(parties, &block)
      @parties = parties
      @action  = block
      @generation     = Generation.new(0, :waiting)
      @number_waiting = 0
    end
  end
end
