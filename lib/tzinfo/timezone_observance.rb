# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  # A snapshot of the offset, daylight savings status and abbreviation a time
  # zone observes at a particular time. Used by {TimezoneDifference} to
  # describe how the observances reported by two {DataSource} instances
  # differ.
  class TimezoneObservance
    # @return [Integer] the observed offset from UTC in seconds (including any
    #   adjustment for daylight savings time).
    attr_reader :observed_utc_offset

    # @return [String] the abbreviation that identifies the observed offset,
    #   for example, GMT or BST.
    attr_reader :abbreviation

    # Initializes a new {TimezoneObservance}.
    #
    # @param observed_utc_offset [Integer] the observed offset from UTC in
    #   seconds.
    # @param dst [Boolean] whether daylight savings time is being observed.
    # @param abbreviation [String] the abbreviation identifying the observed
    #   offset.
    def initialize(observed_utc_offset, dst, abbreviation)
      @observed_utc_offset = observed_utc_offset
      @dst = !!dst
      @abbreviation = abbreviation.freeze
    end

    # Determines if daylight savings time is being observed.
    #
    # @return [Boolean] `true` if daylight savings time is being observed,
    #   otherwise `false`.
    def dst?
      @dst
    end

    # Creates a {TimezoneObservance} from a {TimezonePeriod}.
    #
    # @param period [TimezonePeriod] the {TimezonePeriod} to take a snapshot
    #   of.
    # @return [TimezoneObservance] a {TimezoneObservance} representing the
    #   offset, daylight savings status and abbreviation observed in `period`.
    def self.for_period(period)
      new(period.observed_utc_offset, period.dst?, period.abbreviation)
    end

    # Determines if this {TimezoneObservance} is equal to another instance.
    #
    # @param toi [Object] the instance to test for equality.
    # @return [Boolean] `true` if `toi` is a {TimezoneObservance} with the
    #   same {observed_utc_offset}, {dst?} and {abbreviation} as this
    #   {TimezoneObservance}, otherwise `false`.
    def ==(toi)
      toi.kind_of?(TimezoneObservance) &&
        observed_utc_offset == toi.observed_utc_offset && dst? == toi.dst? && abbreviation == toi.abbreviation
    end
    alias eql? ==

    # @return [Integer] a hash based on {observed_utc_offset}, {dst?} and
    #   {abbreviation}.
    def hash
      [@observed_utc_offset, @dst, @abbreviation].hash
    end

    # @return [String] the internal object state as a programmer-readable
    #   `String`.
    def inspect
      "#<#{self.class}: @observed_utc_offset=#{@observed_utc_offset}, @dst=#{@dst}, @abbreviation=#{@abbreviation}>"
    end
  end
end
