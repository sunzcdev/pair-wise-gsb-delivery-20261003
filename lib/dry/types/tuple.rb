# frozen_string_literal: true

module Dry
  module Types
    # Tuple describes an array whose positions are typed individually,
    # as opposed to {Array::Member} where every element shares one type.
    #
    # Positions are declared as a fixed prefix of required slots,
    # optionally followed by optional tail slots and, at most,
    # one separately declared repeated tail.
    #
    # @example
    #   tuple = Dry::Types["nominal.array"].tuple(
    #     Dry::Types["strict.symbol"],
    #     Dry::Types["strict.integer"],
    #     Dry::Types["strict.string"].omittable
    #   )
    #
    #   tuple.([:create, 1, "v1"])
    #   # => [:create, 1, "v1"]
    #
    # @api public
    class Tuple < Array
      # Validate and normalize a list of slot declarations
      #
      # A slot declared after the first optional slot must be optional as
      # well, otherwise its presence cannot be determined unambiguously.
      # At most one slot may be declared as a repeated tail and it must
      # come last.
      #
      # @param [Array] declarations
      #
      # @raise [TupleDeclarationError]
      #
      # @return [Array<Slot>]
      #
      # @api private
      #
      # rubocop:disable Metrics/PerceivedComplexity
      def self.normalize(declarations)
        slots = declarations.map { |decl|
          if Slot === decl
            decl
          else
            type = resolve(decl)
            Slot.new(type, optional: type.default?)
          end
        }

        seen_optional = false
        rest_seen = false

        slots.each do |slot|
          if rest_seen
            raise TupleDeclarationError,
              "a repeated tail must be the last slot of a tuple"
          end

          if slot.required? && !slot.rest? && seen_optional
            raise TupleDeclarationError,
              "a required slot cannot follow an optional slot " \
              "in a tuple declaration, as such a position is ambiguous"
          end

          seen_optional ||= slot.optional?
          rest_seen = slot.rest?
        end

        slots
      end
      # rubocop:enable Metrics/PerceivedComplexity

      # Resolve a type-like value into a type
      #
      # @api private
      def self.resolve(type)
        case type
        when Type then type
        when ::Class, ::String then Types[type]
        else type
        end
      end

      # @return [Array<Slot>]
      attr_reader :slots

      # @param [Class] primitive
      # @param [Hash] options
      #
      # @option options [Array<Slot>] :slots
      #
      # @api private
      def initialize(primitive, slots:, **options)
        @slots = slots
        super(primitive, **options, slots: slots)
      end

      # @return [String]
      #
      # @api public
      def name = "Tuple"

      # @return [Boolean]
      #
      # @api public
      def constrained? = slots.any? { |slot| slot.type.constrained? }

      # @param [Array] input
      #
      # @return [Array]
      #
      # @api private
      def call_unsafe(input)
        try(input) { |failure|
          raise TupleError.new(failure.error.message, index: failure.error.index)
        }.input
      end

      # @param [Array] input
      #
      # @return [Array]
      #
      # @api private
      def call_safe(input)
        try(input) { |failure|
          return yield(failure.input)
        }.input
      end

      # @param [Array] input
      # @param [#call,nil] block
      #
      # @yieldparam [Failure] failure
      # @yieldreturn [Result]
      #
      # @return [Result]
      #
      # @api public
      def try(input, &block)
        if primitive?(input)
          coerce(input, &block)
        else
          failure = failure(
            input,
            TupleError.new(
              "#{input.inspect} must be an instance of #{primitive}",
              index: nil
            )
          )
          block ? yield(failure) : failure
        end
      end

      # Build a lax type
      #
      # @return [Lax]
      #
      # @api public
      def lax
        Lax.new(
          Tuple.new(
            primitive,
            **options,
            slots: slots.map { |slot| Slot.new(slot.type.lax, optional: slot.optional?, rest: slot.rest?) }
          )
        )
      end

      # @see Nominal#to_ast
      #
      # @api public
      def to_ast(meta: true)
        [:tuple, [slots.map { |slot| slot.to_ast(meta: meta) }, meta_ast(meta)]]
      end

      private

      # Number of slots that must be present in the input
      #
      # @return [Integer]
      #
      # @api private
      def min_size
        slots.count { |slot| slot.required? && !slot.rest? }
      end

      # Maximum number of elements the input may contain. Returns nil when
      # a repeated tail absorbs any number of elements.
      #
      # @return [Integer,nil]
      #
      # @api private
      def max_size
        slots.size if slots.none?(&:rest?)
      end

      # Coerce a value known to be an array
      #
      # @param [Array] input
      #
      # @return [Result]
      #
      # @api private
      #
      # rubocop:disable Metrics/AbcSize
      # rubocop:disable Metrics/PerceivedComplexity
      def coerce(input)
        if input.size < min_size
          error = TupleError.new(
            "#{input.inspect} has too few elements for the tuple " \
            "(#{input.size} given, at least #{min_size} required)",
            index: input.size
          )
          failure = failure(input, error)
          return block_given? ? yield(failure) : failure
        end

        if max_size && input.size > max_size
          error = TupleError.new(
            "#{input.inspect} has too many elements for the tuple " \
            "(#{input.size} given, at most #{max_size} allowed)",
            index: max_size
          )
          failure = failure(input, error)
          return block_given? ? yield(failure) : failure
        end

        output = []
        offset = 0

        slots.each do |slot|
          if slot.rest?
            consumed = rest_count(input.size)
            (offset...(offset + consumed)).each do |index|
              result = slot.type.try(input[index])

              unless result.success?
                failure = failure(output, TupleError.new(result.error.message, index: index))
                return block_given? ? yield(failure) : failure
              end

              output << result.input
            end
            offset += consumed
          elsif offset < input.size
            result = slot.type.try(input[offset])

            unless result.success?
              failure = failure(output, TupleError.new(result.error.message, index: offset))
              return block_given? ? yield(failure) : failure
            end

            output << result.input
            offset += 1
          elsif slot.default?
            output << slot.type.call_unsafe(Undefined)
          elsif slot.required?
            error = TupleError.new(
              "missing required element at position #{offset}",
              index: offset
            )
            failure = failure(output, error)
            return block_given? ? yield(failure) : failure
          end
        end

        success(output)
      end
      # rubocop:enable Metrics/AbcSize
      # rubocop:enable Metrics/PerceivedComplexity

      # Number of elements consumed by the repeated tail
      #
      # @param [Integer] size
      #
      # @return [Integer]
      #
      # @api private
      def rest_count(size)
        fixed = slots.reject(&:rest?).size
        size - fixed
      end
    end
  end
end
