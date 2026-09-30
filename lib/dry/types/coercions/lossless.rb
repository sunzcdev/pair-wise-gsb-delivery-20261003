# frozen_string_literal: true

require "bigdecimal"
require "bigdecimal/util"

module Dry
  module Types
    module Coercions
      # Lossless numeric coercions.
      #
      # Unlike the regular coercible types, these coercions guarantee that the
      # *mathematical value* of the input survives the conversion. The concrete
      # representation may change (e.g. a decimal string becomes a BigDecimal)
      # but converting the result back yields the same value. Any conversion
      # that would silently change the value is rejected with {CoercionError}.
      #
      # == Rules
      #
      # 1. Only +Integer+, +Float+, +BigDecimal+ and their common decimal
      #    string forms are accepted. Everything else fails.
      # 2. Non-finite values (+NaN+, +Infinity+, +-Infinity+) are always
      #    rejected, even by the identity coercion +Float -> Float+.
      # 3. Negative zero (-0.0, BigDecimal("-0")) is rejected everywhere. It
      #    carries a sign with no mathematical magnitude and is not
      #    representable by Integer or a regular (non-negative-zero)
      #    BigDecimal, so accepting it would be lossy for sum types made of
      #    these types. Positive zero is accepted.
      # 4. Fractions are never truncated into integers: "3.7" cannot become 3.
      # 5. A decimal value is converted to Float only when it has an exact
      #    binary64 representation. "3.7" therefore fails to coerce to Float,
      #    while "3.5" and binary-exact floats themselves succeed. Integers
      #    that cannot be represented exactly as floats (they round, or
      #    overflow to Infinity) are rejected.
      # 6. Float -> BigDecimal is always exact for finite floats: the full
      #    binary64 value is materialized as a BigDecimal. Note that the
      #    familiar-looking +Float#to_d+ (as provided by bigdecimal/util) is
      #    *not* exact — it rounds to {Float::DIG} decimal digits.
      # 7. Strings are parsed with a strict decimal grammar. Exponent notation
      #    is allowed for Float/BigDecimal inputs; hex floats (0x1p4),
      #    underscores ("1_000"), embedded whitespace, and the special tokens
      #    "Infinity"/"NaN" are not decimal strings and are rejected.
      #
      # @api public
      module Lossless
        extend Coercions

        # Number of significant decimal digits used when materializing a Float
        # as an exact BigDecimal.
        #
        # A binary64 denominator never exceeds 2**1074 (subnormals included),
        # which has 324 decimal digits. 1100 digits comfortably covers every
        # finite Float and keeps Rational#to_d from rounding.
        FLOAT_TO_DECIMAL_PRECISION = 1100

        # Strict integer string: optional sign followed by decimal digits
        INTEGER_STRING = /\A[+-]?\d+\z/.freeze

        # Strict decimal string: optional sign, an integer or fractional
        # mantissa (digits are required after the decimal point), and an
        # optional base-10 exponent.
        DECIMAL_STRING = /\A
          [+-]?
          (?:\d+(?:\.\d+)?|\.\d+)
          (?:e[+-]?\d+)?
        \z/ix.freeze

        # Losslessly coerce input to Integer
        #
        # @param [Integer, Float, BigDecimal, String] input
        #
        # @return [Integer]
        #
        # @raise [CoercionError] if the conversion is not value-preserving
        #
        # @api public
        def self.to_int(input, &)
          case input
          when ::Integer
            input
          when ::Float
            if finite_float?(input) && !negative_zero?(input) &&
               input.to_r == input.to_i
              input.to_i
            else
              fail(input, ::Integer, &)
            end
          when ::BigDecimal
            if input.finite? && integral_decimal?(input) && !negative_zero_decimal?(input)
              input.to_i
            else
              fail(input, ::Integer, &)
            end
          when ::String
            if INTEGER_STRING.match?(input)
              input.to_i
            else
              fail(input, ::Integer, &)
            end
          else
            fail(input, ::Integer, &)
          end
        rescue ::FloatDomainError, ::ArgumentError, ::TypeError
          fail(input, ::Integer, &)
        end

        # Losslessly coerce input to Float
        #
        # @param [Integer, Float, BigDecimal, String] input
        #
        # @return [Float]
        #
        # @raise [CoercionError] if the conversion is not value-preserving
        #
        # @api public
        def self.to_float(input, &)
          case input
          when ::Float
            if finite_float?(input) && !negative_zero?(input)
              input
            else
              fail(input, ::Float, &)
            end
          when ::Integer
            coerced = silently { input.to_f }
            if coerced.finite? && exact_float_decimal(coerced) == BigDecimal(input)
              coerced
            else
              fail(input, ::Float, &)
            end
          when ::BigDecimal
            if input.finite? && !negative_zero_decimal?(input)
              coerced = input.to_f
              if coerced.finite? && exact_float_decimal(coerced) == input
                coerced
              else
                fail(input, ::Float, &)
              end
            else
              fail(input, ::Float, &)
            end
          when ::String
            if DECIMAL_STRING.match?(input)
              # `Kernel.Float` parses exactly like `Float()`. The method form
              # is used because the parser reads `::Float(...)` as a local
              # variable reference after `when ::Float`.
              parsed = BigDecimal(input)
              coerced = silently { ::Kernel.Float(input) }
              if finite_float?(coerced) && !negative_zero?(coerced) &&
                 !negative_zero_decimal?(parsed) &&
                 exact_float_decimal(coerced) == parsed
                coerced
              else
                fail(input, ::Float, &)
              end
            else
              fail(input, ::Float, &)
            end
          else
            fail(input, ::Float, &)
          end
        rescue ::FloatDomainError, ::ArgumentError, ::TypeError
          fail(input, ::Float, &)
        end

        # Losslessly coerce input to BigDecimal
        #
        # @param [Integer, Float, BigDecimal, String] input
        #
        # @return [BigDecimal]
        #
        # @raise [CoercionError] if the conversion is not value-preserving
        #
        # @api public
        def self.to_decimal(input, &)
          case input
          when ::BigDecimal
            if input.finite? && !negative_zero_decimal?(input)
              input
            else
              fail(input, ::BigDecimal, &)
            end
          when ::Integer
            BigDecimal(input)
          when ::Float
            if finite_float?(input) && !negative_zero?(input)
              exact_float_decimal(input)
            else
              fail(input, ::BigDecimal, &)
            end
          when ::String
            if DECIMAL_STRING.match?(input)
              parsed = BigDecimal(input)
              if negative_zero_decimal?(parsed)
                fail(input, ::BigDecimal, &)
              else
                parsed
              end
            else
              fail(input, ::BigDecimal, &)
            end
          else
            fail(input, ::BigDecimal, &)
          end
        rescue ::FloatDomainError, ::ArgumentError, ::TypeError
          fail(input, ::BigDecimal, &)
        end

        class << self
          private

          # @return [Boolean]
          #
          # @api private
          def finite_float?(value)
            value.finite?
          end

          # Detects -0.0. `#zero?` alone cannot tell -0.0 apart from +0.0.
          #
          # @return [Boolean]
          #
          # @api private
          def negative_zero?(value)
            value.zero? && (1.0 / value) == -::Float::INFINITY
          end

          # BigDecimal represents signless zero with sign +1 and negative
          # zero with sign -1. `#negative?` reports false for negative zero,
          # hence the direct sign check.
          #
          # @return [Boolean]
          #
          # @api private
          def negative_zero_decimal?(value)
            value.zero? && value.sign == -1
          end

          # A BigDecimal is integral when the remainder of division by 1 is 0
          #
          # @return [Boolean]
          #
          # @api private
          def integral_decimal?(value)
            (value % 1).zero?
          end

          # The exact decimal expansion of a finite Float. Using the rational
          # representation guarantees the result is mathematically equal to the
          # float, unlike Float#to_d from bigdecimal/util (which rounds to
          # Float::DIG significant digits).
          #
          # @return [BigDecimal]
          #
          # @api private
          def exact_float_decimal(value)
            value.to_r.to_d(FLOAT_TO_DECIMAL_PRECISION)
          end

          # Integer#to_f warns ("Integer out of Float range") when the value
          # overflows to Infinity, and Kernel.Float warns ("Float 1e400 out
          # of range") on overflow. Such inputs are going to be rejected
          # anyway, so the warnings only add noise.
          #
          # @return [Float]
          #
          # @api private
          def silently
            verbose, $VERBOSE = $VERBOSE, nil
            yield
          ensure
            $VERBOSE = verbose
          end

          # @return [void]
          #
          # @api private
          def fail(input, target, &block)
            message =
              "#{input.inspect} cannot be losslessly coerced to #{target}"

            if block
              block.call
            else
              raise CoercionError, message
            end
          end
        end
      end
    end
  end
end
