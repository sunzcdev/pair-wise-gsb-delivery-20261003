# frozen-string-literal: true

require 'forwardable'

module Tomlrb
  # Offset Date-Time returned in exact fractional seconds mode.
  #
  # Unlike a plain +Time+, +#to_s+ always renders the fractional seconds and
  # the declared offset, and the fractional part is stored exactly (it is
  # never rounded through a binary float or carried into the next second).
  class OffsetDateTime
    extend Forwardable

    def_delegators :@time, :year, :month, :day, :hour, :min, :sec, :usec, :nsec, :subsec, :utc_offset

    def initialize(year, month, day, hour, min, sec, offset) # rubocop:disable Metrics/ParameterLists
      @sec_int, @fraction, @fraction_digits = FractionalSeconds.parse(sec)
      @offset = offset

      validation = Time.utc(year, month, day, hour, min, @sec_int)
      raise ArgumentError, "Invalid Offset Date-Time: #{year}-#{month}-#{day}T#{hour}:#{min}:#{sec}#{offset}" unless min.to_i == validation.min && hour.to_i == validation.hour && day.to_i == validation.day && month.to_i == validation.month && year.to_i == validation.year

      @time = Time.new(year, month, day, hour, min, @sec_int + @fraction, offset)
    end

    # Exact fractional part of the second, as a Rational in [0, 1).
    def fraction
      @fraction
    end

    # @return [Time] a Time carrying the exact Rational fractional seconds
    def to_time
      @time
    end

    def to_s
      @time.strftime('%FT%T') <<
        FractionalSeconds.suffix(@fraction, @fraction_digits) <<
        @offset
    end

    def ==(other)
      case other
      when self.class
        to_time == other.to_time
      when Time
        to_time == other
      else
        false
      end
    end

    def inspect
      "#<#{self.class}: #{self}>"
    end
  end
end
