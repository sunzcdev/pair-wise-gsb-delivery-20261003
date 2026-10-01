module MultiJson
  # A JSON number preserved in its original lexical form.
  #
  # Financial data often contains numbers that do not survive a round trip
  # through Float (e.g. 1.2300 vs 123e-2, or integers beyond 2**53).
  # Loading with <tt>raw_numbers: true</tt> (supported by the OkJson
  # adapter) yields RawNumber instances instead of Integer/Float, keeping
  # the exact source text available via #to_s.
  #
  # A RawNumber is not a String and not a Numeric on purpose: it cannot be
  # confused with a JSON string, and it never silently goes through Float.
  # Convert explicitly with #to_i, #to_r or #to_d.
  #
  # The wrapped text is validated against the JSON number grammar
  # (RFC 8259) at construction time, so dumping a RawNumber always emits a
  # legal, unquoted JSON number and can never inject arbitrary text.
  class RawNumber
    # Grammar of a JSON number as defined by RFC 8259, section 6.
    PATTERN = /\A-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?\z/

    # Decomposition of a valid JSON number into sign, integer digits,
    # fraction digits and exponent.
    DECOMPOSITION = /\A(-?)(\d+)(?:\.(\d+))?(?:[eE]([+-]?\d+))?\z/

    class << self
      # Returns true if the given string is a syntactically valid JSON
      # number and could therefore be wrapped by RawNumber.
      def valid?(string)
        string.is_a?(::String) && PATTERN.match?(string)
      end
    end

    def initialize(string)
      unless self.class.valid?(string)
        raise ArgumentError, "invalid JSON number literal: #{string.inspect}"
      end

      @string = string.dup.freeze
      freeze
    end

    # The original number text, exactly as it appeared in the source.
    def to_s
      @string
    end

    def inspect
      "#<#{self.class.name} #{@string}>"
    end

    # Dumps as the bare number literal, never as a quoted string.
    # The text was validated at construction time, so this is always a
    # legal JSON number and cannot inject arbitrary content.
    def to_json(*)
      @string
    end

    def ==(other)
      other.is_a?(RawNumber) && other.to_s == @string
    end
    alias_method :eql?, :==

    def hash
      [self.class, @string].hash
    end

    # Explicit conversion to Integer. Fractional parts are truncated
    # toward zero, like Numeric#to_i.
    def to_i
      to_r.to_i
    end

    # Explicit, exact conversion to Rational. Never goes through Float.
    def to_r
      _, sign, integer_digits, fraction_digits, exponent = *DECOMPOSITION.match(@string)

      digits = integer_digits + (fraction_digits || "")
      power = (exponent || "0").to_i - (fraction_digits ? fraction_digits.length : 0)
      numerator = digits.to_i
      numerator = -numerator if sign == "-"

      if power >= 0
        Rational(numerator * (10**power), 1)
      else
        Rational(numerator, 10**(-power))
      end
    end

    # Explicit, exact conversion to BigDecimal. Never goes through Float.
    def to_d
      require "bigdecimal"
      BigDecimal(@string)
    end
  end
end
