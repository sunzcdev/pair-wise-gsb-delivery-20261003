# frozen_string_literal: true

module I18n
  # A pluralization count that preserves the exact decimal representation of
  # its (usually textual) input.
  #
  # Plain Ruby numerics cannot tell "1", "1.0" and "1.00" apart: as soon as
  # user input is parsed into an Integer or a Float the number of visible
  # fraction digits is lost. Some pluralization rules care about exactly
  # that information.
  #
  #   count = I18n::Count.new("1.00")
  #   count.integer_part    # => 1
  #   count.decimal_places  # => 2
  #   count.to_s            # => "1.00"
  #
  # A Count can be passed as the :count option of I18n.translate. It travels
  # through the whole translate pipeline (including the Pluralization and
  # Fallbacks backend modules) unchanged and is handed to the pluralization
  # rule as-is - never as a Float. It also interpolates like any other
  # value, rendering its original text:
  #
  #   I18n.t(:inbox, :count => I18n::Count.new("1.00")) # interpolates "1.00"
  #
  # == Read-only operands
  #
  # The CLDR plural operands (https://www.unicode.org/reports/tr35/#Operands)
  # are available under their CLDR names as well as more explicit aliases:
  #
  # * +n+ / +absolute_value+        - absolute numeric value (exact Rational)
  # * +i+ / +integer_part+          - integer digits of the absolute value
  # * +v+ / +decimal_places+        - number of visible fraction digits, including trailing zeros
  # * +f+ / +fraction_part+         - visible fraction digits as an integer, including trailing zeros
  # * +t+ / +trimmed_fraction_part+ - visible fraction digits as an integer, without trailing zeros
  #
  # Additionally +negative?+, +positive?+, +zero?+, +sign+ and +source+ (the
  # original text) are available.
  #
  # == Compatibility with numeric rules
  #
  # Pluralization rules written before Count existed expect a Numeric. Count
  # therefore includes Comparable and delegates the common arithmetic
  # protocol (+, -, *, /, %, modulo, divmod, coercion, ...) to its numeric
  # value, so legacy rules such as
  #
  #   lambda { |n| n % 10 == 1 && n % 100 != 11 ? :one : :other }
  #
  # keep working unchanged. Rules (or any other code) that explicitly need a
  # plain Numeric can use #to_numeric - the compatibility entry point - which
  # returns an Integer when there are no visible fraction digits and an
  # exact Rational otherwise, so no precision is ever lost through a Float.
  class Count
    include Comparable

    FORMAT = /\A(?<sign>[+-]?)(?<integer>\d+)(?:\.(?<fraction>\d+))?\z/.freeze

    # The original decimal representation, e.g. "1.00".
    attr_reader :source
    alias to_s source

    class << self
      # Coerces +value+ to a Count. Returns Count instances unchanged.
      def [](value)
        value.is_a?(self) ? value : new(value)
      end
    end

    def initialize(value)
      source =
        case value
        when Count   then value.source
        when String  then +value
        when Integer then value.to_s
        when Float   then value.to_s
        else
          if defined?(BigDecimal) && value.is_a?(BigDecimal)
            value.to_s('F')
          else
            raise ArgumentError, "cannot build an I18n::Count from #{value.inspect}"
          end
        end

      match = FORMAT.match(source) or
        raise ArgumentError, "invalid decimal representation for I18n::Count: #{source.inspect}"

      @source          = source.freeze
      @integer_digits  = match[:integer]
      @fraction_digits = match[:fraction] || ''
      @value           = Rational(@source)
      freeze
    end

    # CLDR operand "n": the absolute value of the count, as an exact Rational.
    def n
      @value.abs
    end
    alias absolute_value n

    # CLDR operand "i": integer digits of the absolute value.
    def i
      @integer_digits.to_i
    end
    alias integer_part i

    # CLDR operand "v": number of visible fraction digits, including
    # trailing zeros. This is what distinguishes "1", "1.0" and "1.00".
    def v
      @fraction_digits.length
    end
    alias decimal_places v

    # CLDR operand "f": visible fraction digits, including trailing zeros,
    # as an integer.
    def f
      @fraction_digits.to_i
    end
    alias fraction_part f

    # CLDR operand "t": visible fraction digits, without trailing zeros,
    # as an integer.
    def t
      @fraction_digits.sub(/0+\z/, '').to_i
    end
    alias trimmed_fraction_part t

    def negative?
      @value < 0
    end

    def positive?
      @value > 0
    end

    def zero?
      @value.zero?
    end

    # Returns -1, 0 or 1 depending on the sign of the numeric value.
    def sign
      @value <=> 0
    end

    # The exact numeric value of the count, as a Rational.
    def to_r
      @value
    end

    # Compatibility entry point for code (e.g. legacy pluralization rules)
    # that expects a plain Numeric. Returns an Integer when the count has no
    # visible fraction digits and an exact Rational otherwise - never a
    # Float, so no precision is lost on the way.
    def to_numeric
      v.zero? ? @value.to_i : @value
    end

    def to_i
      @value.to_i
    end

    def to_f
      @value.to_f
    end

    def <=>(other)
      case other
      when Count   then @value <=> other.to_r
      when Numeric then @value <=> other
      end
    end

    def eql?(other)
      other.is_a?(Count) && source == other.source
    end

    def hash
      source.hash
    end

    def coerce(other)
      case other
      when Integer then [other, to_numeric]
      when Float   then [other, to_f]
      when Numeric then [other, @value]
      else
        raise TypeError, "#{other.class} can't be coerced into I18n::Count"
      end
    end

    # Arithmetic delegates, so that legacy pluralization rules written for
    # plain numerics keep working unchanged.
    %i[% modulo divmod ** ceil floor round truncate].each do |operator|
      define_method(operator) do |*args|
        to_numeric.public_send(operator, *args)
      end
    end

    %i[+ - * /].each do |operator|
      define_method(operator) do |other|
        to_numeric.public_send(operator, other.is_a?(Count) ? other.to_numeric : other)
      end
    end

    # Unary operators preserve the decimal representation.
    def abs
      self.class.new(@source.sub(/\A[+-]/, ''))
    end

    def -@
      self.class.new(@source.start_with?('-') ? @source[1..] : "-#{@source.sub(/\A\+/, '')}")
    end

    def +@
      self
    end

    def inspect
      "#<#{self.class.name} #{@source}>"
    end
  end
end
