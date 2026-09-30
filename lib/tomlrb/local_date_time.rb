# frozen-string-literal: true

require 'forwardable'

module Tomlrb
  class LocalDateTime
    extend Forwardable

    def_delegators :@time, :year, :month, :day, :hour, :min, :sec

    def initialize(year, month, day, hour, min, sec, exact: false) # rubocop:disable Metrics/ParameterLists
      @exact = exact
      if exact
        @sec_int, @fraction, @fraction_digits = FractionalSeconds.parse(sec)
        base_sec = @sec_int
        @sec = @sec_int + @fraction
      else
        base_sec = sec
        @sec = sec
      end

      @time = Time.utc(year, month, day, hour, min, base_sec)
      raise ArgumentError, "Invalid Local Date-Time: #{year}-#{month}-#{day}T#{hour}:#{min}:#{sec}" unless min.to_i == @time.min && hour.to_i == @time.hour && day.to_i == @time.day && month.to_i == @time.month && year.to_i == @time.year
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

    # @param offset [String, Symbol, Numeric, nil] time zone offset.
    #   * when +String+, must be '+HH:MM' format, '-HH:MM' format, 'UTC', 'A'..'I' or 'K'..'Z'. Arguments excluding '+-HH:MM' are supporeted at Ruby >= 2.7.0
    #   * when +Symbol+, must be +:dst+(for summar time for local) or +:std+(for standard time).
    #   * when +Numeric+, it is time zone offset in second.
    #   * when +nil+, local time zone offset is used.
    # @return [Time]
    def to_time(offset = '-00:00')
      if @exact
        zone = offset == '-00:00' ? '+00:00' : offset
        Time.new(year, month, day, hour, min, @sec_int + @fraction, zone)
      else
        return @time if offset == '-00:00'
        Time.new(year, month, day, hour, min, @sec, offset)
      end
    end

    def to_s
      base = @time.strftime('%FT%T')
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
        [year, month, day, hour, min, sec] ==
          [other.year, other.month, other.day, other.hour, other.min, other.sec] &&
          fraction == other.fraction
      else
        to_time == other.to_time
      end
    end

    def inspect
      "#<#{self.class}: #{self}>"
    end
  end
end
