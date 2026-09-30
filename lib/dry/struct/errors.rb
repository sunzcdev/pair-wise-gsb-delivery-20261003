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

    # When struct class stored in ast was garbage collected because no alive objects exists
    # This shouldn't happen in a working application
    class RecycledStructError < ::RuntimeError
      def initialize
        super("Reference to struct class was garbage collected")
      end
    end

    # Base class for projection (view building) errors
    class ProjectionError < ::ArgumentError; end

    # Raised when a projection path doesn't match any attribute
    class UnknownAttributeError < ProjectionError
      # @param [Class] source struct class being projected
      # @param [Symbol] name unknown attribute name
      # @param [Array<Symbol>] path path to the attribute within the tree
      def initialize(source:, name:, path:)
        full_path = (path + [name]).join(".")
        available = source.attribute_names.map { |attr| ":#{attr}" }.join(", ")

        super(
          "Unknown attribute :#{name} at \"#{full_path}\" on #{source} " \
          "(available attributes: #{available})"
        )
      end
    end

    # Raised when a nested projection selects nothing
    class EmptyProjectionError < ProjectionError
      # @param [Array<Symbol>] path path to the attribute with an empty selection
      def initialize(path)
        super(
          "Empty projection at \"#{path.join('.')}\": select at least one " \
          "nested attribute, or select :#{path.last} as a whole"
        )
      end
    end

    # Raised when projecting into a type that has no projectable attributes
    class UnsupportedProjectionError < ProjectionError
      # @param [Array<Symbol>] path path to the attribute
      # @param [Object] type attribute type that cannot be projected
      # @param [String, nil] reason optional explanation
      def initialize(path, type, reason: nil)
        super(
          "Cannot project into \"#{path.join('.')}\": #{reason || "#{type} is not a struct " \
          "or an array of structs"}"
        )
      end
    end
  end
end
