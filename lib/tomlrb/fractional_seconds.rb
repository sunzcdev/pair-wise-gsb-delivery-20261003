# frozen-string-literal: true

module Tomlrb
  # Renders the fractional part of a second without going through
  # binary floating point, so values such as 59.999999999999 are
  # never rounded up into the next second.
  module FractionalSeconds
    private

    def fractional_second_string(frac)
      return '' if frac.zero?
      return frac.to_s[1..-1] unless frac.is_a?(Rational)

      digits = decimal_digits(frac)
      digits.empty? ? '' : ".#{digits}"
    end

    # A fraction parsed from a finite decimal string always reduces to
    # a denominator of the form 2**a * 5**b, so it can be scaled to an
    # exact power of ten and rendered digit by digit without rounding.
    def decimal_digits(rational)
      numerator = rational.numerator
      denominator = rational.denominator
      twos = 0
      while denominator.even?
        denominator /= 2
        twos += 1
      end
      fives = 0
      while (denominator % 5).zero?
        denominator /= 5
        fives += 1
      end
      raise ArgumentError, "Cannot express #{rational} as a finite decimal" unless denominator == 1

      width = [twos, fives].max
      numerator *= 2**(width - twos) * 5**(width - fives)
      numerator.to_s.rjust(width, '0').sub(/0+\z/, '')
    end
  end
end
