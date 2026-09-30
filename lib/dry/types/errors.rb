# frozen_string_literal: true

module Dry
  module Types
    extend ::Dry::Core::ClassAttributes

    # @!attribute [r] namespace
    #   @return [Container{String => Nominal}]
    defines :namespace

    namespace self

    # Base class for coercion errors raise by dry-types
    #
    class CoercionError < ::StandardError
      # @api private
      def self.handle(exception, meta: Undefined)
        if block_given?
          yield
        else
          raise new(
            exception.message,
            meta: meta,
            backtrace: exception.backtrace
          )
        end
      end

      # Metadata associated with the error
      #
      # @return [Object]
      attr_reader :meta

      # @api private
      def initialize(message, meta: Undefined, backtrace: Undefined)
        unless message.is_a?(::String)
          raise ::ArgumentError, "message must be a string, #{message.class} given"
        end

        super(message)
        @meta = Undefined.default(meta, nil)
        set_backtrace(backtrace) unless Undefined.equal?(backtrace)
      end
    end

    # Collection of multiple errors
    #
    class MultipleError < CoercionError
      # @return [Array<CoercionError>]
      attr_reader :errors

      # @param [Array<CoercionError>] errors
      def initialize(errors)
        super("")
        @errors = errors
      end

      # @return string
      def message = errors.map(&:message).join(", ")

      # @return [Array]
      def meta = errors.map(&:meta)
    end

    class SchemaError < CoercionError
      # @return [String, Symbol]
      attr_reader :key

      # @return [Object]
      attr_reader :value

      # @param [String,Symbol] key
      # @param [Object] value
      # @param [String, #to_s] result
      def initialize(key, value, result)
        @key = key
        @value = value
        super(
          "#{value.inspect} (#{value.class}) has invalid type " \
          "for :#{key} violates constraints (#{result} failed)"
        )
      end
    end

    MapError = ::Class.new(CoercionError)

    SchemaKeyError = ::Class.new(CoercionError)
    private_constant(:SchemaKeyError)

    class MissingKeyError < SchemaKeyError
      # @return [Symbol]
      attr_reader :key

      # @param [String,Symbol] key
      def initialize(key)
        @key = key
        super("#{key.inspect} is missing in Hash input")
      end
    end

    class UnknownKeysError < SchemaKeyError
      # @return [Array<Symbol>]
      attr_reader :keys

      # @param [<String, Symbol>] keys
      def initialize(keys)
        @keys = keys
        super("unexpected keys #{keys.inspect} in Hash input")
      end
    end

    # Base class for errors raised while reading the discriminator tag of a
    # {Union} type. Subclasses are raised before any branch type is applied,
    # so a tag error can always be told apart from a branch coercion error.
    class UnionTagError < CoercionError
      # @return [Symbol]
      attr_reader :tag

      # @param [Symbol] tag Name of the field used for dispatching
      # @param [Object] input
      # @param [String] message
      def initialize(tag, input, message)
        @tag = tag
        super(message)
      end
    end

    # Raised when the input is not a hash and thus cannot carry a tag
    class InvalidUnionInputError < UnionTagError
      # @param [Symbol] tag Name of the field used for dispatching
      # @param [Object] input
      def initialize(tag, input)
        super(
          tag,
          input,
          "#{input.inspect} (#{input.class}) cannot be dispatched by " \
          "#{tag.inspect}: a Hash is required"
        )
      end
    end

    # Raised when neither the symbol nor the string version of the tag
    # field is present in the input hash
    class MissingUnionTagError < UnionTagError
      # @param [Symbol] tag Name of the field used for dispatching
      # @param [Object] input
      def initialize(tag, input)
        super(tag, input, "#{tag.inspect} is missing in #{input.inspect}")
      end
    end

    # Raised when the tag value does not match any of the declared branches
    class UnknownUnionTagError < UnionTagError
      # @return [Object]
      attr_reader :value

      # @param [Symbol] tag Name of the field used for dispatching
      # @param [Object] value Value found at the tag field
      # @param [Object] input
      # @param [Array<Object>] tags Known tag values
      def initialize(tag, value, input, tags)
        @value = value
        super(
          tag,
          input,
          "#{value.inspect} is not a known tag for #{tag.inspect}, " \
          "expected one of #{tags.inspect}"
        )
      end
    end

    class ConstraintError < CoercionError
      # @return [String, #to_s]
      attr_reader :result
      # @return [Object]
      attr_reader :input

      # @param [String, #to_s] result
      # @param [Object] input
      def initialize(result, input)
        @result = result
        @input = input

        if result.is_a?(::String)
          super(result)
        else
          super(to_s)
        end
      end

      # @return [String]
      def message
        "#{input.inspect} violates constraints (#{result} failed)"
      end
      alias_method :to_s, :message
    end
  end
end
