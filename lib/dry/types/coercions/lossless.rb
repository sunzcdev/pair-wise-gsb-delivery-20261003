# frozen_string_literal: true

require "bigdecimal"
require "bigdecimal/util"

module Dry
  module Types
    module Coercions
      # Lossless numeric coercions.
      #
      # Unlike the built-in {Coercible}, {Params}, and {JSON} coercions these
      # functions refuse to produce a value whose mathematical meaning differs
      # from the input:
      #
      # - The numeric representation is allowed to change (e.g. a string of
      #   digits may become an Integer) as long as the mathematical value is
      #   preserved exactly.
      # - Non-finite values (+-Infinity, NaN) are never accepted, regardless
      #   of whether they arrive as Float/BigDecimal objects or as strings.
      # - Negative zero is normalized to a positive zero (0.0 / 0), since the
      #   sign of zero does not carry mathematical meaning.
      # - Anything that cannot be represented exactly by the target type
      #   (e.g. 3.7 for Integer, 2**53 + 1 or "3.14" for Float) results in a
      #   {CoercionError}.
      #
      # @api public
      module Lossless
        extend Coercions

        # Standard errors raised by numeric parsing/arithmetic. They are
        # translated into {CoercionError} so callers have a single exception
        # type to rescue.
        NUMERIC_ERRORS = [
          ::ArgumentError, ::TypeError, ::NoMethodError,
          ::FloatDomainError, ::RangeError
        ].freeze

        # Lossless Integer coercion.
        #
        # Accepted inputs:
        # - {Integer}, passed through unchanged;
        # - {Float} and {BigDecimal} holding an integral value, e.g. 3.0;
        # - base-10 {String}s consisting solely of an integer literal,
        #   e.g. "3", "-17". A decimal fraction ("3.7", "3.0") is rejected
        #   because it is not an integer literal.
        #
        # Non-integral floats/decimals (3.7) are rejected instead of being
        # truncated towards zero like Kernel.Integer does.
        #
        # @param [Object] input
        #
        # @return [Integer]
        #
        # @raise CoercionError
        #
        # @api public
        def self.to_integer(input, &block)
          case input
          when ::Integer
            input
          when ::Float
            if input.finite? && integral_float?(input)
              input.to_i
            else
              reject(input, "integer", block)
            end
          when ::BigDecimal
            if input.finite? && integral_decimal?(input)
              input.to_i
            else
              reject(input, "integer", block)
            end
          when ::String
            begin
              Integer(input, 10)
            rescue *NUMERIC_ERRORS
              reject(input, "integer", block)
            end
          else
            reject(input, "integer", block)
          end
        end

        # Lossless Float coercion.
        #
        # Accepted inputs:
        # - finite {Float}s, passed through (-0.0 is normalized to 0.0);
        # - {Integer}s that have an exact binary64 representation, i.e. values
        #   that round-trip through Float without changing their value
        #   (all integers up to 2**53 in magnitude and larger powers of two);
        # - {BigDecimal}s that are exactly representable as binary64;
        # - decimal {String}s that parse to such an exactly representable
        #   finite float, e.g. "0.5", "3.0", "1_000". Fractional literals that
        #   are not exactly representable ("3.14") and overflow ("1e400")
        #   are rejected.
        #
        # @param [Object] input
        #
        # @return [Float] a finite float, never -0.0
        #
        # @raise CoercionError
        #
        # @api public
        def self.to_float(input, &block)
          case input
          when ::Float
            if input.finite?
              normalize_float(input)
            else
              reject(input, "float", block)
            end
          when ::Integer
            float = quiet_float(input)

            if float.is_a?(::Float) && float.finite? && float.to_r == input.to_r
              float
            else
              reject(input, "float", block)
            end
          when ::BigDecimal
            float = quiet_float(input)

            if float.is_a?(::Float) && float.finite? && float.to_r == input.to_r
              normalize_float(float)
            else
              reject(input, "float", block)
            end
          when ::String
            float = quiet_float(input)

            unless float.is_a?(::Float)
              return reject(input, "float", block)
            end

            decimal =
              begin
                BigDecimal(input)
              rescue *NUMERIC_ERRORS
                return reject(input, "float", block)
              end

            if float.finite? && float.to_r == decimal.to_r
              normalize_float(float)
            else
              reject(input, "float", block)
            end
          else
            reject(input, "float", block)
          end
        end

        # Lossless BigDecimal coercion.
        #
        # Accepted inputs:
        # - {BigDecimal}s, passed through (NaN/Infinity rejected, -0 is
        #   normalized to 0);
        # - {Integer}s, converted exactly regardless of magnitude;
        # - finite {Float}s, converted with {Float#to_d}. The conversion is
        #   accepted only when it round-trips back to the exact same float;
        # - decimal {String}s accepted by Kernel.BigDecimal, e.g.
        #   "3.7", "0.66666666666666666667", "1e999999". BigDecimal is a
        #   decimal representation so the mathematical value of decimal
        #   literals is preserved regardless of binary representability.
        #
        # @param [Object] input
        #
        # @return [BigDecimal] a finite decimal, never negative zero
        #
        # @raise CoercionError
        #
        # @api public
        def self.to_decimal(input, &block)
          case input
          when ::BigDecimal
            if input.finite?
              normalize_decimal(input)
            else
              reject(input, "decimal", block)
            end
          when ::Integer
            input.to_d
          when ::Float
            if input.finite?
              decimal = input.to_d
              if decimal.to_f == input
                normalize_decimal(decimal)
              else
                reject(input, "decimal", block)
              end
            else
              reject(input, "decimal", block)
            end
          when ::String
            begin
              decimal = BigDecimal(input)
            rescue *NUMERIC_ERRORS
              reject(input, "decimal", block)
            end

            if decimal.finite?
              normalize_decimal(decimal)
            else
              reject(input, "decimal", block)
            end
          else
            reject(input, "decimal", block)
          end
        end

        # Whether a finite float carries an integral value
        #
        # @param [Float] float
        #
        # @return [Boolean]
        #
        # @api private
        def self.integral_float?(float)
          float.to_r.denominator == 1
        end

        # Whether a finite BigDecimal carries an integral value
        #
        # @param [BigDecimal] decimal
        #
        # @return [Boolean]
        #
        # @api private
        def self.integral_decimal?(decimal)
          decimal % 1 == 0
        end

        # Turn -0.0 into 0.0
        #
        # @param [Float] float
        #
        # @return [Float]
        #
        # @api private
        def self.normalize_float(float)
          float.zero? ? 0.0 : float
        end

        # Turn negative zero decimals into positive zero
        #
        # @param [BigDecimal] decimal
        #
        # @return [BigDecimal]
        #
        # @api private
        def self.normalize_decimal(decimal)
          decimal.zero? ? BigDecimal("0") : decimal
        end

        # Convert to Float while suppressing the "out of range" warnings MRI
        # emits for overflows ("1e400", integers beyond Float range).
        #
        # @param [Integer, BigDecimal, String] input
        #
        # @return [Float, nil] the parsed float, or nil when unparseable
        #
        # @api private
        def self.quiet_float(input)
          verbose = $VERBOSE
          $VERBOSE = nil
          begin
            Float(input)
          rescue *NUMERIC_ERRORS
            nil
          ensure
            $VERBOSE = verbose
          end
        end

        # Invoke the failure callback, or raise {CoercionError}, on failure
        #
        # @param [Object] input
        # @param [String] target
        # @param [Proc, nil] block
        #
        # @return [Object] the result of the block when given
        #
        # @raise CoercionError
        #
        # @api private
        def self.reject(input, target, block)
          if block
            block.call
          else
            raise CoercionError,
                  "#{input.inspect} cannot be losslessly coerced to #{target}"
          end
        end

        private_class_method :integral_float?, :integral_decimal?,
                             :normalize_float, :normalize_decimal,
                             :quiet_float, :reject
      end
    end
  end
end
