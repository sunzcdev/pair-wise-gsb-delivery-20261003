# frozen_string_literal: true

module Dry
  module Types
    class Tuple < Array
      # A positional slot of a {Tuple}. Slots are created when a tuple is
      # declared, they are not meant to be used directly.
      #
      # @api public
      class Slot
        include ::Dry::Equalizer(:type, :optional_flag, :rest_flag, inspect: false, immutable: true)

        # @return [Type]
        attr_reader :type

        # @param [Type] type
        # @param [Boolean] optional Whether the slot may be missing in input
        # @param [Boolean] rest Whether the slot repeats to cover a tail
        #
        # @api private
        def initialize(type, optional: false, rest: false)
          @type = type
          @optional = optional
          @rest = rest
          freeze
        end

        # Whether the slot may be absent from input
        #
        # @return [Boolean]
        #
        # @api public
        def optional? = @optional

        # Whether the slot is required to be present
        #
        # @return [Boolean]
        #
        # @api public
        def required? = !optional?

        # Whether the slot is a repeated tail
        #
        # @return [Boolean]
        #
        # @api public
        def rest? = @rest

        # Whether the slot fills missing positions with a default value
        #
        # @return [Boolean]
        #
        # @api public
        def default? = type.default?

        # Build a copy with overridden flags
        #
        # @return [Slot]
        #
        # @api private
        def with(optional: @optional, rest: @rest)
          Slot.new(type, optional: optional, rest: rest)
        end

        # Mark the slot as an optional tail item
        #
        # @return [Slot]
        #
        # @api public
        def omittable = with(optional: true)

        # Mark the slot as a repeated tail
        #
        # @return [Slot]
        #
        # @api public
        def rest = with(rest: true)

        # Dump to internal AST representation
        #
        # @return [Array]
        #
        # @api public
        def to_ast(meta: true)
          [:slot, [
            type.to_ast(meta: meta),
            {optional: optional?, rest: rest?}
          ]]
        end

        # @return [String]
        #
        # @api public
        def inspect
          marker = rest? ? "*" : optional? ? "?" : ::Dry::Core::Constants::EMPTY_STRING
          "#<Dry::Types::Tuple::Slot#{marker} #{type}>"
        end

        protected

        # @api private
        def optional_flag = @optional

        # @api private
        def rest_flag = @rest
      end
    end
  end
end
