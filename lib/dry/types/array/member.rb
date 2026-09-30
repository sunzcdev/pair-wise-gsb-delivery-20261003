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
        # @param [Object] context per-call context
        #
        # @return [Array]
        #
        # @api private
        def call_unsafe(input, context = Undefined)
          if primitive?(input)
            input.each_with_object([]) do |el, output|
              coerced = member.call_unsafe(el, context)

              output << coerced unless Undefined.equal?(coerced)
            end
          else
            super(input, context)
          end
        end

        # @param [Object] input
        # @return [Array]
        #
        # @api private
        def call_safe(input, context = Undefined)
          if primitive?(input)
            failed = false

            result = input.each_with_object([]) do |el, output|
              coerced = member.call_safe(el, context) { |out = el|
                failed = true
                out
              }

              output << coerced unless Undefined.equal?(coerced)
            end

            failed ? yield(result) : result
          else
            yield
          end
        end

        # @param [Array, Object] input
        # @param [Object] context per-call context
        # @param [#call,nil] block
        #
        # @yieldparam [Failure] failure
        # @yieldreturn [Result]
        #
        # @return [Result,Logic::Result]
        #
        # @api public
        def try(input, context = Undefined, &block) # rubocop:disable Metrics/PerceivedComplexity
          if primitive?(input)
            output = []

            result = input.map { |el| member.try(el, context) }
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
      end
    end
  end
end
