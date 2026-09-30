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

      # Returns an `Array` containing the single unbounded {TimezonePeriod} for
      # each of the local times specified by `local_timestamps`.
      #
      # Invalid entries do not prevent the remaining entries from being
      # resolved: the corresponding result position contains the
      # {ArgumentError} that would have been raised by {periods_for_local}.
      #
      # @param local_timestamps [Enumerable] an `Enumerable` of local times,
      #   each represented as a {Timestamp} with an unspecified
      #   {Timestamp#utc_offset utc_offset}.
      # @return [Array<Array<TimezonePeriod>>] an `Array` containing a
      #   single-element `Array` of the {TimezonePeriod} for each valid local
      #   timestamp, or an {ArgumentError} for each invalid entry.
      # @raise [ArgumentError] if `local_timestamps` is `nil`.
      def periods_for_local_batch(local_timestamps)
        raise ArgumentError, 'local_timestamps must be specified' unless local_timestamps

        local_timestamps.map do |local_timestamp|
          begin
            raise ArgumentError, 'local_timestamp must be specified' unless local_timestamp
            raise ArgumentError, 'local_timestamp must have an unspecified utc_offset' if local_timestamp.utc_offset
          rescue ArgumentError => e
            next e
          end

          [constant_period]
        end
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
    end
  end
end
