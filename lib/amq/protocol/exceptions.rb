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

    # Base class for content header property encoding/decoding failures.
    class PropertiesError < Protocol::Error
      DEFAULT_MESSAGE = "Invalid content header properties".freeze

      def self.inherited(subclass)
        Error.inherited(subclass)
      end # self.inherited
    end

    # Raised when a flags word contains bits that do not correspond to
    # any declared property (the continuation bit excluded).
    class UnknownPropertyFlags < PropertiesError
      def initialize(word_index, flags)
        super(format("Unknown property flags 0x%04x in flags word %d", flags, word_index))
      end
    end

    # Raised when a flags word has the continuation bit set but the frame
    # ends before the next flags word (or the frame has no flags word at all).
    class MissingPropertyFlagsContinuation < PropertiesError
      def initialize(word_index)
        super("Property flags word #{word_index} is missing or truncated within the frame")
      end
    end

    # Raised when a declared property value is cut off by the end of the frame.
    class TruncatedPropertyValue < PropertiesError
      def initialize(name, type, required, available)
        super("Property #{name} (#{type}) requires #{required} more byte(s) but only #{available} remain(s) in the frame")
      end
    end

    # Raised when bytes remain after the last declared property value.
    class UnexpectedPropertyBytes < PropertiesError
      def initialize(count)
        super("#{count} unexpected byte(s) after the last content header property")
      end
    end

    # Raised when encoding a property that is not part of the property
    # descriptions of the content class.
    class UnknownProperty < PropertiesError
      def initialize(name)
        super("Unknown content header property #{name.inspect}")
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
