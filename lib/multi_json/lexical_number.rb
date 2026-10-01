module MultiJson
  # A JSON number that remembers its exact original spelling.
  #
  # Produced by {MultiJson.load} when <tt>:preserve_numbers</tt> is true.
  # Unlike a Float, it never loses precision and keeps the distinction between
  # equivalent spellings such as <tt>"1.2300"</tt>, <tt>"1.23"</tt> and
  # <tt>"123e-2"</tt>. Unlike a String, it is a Numeric value, so dumping it
  # emits an unquoted JSON number rather than a string.
  #
  #   n = MultiJson.load("1.2300", preserve_numbers: true)
  #   n.class        # => MultiJson::LexicalNumber
  #   n.lexeme       # => "1.2300"
  #   n.to_r         # => (123/100)
  #   n.to_d.to_s    # => "0.123e1"
  #   MultiJson.dump(n) # => "1.2300"
  #
  # Use {#to_r}, {#to_d} or {#to_i} to obtain exact Ruby numerics. {#to_f} is
  # provided for interoperability but may round, just like Float itself.
  class LexicalNumber < Numeric
    # A complete, anchored JSON number grammar (RFC 8259). The text is checked
    # against it before it can ever be emitted, which makes the value a safe
    # channel: arbitrary text (quotes, braces, code) is rejected here and can
    # never be injected into generated JSON.
    GRAMMAR = /\A-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?\z/.freeze

    # Matches the numeric parts of a valid lexeme. GRAMMAR must already match.
    DECOMPOSITION = /\A(-)?(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?\z/.freeze

    # The exact, frozen JSON text of the number, e.g. "123e-2".
    attr_reader :lexeme
    alias_method :to_s, :lexeme

    # @param text [#to_str] the exact JSON number text
    # @raise [ArgumentError] if +text+ is not a valid JSON number
    def initialize(text)
      @lexeme = self.class.send(:validate!, text)
      @rational = rational_value
      freeze
    end

    # Builds a value for a tokenizer that already hands over number text. The
    # text is still validated against {GRAMMAR}, so a misbehaving tokenizer
    # cannot smuggle arbitrary text through this path either. @api private
    def self.from_lexeme(text)
      allocate.tap do |number|
        number.instance_variable_set(:@lexeme, validate!(text))
        number.instance_variable_set(:@rational, number.send(:rational_value))
        number.freeze
      end
    end

    # Builds a value from an already exact Integer. Backends such as the json
    # gem hand back arbitrary-precision Integers for integral JSON numbers but
    # do not expose their spelling, so the canonical spelling is used instead.
    # Precision is still exact; only leading zeros that JSON forbids anyway are
    # absent. @api private
    def self.from_integer(value)
      from_lexeme(value.to_s)
    end

    # @see #initialize
    def self.[](text)
      new(text)
    end

    class << self
      private

      # Validates +text+ against {GRAMMAR} and returns a frozen copy.
      def validate!(text)
        text = String.try_convert(text) or
          raise TypeError, "no implicit conversion of #{text.class} into String"
        unless GRAMMAR.match?(text)
          raise ArgumentError, "not a valid JSON number: #{text.inspect}"
        end

        text.dup.freeze
      end
    end

    # True only when the number denotes a mathematical integer
    # (e.g. "12", "1.0", "1e3"); false for values with a fractional part.
    def integer?
      to_r.denominator == 1
    end

    # Exact conversion to a Rational. Works for any precision and any exponent.
    # @return [Rational]
    def to_r
      @rational
    end

    # Exact conversion to an Integer.
    # @raise [RangeError] if the number is not an integral value
    # @return [Integer]
    def to_i
      rational = to_r
      unless rational.denominator == 1
        raise RangeError, "#{lexeme.inspect} is not an integer"
      end

      rational.numerator
    end
    alias_method :to_int, :to_i

    # Exact decimal conversion. Requires the bigdecimal standard library.
    # @return [BigDecimal]
    def to_d
      require "bigdecimal"
      BigDecimal(lexeme)
    end

    # Conversion to Float. This may lose precision; use {#to_r} or {#to_d}
    # when exactness matters.
    # @return [Float]
    def to_f
      Float(lexeme)
    end

    # Emits the original number text as raw JSON. Because the lexeme was
    # validated against {GRAMMAR}, this output is always a legal number and can
    # never contain quotes or arbitrary text.
    # @return [String]
    def to_json(*)
      lexeme
    end

    # Numeric equality compares exact mathematical values (never through
    # Float), so 1.0 == 1.00. Use {#eql?} to also require equal spelling.
    def ==(other)
      other.is_a?(Numeric) && to_r == other.to_r
    end

    # Hash identity is the exact spelling: 1.0 and 1.00 stay distinguishable.
    def eql?(other)
      other.is_a?(LexicalNumber) && lexeme.eql?(other.lexeme)
    end

    def hash
      [self.class, lexeme].hash
    end

    def <=>(other)
      return nil unless other.is_a?(Numeric)

      to_r <=> other.to_r
    end

    # Lets native Numerics compare against this value (1 == number) without
    # going through Float.
    def coerce(other)
      [other.to_r, to_r]
    end

    def inspect
      "#<#{self.class}:#{lexeme}>"
    end

    private

    def rational_value
      sign, int, frac, exp = parts
      fraction_digits = frac ? frac.length : 0
      digits = int + (frac || "")
      # Unscaled digits over 10^fraction_digits, shifted by the exponent.
      value = Rational(digits.to_i) * Rational(1, 10**fraction_digits) * (10**exp.to_i)
      sign == "-" ? -value : value
    end

    def parts
      DECOMPOSITION.match(lexeme).captures
    end
  end
end
