# frozen_string_literal: true

require "bunny/concurrent/continuation_queue"

module Bunny
  module Concurrent
    # Tracks in-flight channel.open/channel.close operations by channel id.
    #
    # A channel id can be reused once a channel has been closed, so responses
    # must be routed to the operation that actually requested them instead of
    # being dumped into a single connection-wide queue. Each waiter records
    # the protocol method class it expects, which lets a late channel.close-ok
    # for a previous channel generation be matched to its own channel.close
    # even when a channel.open has already reused the same id. Frames that do
    # not match any pending operation are ignored.
    #
    # Every connection has its own registry. All operations are synchronized
    # and safe to perform from multiple threads.
    #
    # @private
    class PendingChannelOperationRegistry
      def initialize
        @mutex    = Monitor.new
        @waiters  = Hash.new { |hash, key| hash[key] = [] }
        @sequence = 0
      end

      # Registers a pending operation for the given channel id and yields to
      # the block, which is expected to put the request frame on the wire. The
      # block runs while holding the registry lock so that responses cannot
      # overtake the registration.
      #
      # @param [Integer] id channel id the operation is performed on
      # @param [Class] expected_method_class protocol method class the
      #   operation waits for, e.g. AMQ::Protocol::Channel::OpenOk
      # @param [Integer] timeout_in_ms continuation timeout in milliseconds
      # @return [Object] the protocol method delivered for this operation
      def await(id, expected_method_class, timeout_in_ms)
        token, queue = @mutex.synchronize do
          token = next_sequence
          queue = ContinuationQueue.new
          @waiters[id] << [token, expected_method_class, queue]
          yield
          [token, queue]
        end

        queue.poll(timeout_in_ms)
      ensure
        @mutex.synchronize do
          entries = @waiters[id]
          entries.delete_if { |entry_token, _klass, _queue| entry_token == token }
          @waiters.delete(id) if entries.empty?
        end
      end

      # Delivers a response frame to the operation waiting for its method
      # class on the given channel id. Frames that do not match any pending
      # operation are ignored: they belong to a channel generation whose
      # operation already has a terminal state, and must never affect the
      # channel that reused the id.
      #
      # @return [Boolean] true when the frame was delivered
      def deliver(id, method)
        @mutex.synchronize do
          entries = @waiters[id]
          return false if entries.nil? || entries.empty?

          entry = entries.find { |_token, klass, _queue| method.is_a?(klass) }
          return false unless entry

          entry.last.push(method)
          true
        end
      end

      # Fails every pending operation, e.g. when the connection is lost, so
      # that waiting threads do not block until the continuation timeout.
      def fail_all(method)
        @mutex.synchronize do
          @waiters.each_value do |entries|
            entries.each { |_token, _klass, queue| queue.push(method) }
          end
        end
      end

      # @return [Boolean] true when at least one operation for the id is pending
      def pending?(id)
        @mutex.synchronize do
          entries = @waiters[id]
          entries && !entries.empty?
        end
      end

      # @return [Integer] number of pending operations across all channel ids
      def size
        @mutex.synchronize { @waiters.values.sum(&:size) }
      end

      def empty?
        size.zero?
      end

      private

      def next_sequence
        @sequence += 1
      end
    end
  end
end
