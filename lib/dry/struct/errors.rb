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

    # Raised when attributes imported from different sources share a name
    # but originate from distinct definitions. The collision must be resolved
    # explicitly (by renaming, namespacing or picking one of the sources).
    class AttributeConflictError < ::ArgumentError
      # @param attribute [Symbol] the colliding attribute name
      # @param definitions [Array<Dry::Struct::AttributeFragment::Origin>]
      #   the definitions that diverge
      def self.build(attribute:, definitions:)
        listed = definitions.each_with_index.map do |origin, index|
          location = origin.location
          suffix = location ? "\n      defined at #{location}" : ""
          "  #{index + 1}. #{origin.describe}#{suffix}"
        end.join("\n")

        new(
          "Conflicting definitions of attribute :#{attribute}:\n" \
          "#{listed}\n" \
          "Resolve the collision explicitly using `rename:`, `namespace:` " \
          "or `only:` when importing the attributes."
        )
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
  end
end
