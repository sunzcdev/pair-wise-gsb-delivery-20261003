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

      # Build a tuple type, i.e. an array with a type declared per position
      #
      # @example fixed positions
      #   message = Types::Array.tuple(Types::String, Types::Integer)
      #   message.(["create", 42])
      #
      # @example optional trailing position and a repeated tail
      #   message = Types::Array.tuple do
      #     required Types::String
      #     required Types::Integer
      #     optional Types::String
      #     rest Types::Any
      #   end
      #
      # @overload tuple(*types)
      #   @param [Type, String, Class] types fixed position types
      #   @return [Tuple]
      #
      # @overload tuple(&block)
      #   Declare positions with a block, see {Tuple::DSL}
      #   @return [Tuple]
      #
      # @api public
      def tuple(*types, &block)
        positions, rest = Tuple.build_positions(types, &block)

        Tuple.new(primitive, **options, positions: positions, rest: rest, meta: meta)
      end

      # @api private
      def constructor_type
        ::Dry::Types::Array::Constructor
      end
    end
  end
end
