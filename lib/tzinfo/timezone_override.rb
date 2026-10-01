# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  # Represents a single erratum (correction) to be applied to an existing
  # {Timezone} over a bounded period of UTC time.
  #
  # A {TimezoneOverride} records the UTC start and end times of the correction
  # (the start is inclusive and the end is exclusive, matching the convention
  # used for {TimezoneTransition}) together with complete offset information
  # (a base UTC offset, a standard time offset and an abbreviation). Outside of
  # the window, the rules of the original time zone continue to apply.
  #
  # {TimezoneOverride} instances are passed to {Timezone#with_overrides} to
  # construct an {OverriddenTimezone} overlay view.
  class TimezoneOverride
    # @return [Integer] the UTC time at which the override starts (inclusive),
    #   as a number of seconds since 1970-01-01 00:00:00 UTC ignoring leap
    #   seconds.
    attr_reader :start_value

    # @return [Integer] the UTC time at which the override ends (exclusive), as
    #   a number of seconds since 1970-01-01 00:00:00 UTC ignoring leap seconds.
    attr_reader :end_value

    # @return [TimezoneOffset] the complete offset information that applies
    #   from {#start_value} (inclusive) to {#end_value} (exclusive).
    attr_reader :offset

    # Initializes a new {TimezoneOverride}.
    #
    # The start and end times may be given as `Time`, `DateTime` or
    # {Timestamp} instances. Their UTC offsets are taken into consideration
    # when converting to UTC. The times must fall on a whole second (no
    # sub-second component), because time zone transitions are defined with
    # one-second resolution.
    #
    # @param start_at [Object] the UTC start time of the override (inclusive).
    # @param end_at [Object] the UTC end time of the override (exclusive).
    # @param offset [TimezoneOffset] the complete offset that applies within
    #   the window.
    # @raise [ArgumentError] if `start_at`, `end_at` or `offset` is `nil`.
    # @raise [ArgumentError] if `start_at` or `end_at` has a sub-second
    #   component.
    # @raise [ArgumentError] if `end_at` is not later than `start_at`.
    def initialize(start_at, end_at, offset)
      raise ArgumentError, 'start_at must be specified' unless start_at
      raise ArgumentError, 'end_at must be specified' unless end_at
      raise ArgumentError, 'offset must be specified' unless offset
      raise ArgumentError, 'offset must be a TZInfo::TimezoneOffset' unless offset.kind_of?(TimezoneOffset)

      @start_value = to_transition_value(start_at)
      @end_value = to_transition_value(end_at)

      raise ArgumentError, 'end_at must be later than start_at' if @end_value <= @start_value

      @offset = offset
    end

    # Determines if this {TimezoneOverride} is equal to another instance.
    #
    # @param other [Object] the instance to test for equality.
    # @return [Boolean] `true` if `other` is a {TimezoneOverride} with the same
    #   {#start_value}, {#end_value} and {#offset}, otherwise `false`.
    def ==(other)
      other.kind_of?(TimezoneOverride) &&
        start_value == other.start_value && end_value == other.end_value && offset == other.offset
    end
    alias eql? ==

    # @return [Integer] a hash based on {#start_value}, {#end_value} and
    #   {#offset}.
    def hash
      [@start_value, @end_value, @offset].hash
    end

    # @return [String] the internal object state as a programmer-readable
    #   `String`.
    def inspect
      "#<#{self.class}: @start_value=#{@start_value}, @end_value=#{@end_value}, @offset=#{@offset.inspect}>"
    end

    private

    # Converts a `Time`, `DateTime` or {Timestamp} to an integer number of
    # seconds since 1970-01-01 UTC, taking its UTC offset into account.
    #
    # @param time [Object] a `Time`, `DateTime` or {Timestamp}.
    # @return [Integer] the UTC value of `time`.
    # @raise [ArgumentError] if `time` has a sub-second component.
    def to_transition_value(time)
      timestamp = Timestamp.for(time)
      raise ArgumentError, 'override boundaries must be whole seconds' unless timestamp.sub_second == 0
      timestamp.value
    end
  end
end
