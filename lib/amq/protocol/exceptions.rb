# frozen_string_literal: true

module AMQ
  module Protocol
    class Error < StandardError
      DEFAULT_MESSAGE = "AMQP error".freeze

      def self.inherited(subclass)
        @_subclasses ||= []
        @_subclasses << subclass
      end # self.inherited(subclazz)

      def self.subclasses_with_values
        @_subclasses.select{ |k| defined?(k::VALUE) }
      end # self.subclasses_with_values

      def self.[](code)
        if result = subclasses_with_values.detect { |klass| klass::VALUE == code }
          result
        else
          raise "No such exception class for code #{code}" unless result
        end # if
      end # self.[]

      def initialize(message = self.class::DEFAULT_MESSAGE)
        super(message)
      end
    end

    class FrameTypeError < Protocol::Error
      def initialize(types)
        super("Must be one of #{types.inspect}")
      end
    end

    class EmptyResponseError < Protocol::Error
      DEFAULT_MESSAGE = "Empty response received from the server."

      def initialize(message = self.class::DEFAULT_MESSAGE)
        super(message)
      end
    end

    class BadResponseError < Protocol::Error
      def initialize(argument, expected, actual)
        super("Argument #{argument} has to be #{expected.inspect}, was #{actual.inspect}")
      end
    end

    # Base class for FrameDecoder failures. After any of these are raised
    # the decoder is in a failed state and must be reset before reuse.
    class FrameDecoderError < Protocol::Error
      DEFAULT_MESSAGE = "Frame decoder error".freeze
    end

    # Declared frame payload size exceeds the configured maximum. Raised
    # as soon as the header is complete, before payload bytes are buffered.
    class OversizedFrameError < FrameDecoderError
      def initialize(size, max)
        super("Declared frame payload size #{size} exceeds maximum allowed #{max}")
      end
    end

    # Frame does not terminate with the required 0xCE octet. The decoder
    # does not resynchronize by scanning for the next 0xCE.
    class InvalidFrameEndError < FrameDecoderError
      def initialize(actual)
        super(format("Frame must end with 0xCE, got 0x%02X", actual))
      end
    end

    # Buffered input exceeds the configured limit.
    class InputBufferOverflowError < FrameDecoderError
      def initialize(size, max)
        super("Input buffer size #{size} exceeds maximum allowed #{max}")
      end
    end

    # Input was explicitly finished while a partial frame remained.
    class TruncatedInputError < FrameDecoderError
      def initialize(pending_bytes)
        super("Input ended with #{pending_bytes} buffered bytes of an incomplete frame")
      end
    end

    class SoftError < Protocol::Error
      def self.inherited(subclass)
        Error.inherited(subclass)
      end # self.inherited(subclass)
    end

    class HardError < Protocol::Error
      def self.inherited(subclass)
        Error.inherited(subclass)
      end # self.inherited(subclass)
    end
  end
end
