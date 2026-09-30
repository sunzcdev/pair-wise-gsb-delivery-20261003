# frozen_string_literal: true

module Dry
  class Struct
    # Raised when given input doesn't conform schema and constructor type
    Error = Class.new(::Dry::Types::CoercionError)

    # Raised when defining duplicate attributes
    class RepeatedAttributeError < ::ArgumentError
      # @param [Symbol] key
      #   attribute name that is the same as previously defined one
      def initialize(key)
        super("Attribute :#{key} has already been defined")
      end
    end

    # Raised when a struct doesn't have an attribute
    class MissingAttributeError < ::KeyError
      def initialize(attribute:, klass:)
        super("Missing attribute: #{attribute.inspect} on #{klass}")
      end
    end

    # Raised when building a projection references an attribute that
    # doesn't exist on the source struct
    class UnknownProjectionPath < ::KeyError
      # @param path [Array<Symbol>] the full path that couldn't be resolved
      # @param klass [Class] the struct class the path was resolved against
      def initialize(path:, klass:)
        super(
          "Can't project #{path.map(&:inspect).join(" -> ")}: " \
          "#{path.last.inspect} is not an attribute of #{klass}"
        )
      end
    end

    # Raised when a projection descends into a non-struct attribute,
    # or when a nested struct ends up with no selected attributes
    class InvalidProjection < ::ArgumentError
    end

    # When struct class stored in ast was garbage collected because no alive objects exists
    # This shouldn't happen in a working application
    class RecycledStructError < ::RuntimeError
      def initialize
        super("Reference to struct class was garbage collected")
      end
    end
  end
end
