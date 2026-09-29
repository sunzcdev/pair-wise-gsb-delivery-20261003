# frozen_string_literal: true

module I18n
  # A pluralization count that preserves the decimal representation of the
  # original input.
  #
  # When a count is given as text (e.g. a form parameter), the strings "1",
  # "1.0" and "1.00" all denote the same numeric value but carry different
  # visible precision. Coercing them to a Float on the way to the
  # pluralization rule would destroy that information (and the precision of
  # long fractions). I18n::Count keeps the original decimal representation
  # instead and exposes it through read-only operands:
  #
  #   count = I18n::Count.new("-1.00")
  #   count.integer_part    # => 1
  #   count.fraction_part   # => "00"
  #   count.fraction_digits # => 2
  #   count.negative?       # => true
  #   count.to_s            # => "-1.00"
  #
  # A Count interpolates as its original text, so it can be used for
  # %{count} placeholders directly.
  #
  # Pluralization rules that do not know about I18n::Count keep working:
  # numeric messages (e.g. +%+, +zero?+, +<=>+) are answered by the
  # equivalent plain numeric. Rules can also explicitly unwrap a count
  # through the compatibility entry points I18n::Count.numeric(count) or
  # count.to_numeric, both of which return an Integer or a Rational -
  # never a Float.
  class Count
    include Comparable

    PATTERN = /\A\s*(?<sign>[+-])?(?<integer>\d+)(?:\.(?<fraction>\d+))?\s*\z/

    class << self
      # Wraps +value+ in a Count when it carries a decimal representation
      # worth preserving (decimal text, BigDecimal when loaded). Anything else -
      # including text that is not a plain decimal literal - is returned
      # untouched.
      def parse(value)
        case value
        when Count
          value
        when String
          PATTERN.match?(value) ? new(value) : value
        else
          if defined?(BigDecimal) && value.is_a?(BigDecimal)
            new(value.to_s('F'))
          else
            value
          end
        end
      end

      # Compatibility entry for pluralization rules that do not know about
      # I18n::Count: unwraps a Count to a plain numeric (Integer or
      # Rational, never a Float) and passes any other object through
      # untouched.
      def numeric(value)
        value.is_a?(Count) ? value.to_numeric : value
      end
    end

    # The integer digits of the value, as an Integer (unsigned, CLDR +i+).
    attr_reader :integer_part

    # The visible fraction digits, as a String ("" when the input had no
    # decimal point).
    attr_reader :fraction_part

    def initialize(text)
      match = PATTERN.match(text.to_s) or
        raise(I18n::ArgumentError, "#{text.inspect} is not a valid decimal count")

      @negative      = match[:sign] == '-'
      @integer_digits = match[:integer].freeze
      @fraction_part = (match[:fraction] || '').freeze
      @integer_part  = @integer_digits.to_i

      @string = if @fraction_part.empty?
        @negative ? "-#{@integer_digits}" : @integer_digits
      else
        "#{@negative ? '-' : ''}#{@integer_digits}.#{@fraction_part}"
      end.freeze

      freeze
    end

    # The number of visible fraction digits (CLDR +v+). This is what
    # distinguishes "1", "1.0" and "1.00".
    def fraction_digits
      @fraction_part.length
    end

    # The visible fraction digits as an Integer (CLDR +f+).
    def fraction_value
      @fraction_part.to_i
    end

    def negative?
      @negative
    end

    def zero?
      to_numeric.zero?
    end

    # -1, 0 or 1, matching the sign of the value.
    def sign
      to_numeric <=> 0
    end

    # The original decimal representation, e.g. "-1.00".
    def to_s
      @string
    end

    def inspect
      "#<#{self.class.name} #{@string}>"
    end

    def to_i
      @negative ? -@integer_part : @integer_part
    end

    def to_f
      @string.to_f
    end

    # The exact value as a Rational, e.g. Rational(-3, 2) for "-1.5".
    # Computed from the decimal digits, so no precision is lost.
    def to_r
      value = Rational(@integer_digits)
      unless @fraction_part.empty?
        value += Rational(@fraction_part.to_i, 10**@fraction_part.length)
      end
      @negative ? -value : value
    end

    # The value as a plain numeric: an Integer when the input had no
    # fraction digits, a Rational otherwise. Never a Float, so no
    # precision or representation is lost.
    def to_numeric
      @fraction_part.empty? ? to_i : to_r
    end

    def ==(other)
      case other
      when Count then to_r == other.to_r
      when Numeric then to_numeric == other
      else false
      end
    end

    def <=>(other)
      case other
      when Count then to_r <=> other.to_r
      when Numeric then to_numeric <=> other
      end
    end

    def eql?(other)
      other.is_a?(Count) ? to_numeric.eql?(other.to_numeric) : to_numeric.eql?(other)
    end

    def hash
      to_numeric.hash
    end

    def coerce(other)
      [other, to_numeric]
    end

    # Compatibility for legacy pluralization rules: numeric messages that
    # are not defined explicitly (e.g. +%+, +modulo+, +abs+, +floor+) are
    # answered by the plain numeric equivalent.
    def method_missing(name, *args, &block)
      numeric = to_numeric
      numeric.respond_to?(name) ? numeric.public_send(name, *args, &block) : super
    end

    def respond_to_missing?(name, include_private = false)
      to_numeric.respond_to?(name, include_private) || super
    end
  end
end
