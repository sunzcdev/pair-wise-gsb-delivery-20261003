# frozen_string_literal: true

module Shoryuken
  module Helpers
    # Thread-safe bookkeeping for the finite pool of worker slots a processing group shares across its queues.
    #
    # A slot passes through three phases:
    #
    # 1. *reserved* - taken with {#reserve} before the SQS `receive_message` request is even sent, so two queues fetching
    #    at the same time can never reserve more slots than the group has workers.
    # 2. *leased* - a reservation is converted into a lease with {#occupy} for each message that actually came back and is
    #    handed to a worker. A lease occupies a slot until {#release} runs when the worker finishes.
    # 3. *returned* - reservations the server did not fill ({#release_reservation}, e.g. SQS returned fewer messages than
    #    asked for, or the fetch was cancelled) and finished worker leases ({#release}) become available again immediately,
    #    without waiting for a visibility timeout.
    #
    # All counters are guarded by one mutex because every operation is a short, allocation-light critical section; this
    # also keeps reservation and lease accounting consistent on JRuby, where the dispatch and worker threads truly run in
    # parallel.
    #
    # @example Two queues sharing three slots
    #   slots = Shoryuken::Helpers::WorkerSlots.new(3)
    #   slots.reserve(3) # => 3, asked for three before the first request goes out
    #   slots.available  # => 0, the second queue cannot reserve anything until some come back
    #   slots.release_reservation(2) # the server only returned one message
    #   slots.available  # => 2, free for the next fetch immediately
    class WorkerSlots
      # @return [Integer] the total number of slots in the pool
      attr_reader :capacity

      # @param capacity [Integer] the maximum number of concurrent workers (the group concurrency)
      def initialize(capacity)
        raise ArgumentError, 'capacity must be positive' unless capacity.positive?

        @mutex = Mutex.new
        @capacity = capacity
        @reserved = 0
        @leased = 0
      end

      # Reserves up to `requested` free slots for a fetch that is about to be sent.
      #
      # The reservation is made before the request so the `max_number_of_messages` sent to SQS never exceeds what the
      # group can currently process. Returns fewer than requested (possibly zero) when other in-flight fetches or running
      # workers already hold the rest.
      #
      # @param requested [Integer] the number of slots the caller would like to reserve
      # @return [Integer] the number actually reserved, between 0 and `requested`
      def reserve(requested)
        raise ArgumentError, 'requested must be non-negative' if requested.negative?

        @mutex.synchronize do
          taken = [requested, available_unlocked].min
          @reserved += taken
          taken
        end
      end

      # Occupies one slot with a worker lease, converting a reservation made before the fetch.
      #
      # Every leased slot stays occupied until {#release} runs, so a long-running worker keeps its place in the pool even
      # though its fetch has long finished. When called without a reservation (a direct assignment that did not go through
      # a fetch), a free slot is leased instead; the invariant `reserved + leased <= capacity` always holds.
      #
      # @return [Integer] the new total number of leased slots
      # @raise [RuntimeError] when no slot is available
      def occupy
        @mutex.synchronize do
          if @reserved.positive?
            @reserved -= 1
          elsif occupied_unlocked < @capacity
            # Leased directly, without a prior reservation (e.g. a batch assigned outside the fetch path).
          else
            raise 'cannot occupy a slot: none reserved and the pool is full'
          end

          @leased += 1
          @leased
        end
      end

      # Returns reserved slots that were never filled with a message to the pool.
      #
      # Use this when SQS answers with fewer messages than requested, or when a fetch is cancelled before its response is
      # consumed. The slots are reusable by the next fetch immediately.
      #
      # @param count [Integer] how many reserved slots to give back
      # @return [Integer] the number of slots still reserved
      def release_reservation(count)
        @mutex.synchronize do
          if count > @reserved
            raise "cannot release #{count} reserved slot(s), only #{@reserved} are reserved"
          end

          @reserved -= count
          @reserved
        end
      end

      # Releases one finished worker lease when one is held.
      #
      # Unlike {#release}, this is safe for completion paths that may be exercised directly in focused tests without a
      # preceding assignment. It also makes a duplicate completion a no-op instead of driving the counter below zero.
      #
      # @return [Integer] the number of slots still leased
      def release_if_possible
        @mutex.synchronize do
          @leased -= 1 if @leased.positive?
          @leased
        end
      end

      # Releases one finished worker lease, freeing its slot for a future reservation.
      #
      # @return [Integer] the number of slots still leased
      def release
        @mutex.synchronize do
          raise 'cannot release a slot that is not leased' if @leased.zero?

          @leased -= 1
          @leased
        end
      end

      # Slots not currently reserved or leased, i.e. that a new fetch may take.
      #
      # @return [Integer]
      def available
        @mutex.synchronize { available_unlocked }
      end

      # Slots currently held by running workers.
      #
      # @return [Integer]
      def leased
        @mutex.synchronize { @leased }
      end

      # Slots currently reserved by fetches whose messages have not been assigned yet.
      #
      # @return [Integer]
      def reserved
        @mutex.synchronize { @reserved }
      end

      # Slots occupied either by a reservation or a worker lease.
      #
      # @return [Integer]
      def occupied
        @mutex.synchronize { occupied_unlocked }
      end

      private

      # Slot accounting without locking, for callers already holding the mutex. Ruby's Mutex is not reentrant, so the
      # locked public methods must not call each other.
      def available_unlocked
        @capacity - @reserved - @leased
      end

      def occupied_unlocked
        @reserved + @leased
      end
    end
  end
end
