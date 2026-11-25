# frozen_string_literal: true

module Shoryuken
  module Helpers
    # A one-shot, thread-safe cancellation flag for an in-flight fetch.
    #
    # A {Manager} hands one of these to every {Fetcher#fetch} it starts. The SQS request itself cannot be aborted once it
    # has been sent, so "cancelling" is cooperative: the flag is flipped before the response is consumed, and the fetch
    # result (or a response that arrives late, after the cancel) is checked against it instead of occupying worker slots
    # the reservation no longer owns.
    #
    # @example
    #   cancellation = Shoryuken::Helpers::FetchCancellation.new
    #   Thread.new { sleep 0.1; cancellation.cancel }
    #   messages = fetcher.fetch(queue, 3, cancellation)
    #   if cancellation.cancelled?
    #     # the response must not take any slots
    #   end
    class FetchCancellation
      # Creates a new, not-yet-cancelled cancellation.
      def initialize
        @mutex = Mutex.new
        @cancelled = false
      end

      # Marks the fetch as cancelled.
      #
      # Idempotent: cancelling an already-cancelled fetch keeps it cancelled and returns false so callers can tell the
      # first cancel apart.
      #
      # @return [Boolean] true when this call performed the cancellation, false when it was already cancelled
      def cancel
        @mutex.synchronize do
          return false if @cancelled

          @cancelled = true
        end
      end

      # Whether the fetch has been cancelled.
      #
      # @return [Boolean] true once {#cancel} has run
      def cancelled?
        @mutex.synchronize { @cancelled }
      end
    end
  end
end
