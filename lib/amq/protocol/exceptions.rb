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

    class UnexpectedFrameError < Protocol::Error
      DEFAULT_MESSAGE = "Frame received out of sequence.".freeze
    end

    class DuplicateHeaderError < Protocol::Error
      DEFAULT_MESSAGE = "Content header has already been received for the message being assembled.".freeze
    end

    class BodySizeExceededError < Protocol::Error
      DEFAULT_MESSAGE = "Content body exceeds the size declared in the content header.".freeze

      def initialize(remaining = nil, received = nil)
        if remaining && received
          super("Content body exceeds the declared size: #{received} more bytes arrived while only #{remaining} bytes remain.")
        else
          super()
        end
      end
    end

    class ContentClassMismatchError < Protocol::Error
      DEFAULT_MESSAGE = "Content header class does not match the class of the method being assembled.".freeze

      def initialize(expected = nil, actual = nil)
        if expected && actual
          super("Content header class #{actual} does not match method class #{expected}.")
        else
          super()
        end
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
