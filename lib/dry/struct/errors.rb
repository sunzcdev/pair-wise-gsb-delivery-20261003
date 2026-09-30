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

    # Raised when an import brings in an attribute whose name is already
    # taken by a definition from a *different* source. The error message
    # points at both definitions so the conflict can be resolved explicitly
    # (via `rename:`, `except:` or `resolve:`)
    class AttributeConflictError < RepeatedAttributeError
      # @return [Symbol] the conflicting attribute name
      attr_reader :attribute

      # @return [Array<Fragment::Source>] the two diverging definitions
      attr_reader :sources

      # @param [Symbol] attribute
      # @param [Array<Fragment::Source>] sources
      # @param [String, nil] message custom message, built from the sources by default
      def initialize(attribute, sources, message: nil)
        @attribute = attribute
        @sources = sources
        @message = message
        super(attribute)
      end

      def message
        @message || begin
          first, second = @sources
          "Attribute :#{@attribute} is defined by both #{first} and #{second}; " \
            "disambiguate with `rename:` or `except:`, " \
            "or pick a definition with `resolve:`"
        end
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
