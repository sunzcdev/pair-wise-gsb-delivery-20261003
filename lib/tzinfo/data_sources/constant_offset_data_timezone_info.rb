# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  module DataSources
    # Represents a data time zone defined by a constantly observed offset.
    class ConstantOffsetDataTimezoneInfo < DataTimezoneInfo
      # @return [TimezoneOffset] the offset that is constantly observed.
      attr_reader :constant_offset

      # Initializes a new {ConstantOffsetDataTimezoneInfo}.
      #
      # The passed in `identifier` instance will be frozen. A reference to the
      # passed in {TimezoneOffset} will be retained.
      #
      # @param identifier [String] the identifier of the time zone.
      # @param constant_offset [TimezoneOffset] the constantly observed offset.
      # @raise [ArgumentError] if `identifier` or `constant_offset` is `nil`.
      def initialize(identifier, constant_offset)
        super(identifier)
        raise ArgumentError, 'constant_offset must be specified' unless constant_offset
        @constant_offset = constant_offset
      end

      # @param timestamp [Timestamp] ignored.
      # @return [TimezonePeriod] an unbounded {TimezonePeriod} for the time
      #   zone's constantly observed offset.
      def period_for(timestamp)
        constant_period
      end

      # @param local_timestamp [Timestamp] ignored.
      # @return [Array<TimezonePeriod>] an `Array` containing a single unbounded
      #   {TimezonePeriod} for the time zone's constantly observed offset.
      def periods_for_local(local_timestamp)
        [constant_period]
      end

      # (see DataTimezoneInfo#local_to_utc_intervals)
      def local_to_utc_intervals(local_start_timestamp, local_end_timestamp)
        raise ArgumentError, 'local_start_timestamp must be specified' unless local_start_timestamp
        raise ArgumentError, 'local_start_timestamp must have an unspecified utc_offset' if local_start_timestamp.utc_offset
        raise ArgumentError, 'local_end_timestamp must be specified' unless local_end_timestamp
        raise ArgumentError, 'local_end_timestamp must have an unspecified utc_offset' if local_end_timestamp.utc_offset
        raise ArgumentError, 'local_end_timestamp must be greater than or equal to local_start_timestamp' if local_end_timestamp < local_start_timestamp

        offset = @constant_offset.observed_utc_offset
        [utc_timestamp_range(local_start_timestamp, local_end_timestamp, offset)]
      end

      # @param to_timestamp [Timestamp] ignored.
      # @param from_timestamp [Timestamp] ignored.
      # @return [Array] an empty `Array`, since there are no transitions in time
      #   zones that observe a constant offset.
      def transitions_up_to(to_timestamp, from_timestamp = nil)
        []
      end

      private

      # @return [TimezonePeriod] an unbounded {TimezonePeriod} with the constant
      #   offset of this timezone.
      def constant_period
        OffsetTimezonePeriod.new(@constant_offset)
      end

      # @param local_start_timestamp [Timestamp] the inclusive local start.
      # @param local_end_timestamp [Timestamp] the exclusive local end.
      # @param offset [Integer] the observed UTC offset in seconds.
      # @return [Range<Timestamp>] the equivalent half-open UTC interval.
      def utc_timestamp_range(local_start_timestamp, local_end_timestamp, offset)
        Range.new(
          utc_timestamp(local_start_timestamp, offset),
          utc_timestamp(local_end_timestamp, offset),
          true)
      end

      # @param local_timestamp [Timestamp] a local timestamp.
      # @param offset [Integer] the observed UTC offset in seconds.
      # @return [Timestamp] the equivalent UTC timestamp.
      def utc_timestamp(local_timestamp, offset)
        Timestamp.utc(local_timestamp.value - offset, local_timestamp.sub_second)
      end
    end
  end
end
