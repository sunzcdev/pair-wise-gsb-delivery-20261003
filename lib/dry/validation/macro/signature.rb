# frozen_string_literal: true

require "dry/validation/constants"

module Dry
  module Validation
    class Macro < Function
      # An optional declarative signature for a macro
      #
      # When a macro is registered with a signature, every call to the macro
      # is checked against it when the contract definition is completed (and
      # again when the macro is expanded), instead of blowing up on the first
      # validated request. Macros registered without a signature keep the
      # legacy behavior and are never checked.
      #
      # @example
      #   register_macro(:min_size, signature: {required: 1, keywords: [:inclusive]}) do |macro:|
      #     # ...
      #   end
      #
      # @api public
      class Signature
        # @!attribute [r] required
        #   @return [Integer] number of required positional arguments
        #   @api public
        attr_reader :required

        # @!attribute [r] optional
        #   @return [Integer] number of optional positional arguments
        #   @api public
        attr_reader :optional

        # @!attribute [r] keywords
        #   @return [Array<Symbol>] names of allowed keyword arguments
        #   @api public
        attr_reader :keywords

        # Build a signature from a registration-time declaration
        #
        # @param [Hash, Signature, nil] declaration
        #
        # @return [Signature, nil] nil when no signature was declared
        #
        # @api private
        def self.build(declaration)
          case declaration
          when nil then nil
          when self then declaration
          when ::Hash then new(**declaration)
          else
            raise ::ArgumentError, "invalid macro signature declaration: #{declaration.inspect}"
          end
        end

        # @param [Integer] required number of required positional arguments
        # @param [Integer] optional number of optional positional arguments
        # @param [Array<Symbol>] keywords names of allowed keyword arguments
        #
        # @api public
        def initialize(required: 0, optional: 0, keywords: EMPTY_ARRAY)
          unless required.is_a?(::Integer) && optional.is_a?(::Integer) &&
                 required >= 0 && optional >= 0
            raise ::ArgumentError, "required and optional must be non-negative integers"
          end

          @required = required
          @optional = optional
          @keywords = keywords.map(&:to_sym).freeze
        end

        # Check macro call arguments against the signature
        #
        # When the signature declares keywords, a trailing Hash argument is
        # treated as keyword arguments. Otherwise all arguments are treated
        # as positional.
        #
        # @param [Array] args the arguments the macro was called with
        #
        # @return [String, nil] a description of the mismatch, or nil when valid
        #
        # @api private
        def error_for(args)
          positional, kwargs = split_args(args)

          return arity_error(positional.size) unless arity.cover?(positional.size)

          unknown = kwargs.keys - keywords
          "unknown keyword arguments: #{unknown.map(&:inspect).join(', ')}" if unknown.any?
        end

        private

        # @api private
        def split_args(args)
          if keywords.any? && args.last.is_a?(::Hash)
            [args[0..-2], args.last]
          else
            [args, EMPTY_HASH]
          end
        end

        # @api private
        def arity
          required..(required + optional)
        end

        # @api private
        def arity_error(size)
          expected = optional.zero? ? required.to_s : "#{required}..#{required + optional}"
          "wrong number of arguments (expected #{expected}, got #{size})"
        end
      end
    end
  end
end
