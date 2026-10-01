# frozen_string_literal: true

module Dry
  module Types
    # Tuple types validate array input position by position. Unlike {Array::Member},
    # which applies a single type to every element, a tuple declares a type per
    # position:
    #
    #   message = Types::Array.tuple(Types::String, Types::Integer)
    #   message.(["create", 42]) # => ["create", 42]
    #
    # Positions are required by default. The last positions may be marked
    # optional or replaced with a single "rest" type matching any number of
    # trailing elements:
    #
    #   op = Types::Array.tuple do
    #     required Types::String
    #     required Types::Integer
    #     optional Types::String
    #     rest Types::Any
    #   end
    #
    # Missing positions are never silently turned into nil and extra items are
    # never dropped: short or long input fails unless the declaration allows
    # for it.
    #
    # @api public
    class Tuple < Array
      include ::Dry::Equalizer(:primitive, :positions, :rest, :options, :meta,
                                inspect: false, immutable: true)

      # A single position of a tuple declaration
      #
      # @api public
      class Position
        include ::Dry::Equalizer(:type, :kind, inspect: false, immutable: true)

        # @return [Type] type applied to the position
        attr_reader :type

        # @return [Symbol] kind of the position, used for equality
        #
        # @api private
        def kind = :required

        # @param [Type] type
        #
        # @api private
        def initialize(type)
          @type = type
        end

        # @return [Boolean] whether the position may be missing in the input
        #
        # @api public
        def optional? = false

        # @return [Boolean] whether the position collects all remaining elements
        #
        # @api public
        def rest? = false

        # @return [Boolean] whether a missing position is filled with a default
        #
        # @api private
        def default? = type.default?

        # Mark the position as optional, i.e. allowed to be missing
        #
        # @return [Tuple::Position::Optional]
        #
        # @api public
        def optional = Tuple::Position::Optional.new(type)

        # Build an AST node for the position
        #
        # @return [Array]
        #
        # @api public
        def to_ast(meta: true)
          [:tuple_position, [type.to_ast(meta: meta)]]
        end

        # Wrap a type into a position unless it already is one
        #
        # @return [Tuple::Position]
        #
        # @api private
        def self.[](type)
          type.is_a?(Tuple::Position) ? type : new(coerce(type))
        end

        # @api private
        def self.coerce(type)
          case type
          when Type then type
          when ::String then Types[type]
          when ::Class then Types[type]
          else type
          end
        end

        # An optional position, allowed to be missing only when it is among
        # the last positions of the tuple.
        #
        # @api public
        class Optional < Position
          # @return [true]
          #
          # @api public
          def optional? = true

          # @api private
          def kind = :optional

          # @see Position#to_ast
          #
          # @api public
          def to_ast(meta: true)
            [:tuple_optional, [type.to_ast(meta: meta)]]
          end
        end

        # A rest position applies its type to every remaining element
        #
        # @api public
        class Rest < Position
          # @return [true]
          #
          # @api public
          def rest? = true

          # @api private
          def kind = :rest

          # @see Position#to_ast
          #
          # @api public
          def to_ast(meta: true)
            [:tuple_rest, [type.to_ast(meta: meta)]]
          end
        end
      end

      # Block-level DSL for declaring tuple positions. The block is evaluated
      # with the DSL as self but unknown methods are delegated back to the
      # calling object, so helpers available at the call site keep working.
      #
      # @see Array#tuple
      #
      # @api public
      class DSL < BasicObject
        # @return [Array<Tuple::Position>]
        attr_reader :positions

        # @param [Object] caller_self self at the place the block was defined
        #
        # @api private
        def initialize(caller_self)
          @positions = []
          @caller_self = caller_self
        end

        # Declare a required position
        #
        # @param [Type] type
        # @return [Tuple::Position]
        #
        # @api public
        def required(type)
          add(Tuple::Position.new(Tuple::Position.coerce(type)))
        end

        # Declare an optional trailing position
        #
        # @param [Type] type
        # @return [Tuple::Position::Optional]
        #
        # @api public
        def optional(type)
          add(Tuple::Position::Optional.new(Tuple::Position.coerce(type)))
        end

        # Declare a repeated trailing position
        #
        # @param [Type] type
        # @return [Tuple::Position::Rest]
        #
        # @api public
        def rest(type)
          add(Tuple::Position::Rest.new(Tuple::Position.coerce(type)))
        end

        private

        # @api private
        def add(position)
          @positions << position
          position
        end

        # Delegate unknown calls back to the calling object (e.g. RSpec
        # helpers), keeping private methods private
        #
        # @api private
        def method_missing(name, ...)
          if @caller_self.respond_to?(name, false)
            @caller_self.public_send(name, ...)
          else
            super
          end
        end

        # @api private
        def respond_to_missing?(name, include_private = false)
          @caller_self.respond_to?(name, include_private) || super
        end
      end

      # Build positions from plain arguments and/or a block
      #
      # @param [Array<Type, Tuple::Position>] types
      # @return [Array(Array<Tuple::Position>, Tuple::Position::Rest, nil)]
      #
      # @api private
      def self.build_positions(types, &block)
        if block
          unless types.empty?
            raise ::ArgumentError, "do not pass both positional types and a block"
          end

          dsl = DSL.new(eval("self", block.binding))
          dsl.instance_exec(&block)
          raw = dsl.positions
        else
          raw = types.map { |type|
            type.is_a?(Position) ? type : Position.new(Position.coerce(type))
          }
        end

        rest = raw.last if raw.last.is_a?(Position::Rest)
        [rest ? raw[0..-2] : raw, rest]
      end

      # Mark a type as an optional trailing tuple position
      #
      # @param [Type] type
      # @return [Tuple::Position::Optional]
      #
      # @api public
      def self.optional(type)
        Position::Optional.new(Position.coerce(type))
      end

      # Mark a type as a repeated trailing tuple position
      #
      # @param [Type] type
      # @return [Tuple::Position::Rest]
      #
      # @api public
      def self.rest(type)
        Position::Rest.new(Position.coerce(type))
      end

      # Wrap a type as a required tuple position
      #
      # @param [Type] type
      # @return [Tuple::Position]
      #
      # @api public
      def self.required(type)
        Position.new(Position.coerce(type))
      end

      # @return [Array<Tuple::Position>] fixed positions (required, then optional)
      attr_reader :positions

      # @return [Tuple::Position::Rest, nil] optional rest position
      attr_reader :rest

      # @param [Class] primitive
      # @param [Hash] options
      #
      # @option options [Array<Tuple::Position>] :positions Fixed positions
      # @option options [Tuple::Position::Rest, nil] :rest Rest position
      #
      # @api private
      def initialize(primitive, **options)
        @positions = options.fetch(:positions)
        @rest = options[:rest]

        validate!

        @required_count =
          positions.count { |position| !position.optional? && !position.default? }

        super
      end

      # Apply the tuple type
      #
      # @param [Object] input
      #
      # @return [Array]
      #
      # @raise [TupleError, CoercionError]
      #
      # @api private
      def call_unsafe(input)
        return super unless primitive?(input)

        raise(arity_error(input)) unless arity_valid?(input)

        output = []

        traverse_unsafe(input) do |coerced|
          output << coerced unless Undefined.equal?(coerced)
        end

        output
      end

      # @param [Object] input
      #
      # @return [Array]
      #
      # @api private
      def call_safe(input)
        return super unless primitive?(input)

        unless arity_valid?(input)
          return yield(partial_output(input))
        end

        output = []

        traverse_safe(input, output) do
          return yield(output)
        end

        output
      end

      # @param [Object] input
      # @param [#call, nil] block
      #
      # @yieldparam [Failure] failure
      # @yieldreturn [Result]
      #
      # @return [Result::Success, Result::Failure]
      #
      # @api public
      def try(input, &block)
        if !primitive?(input)
          failure = failure(input, CoercionError.new("#{input} is not an array"))
          return block ? yield(failure) : failure
        end

        unless arity_valid?(input)
          failure = failure(partial_output(input), arity_error(input))
          return block ? yield(failure) : failure
        end

        output = []
        failure = nil

        traverse_try(input, output) do |error, partial|
          failure ||= failure(partial, error)
        end

        if failure
          block ? yield(failure) : failure
        else
          success(output)
        end
      end

      # @see Nominal#to_ast
      #
      # @api public
      def to_ast(meta: true)
        nodes = positions.map { |position| position.to_ast(meta: meta) }
        nodes << rest.to_ast(meta: meta) if rest

        [:tuple, [nodes, meta_ast(meta)]]
      end

      # Build a lax tuple type
      #
      # @return [Lax]
      #
      # @api public
      def lax
        lax_positions = positions.map { |position|
          position.class.new(position.type.lax)
        }
        lax_rest = rest && Position::Rest.new(rest.type.lax)

        Lax.new(Tuple.new(
          primitive,
          **options.except(:positions, :rest),
          positions: lax_positions,
          rest: lax_rest,
          meta: meta
        ))
      end

      # @api private
      def constructor_type = ::Dry::Types::Array::Constructor

      private

      # @api private
      def validate!
        saw_optional = false

        positions.each do |position|
          if position.is_a?(Position::Rest)
            # A rest given as a fixed position is rejected, only one rest is
            # allowed and it must be declared via #rest/the :rest option.
            raise ::ArgumentError, "rest position must be declared with #rest"
          end

          if position.optional? || position.default?
            saw_optional = true
          elsif saw_optional
            raise ::ArgumentError,
              "cannot declare a required tuple position after an optional one"
          end
        end

        if rest && !rest.is_a?(Position::Rest)
          raise ::ArgumentError, "rest must be a rest position, #{rest.inspect} given"
        end
      end

      # Yield [value, original_index] for every fixed position present in the
      # input. Missing positions with defaults are passed Undefined; missing
      # optional positions are skipped entirely. When a rest position exists,
      # all remaining input elements are yielded afterwards.
      #
      # @api private
      def each_occurrence(input)
        positions.each_with_index do |position, index|
          if index < input.size
            yield(input.fetch(index), index)
          elsif position.default?
            yield(Undefined, index)
          end
          # Plain optional positions are simply absent in the output.
        end

        if rest
          input[positions.size..].to_a.each_with_index do |value, offset|
            yield(value, positions.size + offset)
          end
        end
      end

      # @api private
      def traverse_unsafe(input)
        each_occurrence(input) do |value, index|
          yield(position_type_at(index).call_unsafe(value))
        rescue CoercionError => exception
          raise element_error(index, exception)
        end
      end

      # @api private
      def traverse_safe(input, output, &halt)
        each_occurrence(input) do |value, index|
          coerced = position_type_at(index).call_safe(value, &halt)

          output << coerced unless Undefined.equal?(coerced)
        end
      end

      # @api private
      def traverse_try(input, output)
        each_occurrence(input) do |value, index|
          result = position_type_at(index).try(value)

          if result.success?
            output << result.input unless Undefined.equal?(result.input)
          else
            output << value
            yield(element_error(index, result.error), output)
          end
        end
      end

      # Best-effort coercion of the whole input, used for partial results
      # reported on arity failures. Every input element is included in the
      # output (coerced when possible, original otherwise), missing defaulted
      # positions are filled. Based on #try so constructors that raise do not
      # propagate out of the safe/failure paths.
      #
      # @api private
      def partial_output(input)
        output = []

        input.each_with_index do |value, index|
          type = index < positions.size ? positions[index]&.type : rest&.type

          if type
            result = type.try(value)
            output << (result.success? && !Undefined.equal?(result.input) ? result.input : value)
          else
            output << value
          end
        end

        positions[input.size..].to_a.each do |position|
          output << position.type.call_unsafe(Undefined) if position.default?
        end

        output
      end
      # @api private
      def position_type_at(index)
        index < positions.size ? positions.fetch(index).type : rest.type
      end

      # @api private
      def arity_valid?(input)
        arity_error(input).nil?
      end

      # @return [TupleError, nil]
      #
      # @api private
      def arity_error(input)
        size = input.size
        min = @required_count
        max = rest ? nil : positions.size

        if size < min
          missing = (size...min).to_a
          message =
            if missing.size == 1
              "tuple is missing a value for position #{missing.first}"
            else
              "tuple is missing values for positions #{missing.join(", ")}"
            end
          TupleError.new(message, index: missing.first)
        elsif max && size > max
          extra = (max...size).to_a
          message =
            if extra.size == 1
              "tuple has an unexpected value at position #{extra.first}"
            else
              "tuple has unexpected values at positions #{extra.join(", ")}"
            end
          TupleError.new(message, index: extra.first)
        end
      end

      # @api private
      def element_error(index, exception)
        TupleError.new(
          "tuple position #{index} rejected value: #{exception.message}",
          index: index
        )
      end
    end
  end
end
