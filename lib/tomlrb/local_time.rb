# frozen-string-literal: true

require 'forwardable'

module Tomlrb
  class LocalTime
    extend Forwardable

    def_delegators :@time, :hour, :min, :sec

    def initialize(hour, min, sec, exact: false)
      @exact = exact
      if exact
        @sec_int, @fraction, @fraction_digits = FractionalSeconds.parse(sec)
        base_sec = @sec_int
        @sec = @sec_int + @fraction
      else
        base_sec = sec
        @sec = sec
      end

      @time = Time.utc(0, 1, 1, hour, min, base_sec)
      raise ArgumentError, "Invalid Local Time: #{hour}-#{min}-#{sec}" unless min.to_i == @time.min && hour.to_i == @time.hour
    end

    # Exact fractional part of the second, as a Rational in [0, 1).
    # Returns +0+ for values built through the legacy float interface.
    def fraction
      @fraction || 0
    end

    def exact?
      @exact
    end

    def nsec
      @exact ? FractionalSeconds.nsec(@fraction) : @time.nsec
    end

    def usec
      @exact ? FractionalSeconds.usec(@fraction) : @time.usec
    end

    # @param year [Integer]
    # @param month [Integer]
    # @param day [Integer]
    # @param offset see {LocalDateTime#to_time}
    # @return [Time] the time of the date specified by params
    def to_time(year, month, day, offset = '-00:00')
      seconds = @exact ? @sec_int + @fraction : @sec
      Time.new(year, month, day, hour, min, seconds, offset)
    end

    def to_s
      base = @time.strftime('%T')
      if @exact
        base << FractionalSeconds.suffix(@fraction, @fraction_digits)
      else
        frac = (@sec - sec)
        base << (frac.zero? ? '' : frac.to_s[1..-1])
      end
    end

    def ==(other)
      return false unless other.is_a?(self.class)

      if exact? || other.exact?
        [hour, min, sec] == [other.hour, other.min, other.sec] &&
          fraction == other.fraction
      else
        @time == other.to_time(0, 1, 1)
      end
    end

    def inspect
      "#<#{self.class}: #{self}>"
    end
  end
end
