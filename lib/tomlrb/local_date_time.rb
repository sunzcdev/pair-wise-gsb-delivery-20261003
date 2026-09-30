# frozen-string-literal: true

require 'forwardable'
require 'tomlrb/fractional_seconds'

module Tomlrb
  class LocalDateTime
    extend Forwardable
    include FractionalSeconds

    def_delegators :@time, :year, :month, :day, :hour, :min, :sec, :usec, :nsec

    def initialize(year, month, day, hour, min, sec) # rubocop:disable Metrics/ParameterLists
      @time = Time.utc(year, month, day, hour, min, sec)
      raise ArgumentError, "Invalid Local Date-Time: #{year}-#{month}-#{day}T#{hour}:#{min}:#{sec}" unless min.to_i == @time.min && hour.to_i == @time.hour && day.to_i == @time.day && month.to_i == @time.month && year.to_i == @time.year

      @sec = sec
    end

    # @param offset [String, Symbol, Numeric, nil] time zone offset.
    #   * when +String+, must be '+HH:MM' format, '-HH:MM' format, 'UTC', 'A'..'I' or 'K'..'Z'. Arguments excluding '+-HH:MM' are supporeted at Ruby >= 2.7.0
    #   * when +Symbol+, must be +:dst+(for summar time for local) or +:std+(for standard time).
    #   * when +Numeric+, it is time zone offset in second.
    #   * when +nil+, local time zone offset is used.
    # @return [Time]
    def to_time(offset = '-00:00')
      return @time if offset == '-00:00'
      Time.new(year, month, day, hour, min, @sec, offset)
    end

    def to_s
      @time.strftime('%FT%T') << fractional_second_string(@sec - sec)
    end

    def ==(other)
      other.is_a?(self.class) &&
        to_time == other.to_time
    end

    def inspect
      "#<#{self.class}: #{self}>"
    end
  end
end
