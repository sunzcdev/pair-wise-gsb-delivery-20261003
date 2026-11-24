# frozen_string_literal: true

module Bunny
  # Outcome of {Bunny::Channel#basic_publish_batch}. Every payload of the
  # batch is represented by one entry, in the original order, so callers can
  # find out how many messages were handed to the transport before a failure
  # and which payloads never started being written.
  #
  # Entry statuses:
  #
  #   +:written+      the frames were handed to the transport, waiting for a confirm
  #   +:failed+       writing this entry raised, see {#failure}
  #   +:not_started+  a previous entry failed, this entry was never written
  #
  # @api public
  class BatchPublishResult
    include Enumerable

    # Per-message outcome of one entry in a batch.
    class Entry
      STATUSES = [:written, :failed, :not_started].freeze

      # @return [Integer] zero-based index of the payload in the original batch
      attr_reader :index
      # @return [Object] the original payload
      attr_reader :payload
      # @return [Integer, nil] publisher confirm sequence number assigned to this entry
      attr_reader :seq_no
      # @return [Symbol] one of {STATUSES}
      attr_reader :status
      # @return [Exception, nil] exception raised while writing this entry
      attr_reader :error

      def initialize(index, payload, status, seq_no: nil, error: nil)
        raise ArgumentError, "unknown status: #{status.inspect}" unless STATUSES.include?(status)

        @index   = index
        @payload = payload
        @status  = status
        @seq_no  = seq_no
        @error   = error
      end

      def written?
        @status == :written
      end

      def failed?
        @status == :failed
      end

      def not_started?
        @status == :not_started
      end

      def to_h
        h = {index: @index, status: @status}
        h[:seq_no] = @seq_no unless @seq_no.nil?
        h[:error]  = @error  unless @error.nil?
        h
      end
    end

    def initialize(entries)
      @entries = entries
    end

    # @param [Integer] index zero-based payload index
    # @return [Entry]
    def [](index)
      @entries.fetch(index)
    end

    # @return [Array<Entry>]
    def entries
      @entries.dup
    end

    def each(&block)
      @entries.each(&block)
    end

    def size
      @entries.size
    end
    alias length size

    # @return [Integer] number of entries handed to the transport before the failure (or all of them)
    def written_count
      @entries.count(&:written?)
    end

    # @return [Integer] number of entries whose write raised
    def failed_count
      @entries.count(&:failed?)
    end

    # @return [Array<Integer>] indexes of entries that never started writing
    def not_started_indexes
      @entries.select(&:not_started?).map(&:index)
    end

    # Payloads that never started writing and can be retried by the caller.
    # @return [Array]
    def not_started_payloads
      @entries.select(&:not_started?).map(&:payload)
    end

    # @return [Entry, nil] the entry whose write raised, if any
    def failure
      @entries.find(&:failed?)
    end

    def success?
      !@entries.any?(&:failed?)
    end

    # Publisher confirm sequence numbers assigned to written entries.
    # Empty when publisher confirms are not enabled.
    # @return [Array<Integer>]
    def seq_nos
      @entries.select(&:written?).map(&:seq_no).compact
    end

    # @return [Array<Hash>]
    def to_a
      @entries.map(&:to_h)
    end
  end
end