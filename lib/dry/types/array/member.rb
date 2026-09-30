# frozen_string_literal: true

module Dry
  module Types
    class Array < Nominal
      # Member arrays define their member type that is applied to each element
      #
      # @api public
      class Member < Array
        # @return [Type]
        attr_reader :member

        # @param [Class] primitive
        # @param [Hash] options
        #
        # @option options [Type] :member
        #
        # @api private
        def initialize(primitive, **options)
          @member = options.fetch(:member)
          super
        end

        # @param [Object] input
        #
        # @return [Array]
        #
        # @api private
        def call_unsafe(input, options = EMPTY_HASH)
          if primitive?(input)
            return map_unsafe(input) unless options[:shared]

            map_unsafe_shared(input)
          else
            super
          end
        end

        # @param [Object] input
        # @return [Array]
        #
        # @api private
        def call_safe(input, options = EMPTY_HASH)
          if primitive?(input)
            return map_safe(input) { |*a| yield(*a) } unless options[:shared]

            map_safe_shared(input) { |*a| yield(*a) }
          else
            yield
          end
        end

        # @param [Array, Object] input
        # @param [#call,nil] block
        #
        # @yieldparam [Failure] failure
        # @yieldreturn [Result]
        #
        # @return [Result,Logic::Result]
        #
        # @api public
        def try(input, &block) # rubocop:disable Metrics/PerceivedComplexity
          if primitive?(input)
            output = []

            result = input.map { |el| member.try(el) }
            result.each do |r|
              output << r.input unless Undefined.equal?(r.input)
            end

            if result.all?(&:success?)
              success(output)
            else
              error = result.find(&:failure?).error
              failure = failure(output, error)
              block ? yield(failure) : failure
            end
          else
            failure = failure(input, CoercionError.new("#{input} is not an array"))
            block ? yield(failure) : failure
          end
        end

        # Build a lax type
        #
        # @return [Lax]
        #
        # @api public
        def lax = Lax.new(Member.new(primitive, **options, member: member.lax, meta: meta))

        # @see Nominal#to_ast
        #
        # @api public
        def to_ast(meta: true)
          if member.respond_to?(:to_ast)
            [:array, [member.to_ast(meta: meta), meta_ast(meta)]]
          else
            [:array, [member, meta_ast(meta)]]
          end
        end

        # @api private
        def constructor_type = ::Dry::Types::Array::Constructor

        private

        # Coerce every element into a freshly allocated array
        #
        # @api private
        def map_unsafe(input)
          input.each_with_object([]) do |el, output|
            append_coerced(output, member.call_unsafe(el))
          end
        end

        # Safe variant of {#map_unsafe}
        #
        # @api private
        def map_safe(input)
          failed = false

          result = input.each_with_object([]) do |el, output|
            coerced = member.call_safe(el) { |out = el|
              failed = true
              out
            }

            append_coerced(output, coerced)
          end

          failed ? yield(result) : result
        end

        # Coerce elements reusing the input array whenever every element
        # keeps its identity, order and retention
        #
        # @api private
        def map_unsafe_shared(input)
          map_shared(input) { |el| member.call_unsafe(el, SHARED_OPTIONS) }
        end

        # Safe variant of {#map_unsafe_shared}
        #
        # @api private
        def map_safe_shared(input)
          failed = false

          result = map_shared(input) do |el|
            member.call_safe(el, SHARED_OPTIONS) { |out = el|
              failed = true
              out
            }
          end

          failed ? yield(result) : result
        end

        # Coerce elements reusing the input array whenever every element
        # keeps its identity, order and retention. Elements are coerced
        # through the given block. The first element that is not identical
        # (using {Object#equal?}, never `==`) to the original, or that is
        # filtered out as Undefined, copies the prefix processed so far and
        # continues building a new array.
        #
        # @api private
        def map_shared(input)
          shared = true
          result = nil

          input.each_with_index do |el, index|
            coerced = yield(el)

            next if shared && coerced.equal?(el)

            if shared
              result = input.first(index)
              shared = false
            end

            append_coerced(result, coerced)
          end

          shared ? input : result
        end

        # Append a coerced value unless it was filtered out as Undefined
        #
        # @api private
        def append_coerced(output, coerced)
          output << coerced unless Undefined.equal?(coerced)
        end
      end
    end
  end
end
