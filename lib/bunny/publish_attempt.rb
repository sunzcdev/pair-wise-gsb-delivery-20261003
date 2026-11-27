# frozen_string_literal: true

module Bunny
  # Tracks how many bytes of a single publish write the socket accepted.
  #
  # It is what lets Bunny tell a message no transport ever saw any byte of
  # apart from one a transport accepted only partially: the former can be
  # encoded again for another connection, the latter has an unknown write
  # result.
  #
  # @private
  class WriteProgress
    # @return [Integer] bytes accepted by the socket so far
    attr_reader :bytes_written
    # @return [Boolean] true when a failed write made it impossible to tell how
    #   many bytes the socket accepted
    attr_reader :unknown

    def initialize
      @bytes_written = 0
      @unknown       = false
    end

    def advance(count)
      @bytes_written += count
    end

    # Records that the number of accepted bytes cannot be determined, which can
    # happen with a blocking socket write that failed mid-buffer.
    def unknown!
      @unknown = true
    end

    def unknown?
      @unknown
    end
  end

  # One connection a published message can be attributed to: its transport, the
  # generation that transport belongs to and the frame_max negotiated on it.
  #
  # The generation changes whenever the connection is replaced, so an attempt
  # whose generation no longer matches the current one was encoded against a
  # connection that is gone, and its `frame_max` may not satisfy the one in use.
  #
  # @private
  PublishAttribution = Struct.new(:transport, :generation, :frame_max) do
    # Encodes a message for this connection. When it was encoded against a
    # different frame_max, which happens when the connection was replaced while
    # the message was being published, it is encoded again.
    #
    # @param [Integer] encoded_frame_max frame_max the frames were encoded against
    # @param [Array<AMQ::Protocol::Frame>] frames frames encoded against `encoded_frame_max`
    # @yield [frame_max] encodes the same message against the given frame_max
    # @return [Bunny::PublishAttempt]
    def frameset(encoded_frame_max, frames, reencoder)
      if encoded_frame_max == frame_max
        PublishAttempt.new(self, frames.reduce(+"") { |acc, frame| acc << frame.encode })
      else
        PublishAttempt.new(self, reencoder.call(frame_max).reduce(+"") { |acc, frame| acc << frame.encode })
      end
    end

    # Encodes a batch of messages for this connection, see {#frameset}.
    #
    # @param [Integer] encoded_frame_max frame_max the data was encoded against
    # @param [String] data frames encoded against `encoded_frame_max`
    # @yield [frame_max] encodes the same batch against the given frame_max
    # @return [Bunny::PublishAttempt]
    def preencoded(encoded_frame_max, data, reencoder)
      if encoded_frame_max == frame_max
        PublishAttempt.new(self, data)
      else
        PublishAttempt.new(self, reencoder.call(frame_max))
      end
    end
  end

  # Encoded bytes of one published message, attributed to one connection.
  #
  # @private
  class PublishAttempt
    # @return [Bunny::PublishAttribution] connection this message is attributed to
    attr_reader :attribution
    # @return [String] encoded frames, in write order
    attr_reader :data

    # @param [Bunny::PublishAttribution] attribution
    # @param [String] data
    def initialize(attribution, data)
      @attribution = attribution
      @data        = data
      # socket writes slice the buffer in place, so the size is captured here
      @bytesize    = data.bytesize
    end

    # @return [Bunny::Transport] transport this message is attributed to
    def transport
      @attribution.transport
    end

    # @return [Integer] frame_max the frames were encoded against
    def frame_max
      @attribution.frame_max
    end

    # @return [Integer] number of bytes this message consists of
    def bytesize
      @bytesize
    end

    # @param [Bunny::PublishAttribution] other
    # @return [Boolean] true if this message is attributed to the given connection
    def attributed_to?(other)
      @attribution.transport.equal?(other.transport) && @attribution.generation == other.generation
    end
  end
end
