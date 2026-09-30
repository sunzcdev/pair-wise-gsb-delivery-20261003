# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  # Represents an interval of time over which the observance of a time zone
  # (the offset, daylight savings status or abbreviation) differs between two
  # {DataSource} instances. Returned by {DataSourceComparer#compare} as part
  # of a {TimezoneComparison}.
  class TimezoneDifference
    # @return [Timestamp] the UTC start of the interval over which the
    #   observances differ.
    attr_reader :starts_at

    # @return [Timestamp] the UTC end of the interval over which the
    #   observances differ (exclusive).
    attr_reader :ends_at

    # @return [TimezoneObservance] the observance reported by the first
    #   {DataSource} over the interval.
    attr_reader :first

    # @return [TimezoneObservance] the observance reported by the second
    #   {DataSource} over the interval.
    attr_reader :second

    # Initializes a new {TimezoneDifference}.
    #
    # @param starts_at [Timestamp] the UTC start of the interval.
    # @param ends_at [Timestamp] the UTC end of the interval (exclusive).
    # @param first [TimezoneObservance] the observance reported by the first
    #   {DataSource} over the interval.
    # @param second [TimezoneObservance] the observance reported by the second
    #   {DataSource} over the interval.
    def initialize(starts_at, ends_at, first, second)
      @starts_at = starts_at
      @ends_at = ends_at
      @first = first
      @second = second
    end

    # Determines if this {TimezoneDifference} is equal to another instance.
    #
    # @param tdi [Object] the instance to test for equality.
    # @return [Boolean] `true` if `tdi` is a {TimezoneDifference} with the
    #   same {starts_at}, {ends_at}, {first} and {second} as this
    #   {TimezoneDifference}, otherwise `false`.
    def ==(tdi)
      tdi.kind_of?(TimezoneDifference) &&
        starts_at == tdi.starts_at && ends_at == tdi.ends_at &&
        first == tdi.first && second == tdi.second
    end
    alias eql? ==

    # @return [Integer] a hash based on {starts_at}, {ends_at}, {first} and
    #   {second}.
    def hash
      [@starts_at, @ends_at, @first, @second].hash
    end

    # @return [String] the internal object state as a programmer-readable
    #   `String`.
    def inspect
      "#<#{self.class}: @starts_at=#{@starts_at.inspect}, @ends_at=#{@ends_at.inspect}, @first=#{@first.inspect}, @second=#{@second.inspect}>"
    end

    # Extends the end of the interval. Used by {DataSourceComparer} to merge
    # adjacent intervals that have identical differences.
    #
    # @param ends_at [Timestamp] the new UTC end of the interval (exclusive).
    # @private
    def extend_to(ends_at)
      @ends_at = ends_at
    end
  end
end
