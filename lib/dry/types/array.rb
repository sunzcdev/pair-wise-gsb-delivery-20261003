# frozen_string_literal: true

module Dry
  module Types
    # Array type can be used to define an array with optional member type
    #
    # @api public
    class Array < Nominal
      # Build an array type with a member type
      #
      # @param [Type,#call] type
      #
      # @return [Array::Member]
      #
      # @api public
      def of(type)
        member =
          case type
          when ::String then Types[type]
          else type
          end

        Array::Member.new(primitive, **options, member: member)
      end

      # Build a tuple type, that is an array whose positions are typed
      # individually
      #
      # @example
      #   Dry::Types["array"].tuple(
      #     Dry::Types["strict.symbol"],
      #     Dry::Types["strict.integer"],
      #     Dry::Types["strict.string"].omittable
      #   )
      #
      # @param slots [Array<Type, Tuple::Slot>] positional type declarations
      #
      # @return [Tuple]
      #
      # @api public
      def tuple(*slots)
        Tuple.new(primitive, **options, slots: Tuple.normalize(slots), meta: meta)
      end

      # @api private
      def constructor_type
        ::Dry::Types::Array::Constructor
      end
    end
  end
end
