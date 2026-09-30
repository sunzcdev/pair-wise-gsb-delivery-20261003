# frozen_string_literal: true

require "dry/initializer"
require "dry/validation/constants"

module Dry
  module Validation
    class Macro < Function
      # Declares the expected arguments of a macro
      #
      # Signatures are optional. Macros without a signature are not checked
      # and keep their historical behavior.
      #
      # @example
      #   register_macro(:min, signature: {required: [:num], keywords: [:message]}) do |macro:|
      #     key.failure("must have at least #{macro.args[0]} items") if value.size < macro.args[0]
      #   end
      #
      # @see Macros::Container#register
      #
      # @api public
      class Signature
        extend Dry::Initializer

        # @!attribute [r] required
        #   Number or names of required positional arguments
        #   @return [Integer, Array<Symbol>]
        #   @api public
        option :required, default: proc { 0 }

        # @!attribute [r] optional
        #   Number or names of optional positional arguments
        #   @return [Integer, Array<Symbol>]
        #   @api public
        option :optional, default: proc { 0 }

        # @!attribute [r] keywords
        #   Names of supported optional keyword arguments
        #   @return [Array<Symbol>]
        #   @api public
        option :keywords, default: proc { EMPTY_ARRAY }

        # @!attribute [r] required_keywords
        #   Names of required keyword arguments
        #   @return [Array<Symbol>]
        #   @api public
        option :required_keywords, default: proc { EMPTY_ARRAY }

        # @!attribute [r] macros
        #   Names of other macros this macro expands into via `Evaluator#run_macro`
        #   @return [Array<Symbol>]
        #   @api public
        option :macros, default: proc { EMPTY_ARRAY }

        # Coerce a signature spec into a Signature
        #
        # @param [Hash, Signature] spec
        #
        # @return [Signature]
        #
        # @api private
        def self.[](spec)
          spec.is_a?(self) ? spec : new(**spec)
        end

        # Check macro call arguments against the signature
        #
        # When the signature declares keywords, a trailing Hash argument is
        # treated as keyword arguments, otherwise it remains positional.
        #
        # @param [Array] args
        #
        # @return [Array<String>] list of problems, empty when the call is valid
        #
        # @api private
        def validate_call(args)
          positional, kwargs = split_args(args)

          [].tap do |problems|
            validate_arity(positional, problems)
            validate_keywords(kwargs, problems)
          end
        end

        # @api private
        def required_arity
          arity_of(required)
        end

        # @api private
        def max_arity
          required_arity + arity_of(optional)
        end

        # @api private
        def accepts_keywords?
          keywords.any? || required_keywords.any?
        end

        private

        # @api private
        def split_args(args)
          if accepts_keywords? && args.last.is_a?(::Hash)
            [args[0..-2], args.last]
          else
            [args, EMPTY_HASH]
          end
        end

        # @api private
        def validate_arity(positional, problems)
          count = positional.size
          return if count >= required_arity && count <= max_arity

          problems << "wrong number of arguments (given #{count}, expected #{arity_description})"
        end

        # @api private
        def validate_keywords(kwargs, problems)
          unknown = kwargs.keys - keywords - required_keywords
          if unknown.any?
            problems << "unknown keywords: #{unknown.map(&:inspect).join(', ')}"
          end

          missing = required_keywords - kwargs.keys
          if missing.any?
            problems << "missing required keywords: #{missing.map(&:inspect).join(', ')}"
          end
        end

        # @api private
        def arity_description
          if max_arity.equal?(required_arity)
            required_arity.to_s
          else
            "#{required_arity}..#{max_arity}"
          end
        end

        # @api private
        def arity_of(spec)
          spec.is_a?(::Array) ? spec.size : spec
        end
      end
    end
  end
end
