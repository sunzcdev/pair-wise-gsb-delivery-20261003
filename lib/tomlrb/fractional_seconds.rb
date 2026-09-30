# frozen-string-literal: true

module Tomlrb
  # Parses and formats fractional seconds without going through a binary
  # floating point value, so a value such as 23:59:59.999999999999 keeps its
  # exact magnitude instead of drifting or being carried over to the next
  # second.
  module FractionalSeconds
    module_function

    # @param sec_string [String] whole seconds, optionally followed by
    #   +.+ and one or more fractional digits (e.g. +"59"+, +"00.999999999999"+)
    # @return [Array(Integer, Rational, String)] the whole seconds, the
    #   exact fractional part as a Rational in [0, 1) and the canonical
    #   fractional digits (trailing zeroes removed); the digits are an
    #   empty string when the fraction is zero.
    def parse(sec_string)
      whole, frac_digits = sec_string.split('.', 2)
      whole = whole.to_i
      return [whole, Rational(0), ''] if frac_digits.nil?

      digits = frac_digits.sub(/0+\z/, '')
      return [whole, Rational(0), ''] if digits.empty?

      fraction = Rational(digits.to_i, 10**digits.length)
      [whole, fraction, digits]
    end

    # @param fraction [Rational] exact fractional part in [0, 1)
    # @param digits [String] canonical fractional digits returned by {parse}
    # @return [String] +""+ for a zero fraction, otherwise +"."+ followed by
    #   the canonical fractional digits.
    def suffix(fraction, digits)
      fraction.zero? ? '' : ".#{digits}"
    end

    # Truncated (never rounded up) nanoseconds of an exact fraction, so a
    # value just below the next second does not carry over.
    def nsec(fraction)
      (fraction * 1_000_000_000).to_i
    end

    # Truncated microseconds of an exact fraction.
    def usec(fraction)
      (fraction * 1_000_000).to_i
    end
  end
end
