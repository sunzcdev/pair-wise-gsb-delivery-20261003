require "bigdecimal"

module IceCube
  module Validations::SubsecondlyInterval
    NANOSECONDS_PER_SECOND = 1_000_000_000
    NANOSECOND = Rational(1, NANOSECONDS_PER_SECOND)

    # Normalize a sub-second interval to an exact Rational number of seconds.
    # Floats are rejected because they cannot represent decimal fractions
    # exactly, and intervals must be a whole number of nanoseconds so they
    # can be aligned with the nanosecond precision of Time.
    def self.normalize_interval(value)
      rational = case value
      when Integer then value.to_r
      when Rational then value
      when BigDecimal then value.to_r
      when Float
        raise ArgumentError, "#{value.inspect} is a Float and cannot represent an exact " \
                             "sub-second interval. Please pass a Rational (e.g. Rational(1, 8)) " \
                             "or a BigDecimal instead."
      when String
        begin
          Rational(value)
        rescue ArgumentError, TypeError
          raise ArgumentError, "'#{value}' is not a valid input for interval. Please pass a " \
                               "positive Integer, Rational or BigDecimal number of seconds."
        end
      else
        raise ArgumentError, "#{value.inspect} is not a valid input for interval. Please pass a " \
                             "positive Integer, Rational or BigDecimal number of seconds."
      end

      unless rational.positive?
        raise ArgumentError, "#{value.inspect} is not a valid input for interval. Please pass a " \
                             "positive number of seconds."
      end

      unless (rational * NANOSECONDS_PER_SECOND).denominator == 1
        raise ArgumentError, "#{value.inspect} cannot be represented as a whole number of " \
                             "nanoseconds. Sub-second intervals must be exact multiples of " \
                             "1 nanosecond."
      end

      rational
    end

    # Format an interval as an exact decimal string (intervals are always a
    # whole number of nanoseconds).
    def self.format_interval(interval)
      nanoseconds = (interval * NANOSECONDS_PER_SECOND).to_i
      seconds, fraction = nanoseconds.divmod(NANOSECONDS_PER_SECOND)
      return seconds.to_s if fraction.zero?
      "#{seconds}.#{fraction.to_s.rjust(9, "0").sub(/0+\z/, "")}"
    end

    def interval(interval)
      @interval = Validations::SubsecondlyInterval.normalize_interval(interval)
      replace_validations_for(:interval, [Validation.new(@interval)])
      clobber_base_validations(:sec)
      self
    end

    class Validation
      # The interval as an exact Rational number of seconds
      attr_reader :interval

      def initialize(interval)
        @interval = interval
        @interval_ns = (interval * NANOSECONDS_PER_SECOND).to_i
      end

      def type
        :sec
      end

      def dst_adjust?
        false
      end

      # Exact arithmetic: offsets are computed from the start time in whole
      # nanoseconds, so repeated advancement never accumulates float error.
      def validate(step_time, start_time)
        elapsed_ns = (step_time.to_r - start_time.to_r) * NANOSECONDS_PER_SECOND
        remainder = elapsed_ns % @interval_ns
        (@interval_ns - remainder) * NANOSECOND if remainder.nonzero?
      end

      def build_s(builder)
        builder.base = "Every #{Validations::SubsecondlyInterval.format_interval(interval)} seconds"
      end

      def build_hash(builder)
        builder[:interval] = interval
      end

      def build_ical(_builder)
        raise NotImplementedError, "Sub-secondly rules cannot be represented in iCalendar (RFC 5545)"
      end
    end
  end
end
