# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  # A {TimezoneCorrection} represents a historical errata entry that overrides
  # the offsets observed by a time zone within a specified interval of UTC
  # time. Outside the interval, the time zone continues to follow its original
  # rules.
  #
  # The interval of time over which a correction applies is half-open: the
  # start time is included and the end time is excluded (`[start_at, end_at)`).
  # Times are specified in UTC. Sub-second precision is not supported because
  # time zone transitions occur on whole second boundaries.
  #
  # The offset that applies during the interval is described by complete offset
  # information:
  #
  # * a base (standard time) offset from UTC in seconds
  #   ({TimezoneOffset#base_utc_offset base_utc_offset});
  # * a seasonal offset from standard time in seconds
  #   ({TimezoneOffset#std_offset std_offset}), typically 0 or 3600 (1 hour);
  # * an abbreviation identifying the offset
  #   ({TimezoneOffset#abbreviation abbreviation}), for example `GMT` or `BST`.
  #
  # {TimezoneCorrection} instances can be passed to {Timezone#with_corrections}
  # (or added with {CorrectionBuilder#add}) in order to construct an
  # overriding view of an existing {Timezone}.
  class TimezoneCorrection
    # @return [Timestamp] the UTC time at which this correction starts to
    #   apply (inclusive).
    attr_reader :start_at

    # @return [Timestamp] the UTC time at which this correction stops applying
    #   (exclusive).
    attr_reader :end_at

    # @return [TimezoneOffset] the complete offset information that applies
    #   while this correction is in effect.
    attr_reader :offset

    # Initializes a new {TimezoneCorrection}.
    #
    # @param start_at [Object] the UTC time at which the correction starts to
    #   apply (inclusive), as a `Time`, `DateTime` or {Timestamp}.
    # @param end_at [Object] the UTC time at which the correction stops
    #   applying (exclusive), as a `Time`, `DateTime` or {Timestamp}.
    # @param offset [TimezoneOffset] the complete offset information that
    #   applies while the correction is in effect.
    # @param base_utc_offset [Object] the base (standard time) offset from UTC
    #   in seconds. Used when `offset` is not specified.
    # @param std_offset [Object] the seasonal offset from standard time in
    #   seconds (typically 0 or 3600). Used when `offset` is not specified.
    # @param abbreviation [String] the abbreviation identifying the offset
    #   (for example `GMT` or `BST`). Used when `offset` is not specified.
    # @raise [ArgumentError] if `start_at` or `end_at` is `nil`.
    # @raise [ArgumentError] if `end_at` is not later than `start_at`.
    # @raise [ArgumentError] if either boundary has a sub-second component.
    # @raise [ArgumentError] if neither `offset` nor complete offset
    #   information (`base_utc_offset`, `std_offset` and `abbreviation`) is
    #   specified.
    def initialize(start_at, end_at, offset = nil, base_utc_offset = nil, std_offset = nil, abbreviation = nil)
      raise ArgumentError, 'start_at must be specified' unless start_at
      raise ArgumentError, 'end_at must be specified' unless end_at

      @start_at = to_utc_timestamp(start_at)
      @end_at = to_utc_timestamp(end_at)

      if @end_at.value <= @start_at.value
        raise ArgumentError, 'end_at must be later than start_at'
      end

      if offset
        raise ArgumentError, 'offset must be a TZInfo::TimezoneOffset' unless offset.kind_of?(TimezoneOffset)
        @offset = offset
      else
        if base_utc_offset.nil? || std_offset.nil? || abbreviation.nil?
          raise ArgumentError, 'either offset or base_utc_offset, std_offset and abbreviation must be specified'
        end
        @offset = TimezoneOffset.new(base_utc_offset, std_offset, abbreviation)
      end

      freeze
    end

    # Determines if this {TimezoneCorrection} is equal to another instance.
    #
    # @param c [Object] the instance to test for equality.
    # @return [Boolean] `true` if `c` is a {TimezoneCorrection} with the same
    #   {start_at}, {end_at} and {offset} as this instance, otherwise
    #   `false`.
    def ==(c)
      c.kind_of?(TimezoneCorrection) &&
        start_at.value == c.start_at.value &&
        end_at.value == c.end_at.value &&
        offset == c.offset
    end
    alias eql? ==

    # @return [Integer] a hash based on {start_at}, {end_at} and {offset}.
    def hash
      [start_at.value, end_at.value, offset].hash
    end

    # @return [String] the internal object state as a programmer-readable
    #   `String`.
    def inspect
      "#<#{self.class}: @start_at=#{start_at.value}, @end_at=#{end_at.value}, @offset=#{offset.inspect}>"
    end

    private

    # Converts a `Time`, `DateTime` or {Timestamp} to a UTC {Timestamp} with a
    # whole number of seconds, validating the result.
    #
    # @param time [Object] a `Time`, `DateTime` or {Timestamp}.
    # @return [Timestamp] the UTC {Timestamp} representing `time`.
    # @raise [ArgumentError] if `time` has a sub-second component.
    def to_utc_timestamp(time)
      timestamp = Timestamp.for(time)
      raise ArgumentError, 'correction boundaries must be whole seconds' unless timestamp.sub_second == 0
      timestamp.utc
    end
  end
end
