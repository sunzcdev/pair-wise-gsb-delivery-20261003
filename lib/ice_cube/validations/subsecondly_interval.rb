module IceCube
  module Validations::SubsecondlyInterval
    NANOSECONDS_PER_SECOND = 1_000_000_000

    # Accept an exact sub-second (or whole-second) interval as an Integer,
    # Rational, BigDecimal or decimal String. The interval is stored as an
    # exact Rational number of seconds so that repeated advancement never
    # accumulates floating point error.
    def interval(value)
      @interval = Validations::SubsecondlyInterval.normalize_interval(value)
      replace_validations_for(:interval, [Validation.new(@interval)])
      clobber_base_validations(:sec)
      self
    end

    def self.normalize_interval(value)
      rational =
        case value
        when Rational then value
        when Integer then value.to_r
        when String then parse_string_interval(value)
        when Float
          raise ArgumentError, "Float interval #{value.inspect} cannot be " \
            "represented exactly. Please pass a Rational (e.g. Rational(1, 8)) " \
            "or a decimal String (e.g. \"0.125\") instead."
        else
          if defined?(BigDecimal) && value.is_a?(BigDecimal)
            value.to_r
          else
            raise ArgumentError, "'#{value}' is not a valid input for interval. " \
              "Please pass a positive Integer, Rational or decimal String."
          end
        end

      unless rational > 0
        raise ArgumentError, "'#{value}' is not a valid input for interval. " \
          "Please pass a positive Integer, Rational or decimal String."
      end

      unless (rational * NANOSECONDS_PER_SECOND).denominator == 1
        raise ArgumentError, "interval '#{value}' (#{rational} seconds) is " \
          "finer than nanosecond precision and cannot be represented."
      end

      rational
    end

    def self.parse_string_interval(value)
      Rational(value)
    rescue ArgumentError, TypeError
      raise ArgumentError, "'#{value}' is not a valid input for interval. " \
        "Please pass a positive Integer, Rational or decimal String."
    end

    class Validation
      attr_reader :interval

      def initialize(interval)
        @interval = interval
      end

      def type
        :sec
      end

      def dst_adjust?
        false
      end

      # Exact Rational arithmetic: both times are converted with sub-second
      # (nanosecond) precision so the offset never drifts.
      def validate(step_time, start_time)
        elapsed = step_time.to_r - start_time.to_r
        remainder = elapsed % interval
        interval - remainder if remainder.nonzero?
      end

      def build_s(builder)
        builder.base = IceCube::I18n.t("ice_cube.each_second", count: interval)
      end

      def build_hash(builder)
        builder[:interval] = interval
      end

      def build_ical(builder)
        raise ArgumentError, "Sub-second intervals cannot be represented in iCalendar (RFC 5545)"
      end
    end
  end
end
