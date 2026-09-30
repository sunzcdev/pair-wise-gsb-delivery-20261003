# frozen_string_literal: true

module Grape
  class Entity
    # Describes a single field that could not be represented.
    class PartialError
      attr_reader :path, :attribute, :exception

      # @param path [Array] external path of the failed value, built
      #   from +:as+ aliases and collection positions (0-based)
      # @param attribute [Symbol, String] internal attribute name
      # @param exception [Exception] the original failure, preserved
      #   as the cause
      def initialize(path, attribute, exception)
        @path = path
        @attribute = attribute
        @exception = exception
      end

      alias cause exception

      def message
        exception.message
      end

      def ==(other)
        other.is_a?(self.class) &&
          path == other.path &&
          attribute == other.attribute &&
          exception.class == other.exception.class &&
          exception.message == other.exception.message
      end
    end

    # Outcome of a partial representation: the (possibly incomplete)
    # serializable value together with the structured list of errors
    # collected while building it.
    class PartialResult
      attr_reader :value, :errors

      def initialize(value, errors)
        @value = value
        @errors = errors
      end

      def success?
        errors.empty?
      end

      def to_h
        value
      end

      def ==(other)
        other.is_a?(self.class) && value == other.value && errors == other.errors
      end
    end
  end
end
