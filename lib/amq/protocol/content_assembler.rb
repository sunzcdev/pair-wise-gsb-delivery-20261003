# encoding: binary
# frozen_string_literal: true

require "amq/protocol/frame"
require "amq/protocol/exceptions"

module AMQ
  module Protocol
    # Assembles content-bearing methods (a method frame, a content header
    # frame and zero or more content body frames) into complete messages.
    #
    # Frames of different channels may be interleaved freely: every channel
    # is tracked independently. Heartbeat frames are ignored and never
    # advance any message being assembled.
    #
    # A message is only published once all body bytes declared by its
    # content header have arrived; a header declaring a zero body size
    # completes the message immediately.
    #
    # This is an optional utility: connection-level framing itself lives
    # in the amq-client library.
    class ContentAssembler
      # A fully assembled content message.
      class Message
        attr_reader :method_frame, :header_frame, :body

        def initialize(method_frame, header_frame, body)
          @method_frame = method_frame
          @header_frame = header_frame
          @body         = body
        end

        def channel
          @method_frame.channel
        end

        def properties
          @header_frame.properties
        end
      end

      # Per-channel assembly state
      class ChannelState
        attr_reader :method_frame, :header_frame

        def initialize(method_frame)
          @method_frame = method_frame
          @header_frame = nil
          @body_size    = nil
          @chunks       = nil
          @received     = 0
        end

        def expects_header?
          @header_frame.nil?
        end

        def start_body(header_frame)
          @header_frame = header_frame
          @body_size    = header_frame.body_size
          @chunks       = []
        end

        def remaining
          @body_size - @received
        end

        def overflows?(size)
          size > remaining
        end

        def append(payload)
          @chunks << payload
          @received += payload.bytesize
        end

        def complete?
          @received == @body_size
        end

        def assembled_body
          @chunks.join
        end
      end

      def initialize
        @states = {}
      end

      # Feeds a frame into the assembler.
      #
      # @return [Message, nil] a complete message once all declared body
      #   bytes have arrived, nil otherwise
      def feed(frame)
        case frame
        when HeartbeatFrame
          nil
        when MethodFrame
          feed_method(frame)
        when HeaderFrame
          feed_header(frame)
        when BodyFrame
          feed_body(frame)
        else
          raise UnexpectedFrameError, "Don't know how to assemble #{frame.inspect}"
        end
      end

      # Drops any incomplete assembly state of a channel, e.g. when the
      # channel is closed. Other channels are not affected, and frames of
      # a subsequent message are never appended to the truncated body.
      def close_channel(channel)
        @states.delete(channel)
        nil
      end

      def pending?(channel)
        @states.key?(channel)
      end

      private

      def feed_method(frame)
        raise UnexpectedFrameError, "#{frame.method_class.name} does not carry content" unless frame.method_class.has_content?
        raise UnexpectedFrameError, "Previous content on channel #{frame.channel} is still incomplete" if @states.key?(frame.channel)

        @states[frame.channel] = ChannelState.new(frame)
        nil
      end

      def feed_header(frame)
        state = @states[frame.channel]
        raise UnexpectedFrameError, "Content header on channel #{frame.channel} without a preceding method" if state.nil?
        raise DuplicateHeaderError unless state.expects_header?

        expected = state.method_frame.method_class.index >> 16
        actual   = frame.klass_id
        raise ContentClassMismatchError.new(expected, actual) unless actual == expected

        state.start_body(frame)
        complete(frame.channel, state) if state.complete?
      end

      def feed_body(frame)
        state = @states[frame.channel]
        raise UnexpectedFrameError, "Content body on channel #{frame.channel} without a preceding method and header" if state.nil? || state.expects_header?
        raise BodySizeExceededError.new(state.remaining, frame.size) if state.overflows?(frame.size)

        state.append(frame.payload)
        complete(frame.channel, state) if state.complete?
      end

      def complete(channel, state)
        @states.delete(channel)
        Message.new(state.method_frame, state.header_frame, state.assembled_body)
      end
    end
  end
end
