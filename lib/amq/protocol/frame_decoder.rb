# encoding: binary
# frozen_string_literal: true

require "amq/protocol/client"

module AMQ
  module Protocol
    # Incremental AMQP 0-9-1 frame decoder with no IO adapter dependency.
    #
    # Feed arbitrary byte chunks with #<< (aliased as #call); every frame
    # that became complete is returned and the remainder is kept buffered
    # for the next chunk. Header, payload and the frame-end octet may be
    # split across chunks at any boundary.
    #
    # After any decoding error the decoder is in a failed state: it rejects
    # further input until #reset is called and never attempts to
    # resynchronize by scanning for the next frame-end octet.
    class FrameDecoder
      # 1 octet type, 2 octets channel, 4 octets payload size
      HEADER_SIZE = 7
      FRAME_END_OCTET = 0xCE
      # AMQP 0-9-1 default frame_max
      DEFAULT_MAX_FRAME_SIZE = 131_072
      DEFAULT_MAX_BUFFER_SIZE = 1_048_576

      attr_reader :max_frame_size, :max_buffer_size

      # @param max_frame_size [Integer] largest accepted declared payload size
      # @param max_buffer_size [Integer] largest accepted input buffer size,
      #   defaults to at least one maximal frame plus header and end octet
      def initialize(max_frame_size: DEFAULT_MAX_FRAME_SIZE, max_buffer_size: nil)
        @max_frame_size = max_frame_size
        @max_buffer_size = max_buffer_size || [DEFAULT_MAX_BUFFER_SIZE, max_frame_size + HEADER_SIZE + 1].max
        reset
      end

      # Clears buffered input and the failed state.
      def reset
        @buffer = +""
        @pending_header = nil
        @failed = false
        self
      end

      def failed?
        @failed
      end

      # Bytes retained because they do not form a complete frame yet.
      def buffered_bytes
        @buffer.bytesize
      end

      # Feeds one chunk of bytes, returns an array of decoded frames
      # (empty when the chunk did not complete any frame).
      def <<(chunk)
        raise FrameDecoderError, "decoder is in a failed state, call #reset before feeding more data" if @failed
        raise TypeError, "chunk must be a String, got #{chunk.class}" unless chunk.is_a?(String)

        @buffer << (chunk.encoding == Encoding::BINARY ? chunk : chunk.dup.force_encoding(Encoding::BINARY))
        fail_with(InputBufferOverflowError.new(@buffer.bytesize, @max_buffer_size)) if @buffer.bytesize > @max_buffer_size

        frames = []
        begin
          decode_available(frames)
        rescue Protocol::Error
          @failed = true
          raise
        end
        frames
      end
      alias_method :call, :<<

      # Signals that no more input will arrive. Returns :complete when all
      # fed bytes formed whole frames, raises TruncatedInputError when a
      # partial frame remains.
      def finish
        raise FrameDecoderError, "decoder is in a failed state, call #reset before reusing" if @failed
        return :complete if @buffer.empty?

        @failed = true
        raise TruncatedInputError.new(@buffer.bytesize)
      end

      private

      def fail_with(error)
        @failed = true
        raise error
      end

      def decode_available(frames)
        loop do
          type, channel, size = @pending_header
          if type.nil?
            break if @buffer.bytesize < HEADER_SIZE

            type, channel, size = Frame.decode_header(@buffer.byteslice(0, HEADER_SIZE))
            # reject oversized declarations before any payload is buffered
            fail_with(OversizedFrameError.new(size, @max_frame_size)) if size > @max_frame_size
            @pending_header = [type, channel, size]
            consume(HEADER_SIZE)
          end

          break if @buffer.bytesize < size + 1

          final_octet = @buffer.getbyte(size)
          fail_with(InvalidFrameEndError.new(final_octet)) unless final_octet == FRAME_END_OCTET

          frames << Frame.new(type, @buffer.byteslice(0, size), channel)
          consume(size + 1)
          @pending_header = nil
        end
      end

      def consume(count)
        @buffer = @buffer.byteslice(count..-1) || (+"")
      end
    end
  end
end
