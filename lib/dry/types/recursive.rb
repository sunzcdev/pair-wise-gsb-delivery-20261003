# frozen_string_literal: true

module Dry
  module Types
    # A named reference to a type defined within a {Recursive::Scope}
    #
    # References are resolved lazily when the type is applied, which
    # allows a group of types to reference themselves and each other
    # without unfolding the (potentially infinite) type tree upfront.
    #
    # @see Dry::Types.recursive
    #
    # @api public
    class Recursive
      include Type
      include Options
      include Meta
      include Builder
      include Printable
      include ::Dry::Equalizer(:name, :options, :meta, inspect: false, immutable: true)

      # @return [Symbol] name of the referenced type
      attr_reader :name

      # @return [Recursive::Scope] scope the reference belongs to
      attr_reader :scope

      # @param [Symbol] name
      # @param [Recursive::Scope] scope
      # @param [Hash] options
      #
      # @api private
      def initialize(name, scope, **options)
        super(name, scope, **options)
        @name = name.to_sym
        @scope = scope
        freeze
      end

      # Resolve the reference to its definition within the scope
      #
      # @return [Type]
      #
      # @raise [RecursiveDefinitionError] when the name is not defined
      #
      # @api public
      def resolve = scope.resolve(name)

      # @param [Object] input
      #
      # @return [Object]
      #
      # @api private
      def call_unsafe(input)
        Guard.guard(name, input) { resolve.call_unsafe(input) }
      end

      # @param [Object] input
      #
      # @return [Object]
      #
      # @api private
      def call_safe(input, &block)
        Guard.guard(name, input) { resolve.call_safe(input, &block) }
      end

      # @param [Object] input
      #
      # @yieldparam [Failure] failure
      #
      # @return [Result, Logic::Result]
      #
      # @api public
      def try(input, &block)
        Guard.guard(name, input) { resolve.try(input, &block) }
      rescue RecursiveInputError => error
        failure = failure(input, error)
        block ? yield(failure) : failure
      end

      # @return [Boolean]
      #
      # @api public
      def default?
        resolve.default?
      rescue RecursiveDefinitionError
        false
      end

      # @return [Boolean]
      #
      # @api public
      def constrained?
        resolve.constrained?
      rescue RecursiveDefinitionError
        false
      end

      # @return [Boolean]
      #
      # @api public
      def optional?
        resolve.optional?
      rescue RecursiveDefinitionError
        false
      end

      # @param [Object] value
      #
      # @return [Boolean]
      #
      # @api public
      def primitive?(value)
        resolve.primitive?(value)
      rescue RecursiveDefinitionError
        false
      end

      # References are exported by name, so the AST of a recursive
      # type is always finite
      #
      # @param [Boolean] meta whether to include meta in the output
      #
      # @return [Array]
      #
      # @api public
      def to_ast(meta: true)
        [:recursive, [name, meta ? self.meta : EMPTY_HASH]]
      end

      # Wrap the type with a proc
      #
      # @return [Proc]
      #
      # @api public
      def to_proc = proc { |value| self.(value) }

      # @param [Object] input
      #
      # @return [Result::Success]
      #
      # @api public
      def success(input) = Result::Success.new(input)

      # @param [Object] input
      # @param [CoercionError] error
      #
      # @return [Result::Failure]
      #
      # @api public
      def failure(input, error)
        raise ::ArgumentError, "error must be a CoercionError" unless error.is_a?(CoercionError)

        Result::Failure.new(input, error)
      end
    end
  end
end
