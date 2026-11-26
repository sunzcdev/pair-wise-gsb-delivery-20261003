# frozen_string_literal: true

module Bunny
  # Outcome of a single {Bunny::Channel#basic_publish} when publish result
  # tracking is enabled (see {Bunny::Channel#confirm_select}).
  #
  # Each result is identified by its publish sequence number, not by message
  # payload, so multiple publishes with identical bodies are told apart.
  # A result combines the publisher confirm (ack or nack) with the
  # basic.return the broker sends for a mandatory message it cannot route.
  class PublishResult

    #
    # API
    #

    # @return [Integer] Publish sequence number that identifies this publish on its channel
    attr_reader :sequence_number
    # @return [String] Exchange the message was published to
    attr_reader :exchange
    # @return [String] Routing key the message was published with
    attr_reader :routing_key
    # @return [Bunny::ReturnInfo, nil] Why the message was returned, nil if it was not returned
    attr_reader :return_info

    # @private
    def initialize(sequence_number, exchange, routing_key)
      @sequence_number = sequence_number
      @exchange        = exchange
      @routing_key     = routing_key

      @confirm     = nil
      @return_info = nil
    end

    # @return [Symbol, nil] :ack or :nack once the broker has confirmed this publish, nil before
    def confirm
      @confirm
    end

    # @return [Boolean] true once the broker has confirmed (positively or negatively) this publish
    def confirmed?
      !@confirm.nil?
    end

    # @return [Boolean] true if the broker has confirmed this publish positively
    def acked?
      @confirm == :ack
    end

    # @return [Boolean] true if the broker has nacked this publish
    def nacked?
      @confirm == :nack
    end

    # @return [Boolean] true if the message was returned as unroutable
    def returned?
      !@return_info.nil?
    end

    # @return [Hash] Hash representation of this publish result
    def to_hash
      {
        sequence_number: @sequence_number,
        exchange:        @exchange,
        routing_key:     @routing_key,
        confirm:         @confirm,
        returned:        returned?,
        return_info:     @return_info&.to_hash
      }
    end

    # @private
    def to_s
      to_hash.to_s
    end

    # @private
    def inspect
      to_hash.inspect
    end

    # @private
    def confirm!(status)
      @confirm = status
    end

    # @private
    def return!(return_info)
      @return_info = return_info
    end
  end
end
