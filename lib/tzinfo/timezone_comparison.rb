# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  # Compares time zone data supplied by two explicit DataSource instances.
  module TimezoneComparison
    # A UTC interval over which two data sources describe a time zone
    # differently.
    class Difference
      # @return [Timestamp] the inclusive UTC start of the interval.
      attr_reader :from

      # @return [Timestamp] the exclusive UTC end of the interval.
      attr_reader :to

      # @return [Integer] the observed UTC offset from the first data source.
      attr_reader :first_utc_offset

      # @return [Integer] the observed UTC offset from the second data source.
      attr_reader :second_utc_offset

      # @return [Boolean] whether the first data source observes DST.
      attr_reader :first_dst

      # @return [Boolean] whether the second data source observes DST.
      attr_reader :second_dst

      # @return [String] the abbreviation from the first data source.
      attr_reader :first_abbreviation

      # @return [String] the abbreviation from the second data source.
      attr_reader :second_abbreviation

      # @return [String] the direct link target from the first data source, or
      #   nil if the time zone is not a link.
      attr_reader :first_link_target

      # @return [String] the direct link target from the second data source,
      #   or nil if the time zone is not a link.
      attr_reader :second_link_target

      # @return [String] the canonical link target from the first data
      #   source, or nil if the time zone is not a link.
      attr_reader :first_canonical_link_target

      # @return [String] the canonical link target from the second data
      #   source, or nil if the time zone is not a link.
      attr_reader :second_canonical_link_target

      # Initializes a new Difference.
      #
      # @param from [Timestamp] the inclusive UTC start.
      # @param to [Timestamp] the exclusive UTC end.
      # @param first_period [TimezonePeriod] the first data source period.
      # @param second_period [TimezonePeriod] the second data source period.
      # @param first_link_target [String] the first direct link target.
      # @param second_link_target [String] the second direct link target.
      # @param first_canonical_link_target [String] the first canonical target.
      # @param second_canonical_link_target [String] the second canonical
      #   target.
      def initialize(from, to, first_period, second_period, first_link_target,
                     second_link_target, first_canonical_link_target,
                     second_canonical_link_target)
        @from = from
        @to = to
        @first_utc_offset = first_period.observed_utc_offset
        @second_utc_offset = second_period.observed_utc_offset
        @first_dst = first_period.dst?
        @second_dst = second_period.dst?
        @first_abbreviation = first_period.abbreviation
        @second_abbreviation = second_period.abbreviation
        @first_link_target = first_link_target
        @second_link_target = second_link_target
        @first_canonical_link_target = first_canonical_link_target
        @second_canonical_link_target = second_canonical_link_target
      end

      # @return [Boolean] whether either the direct or canonical link target
      #   differs between the data sources.
      def link_target_changed?
        first_link_target != second_link_target ||
          first_canonical_link_target != second_canonical_link_target
      end

      # @param other [Difference] an adjacent difference.
      # @return [Boolean] whether the difference content is identical.
      def same_content?(other)
        other.kind_of?(Difference) &&
          first_utc_offset == other.first_utc_offset &&
          second_utc_offset == other.second_utc_offset &&
          first_dst == other.first_dst &&
          second_dst == other.second_dst &&
          first_abbreviation == other.first_abbreviation &&
          second_abbreviation == other.second_abbreviation &&
          first_link_target == other.first_link_target &&
          second_link_target == other.second_link_target &&
          first_canonical_link_target == other.first_canonical_link_target &&
          second_canonical_link_target == other.second_canonical_link_target
      end

      # Extends this interval to an adjacent UTC time.
      #
      # @param new_to [Timestamp] the new exclusive UTC end.
      # @return [Difference] self.
      def extend_to(new_to)
        @to = new_to
        self
      end

      # @param other [Object] an Object to compare with.
      # @return [Boolean] true if other has the same interval and content.
      def ==(other)
        same_content?(other) && from == other.from && to == other.to
      end
      alias eql? ==

      # @return [Integer] a hash based on the interval and content.
      def hash
        [from, to, first_utc_offset, second_utc_offset, first_dst, second_dst,
         first_abbreviation, second_abbreviation, first_link_target,
         second_link_target, first_canonical_link_target,
         second_canonical_link_target].hash
      end

      # @return [String] the internal object state as a programmer-readable
      #   String.
      def inspect
        "#<#{self.class}: #{from.utc}...#{to.utc}>"
    end
  end

  class << self
    # Compares a time zone from two explicit DataSource instances over a UTC
    # window. The process-wide default data source is neither changed nor used
    # to resolve the supplied time zone.
    #
    # Adjacent intervals with the same pair of observances and link metadata
    # are merged. An empty Array is returned when the observed UTC offset, DST
    # flag and abbreviation match throughout and neither link target differs.
    #
    # @param identifier [String] an IANA Time Zone Database time zone
    #   identifier.
    # @param first_data_source [DataSource] the first data source.
    # @param second_data_source [DataSource] the second data source.
    # @param from [Object] the inclusive UTC start as a Time, DateTime or
    #   Timestamp.
    # @param to [Object] the exclusive UTC end as a Time, DateTime or
    #   Timestamp.
    # @return [Array<Difference>] the differing UTC intervals.
    # @raise [ArgumentError] if either data source is not a DataSource, or to
    #   is not later than from.
    def compare(identifier, first_data_source, second_data_source, from, to)
      unless first_data_source.kind_of?(DataSource) &&
             second_data_source.kind_of?(DataSource)
        raise ArgumentError, 'first_data_source and second_data_source must be DataSource instances'
      end

      from_timestamp = utc_timestamp(from)
      to_timestamp = utc_timestamp(to)
      raise ArgumentError, 'to must be later than from' unless to_timestamp > from_timestamp

      first_info, first_timezone = timezone(first_data_source, identifier)
      second_info, second_timezone = timezone(second_data_source, identifier)
      first_link_target = link_target(first_info)
      second_link_target = link_target(second_info)
      first_canonical_link_target =
        canonical_link_target(first_info, first_timezone)
      second_canonical_link_target =
        canonical_link_target(second_info, second_timezone)
      link_target_changed =
        first_link_target != second_link_target ||
        first_canonical_link_target != second_canonical_link_target

      boundaries = [from_timestamp]
      boundaries.concat(transition_times(first_timezone, from_timestamp,
                                         to_timestamp))
      boundaries.concat(transition_times(second_timezone, from_timestamp,
                                         to_timestamp))
      boundaries.sort!
      boundaries.uniq!

      result = []
      first_period = first_timezone.period_for_utc(from_timestamp)
      second_period = second_timezone.period_for_utc(from_timestamp)
      start = from_timestamp

      boundaries[1..-1].each do |boundary|
        append_difference(result, start, boundary, first_period, second_period,
                          first_link_target, second_link_target,
                          first_canonical_link_target, second_canonical_link_target,
                          link_target_changed)

        start = boundary
        first_period = first_timezone.period_for_utc(boundary)
        second_period = second_timezone.period_for_utc(boundary)
      end

      append_difference(result, start, to_timestamp, first_period, second_period,
                        first_link_target, second_link_target,
                        first_canonical_link_target, second_canonical_link_target,
                        link_target_changed)
      result
    end

    private

    def utc_timestamp(time)
      timestamp = Timestamp.for(time, :treat_as_utc)
      raise ArgumentError, 'time must have a specified utc_offset' unless timestamp.utc_offset
      timestamp
    end

    def timezone(data_source, identifier)
      info = data_source.get_timezone_info(identifier)
      [info, create_timezone(info, data_source)]
    end

    def create_timezone(info, data_source)
      if info.method(:create_timezone).arity == 0
        info.create_timezone
      else
        info.create_timezone(data_source)
      end
    end

    def link_target(info)
      info.kind_of?(DataSources::LinkedTimezoneInfo) ? info.link_to_identifier : nil
    end

    def canonical_link_target(info, timezone)
      link_target(info) ? timezone.canonical_identifier : nil
    end

    def transition_times(timezone, from, to)
      timezone.transitions_up_to(to, from).map(&:at)
    end

    def different?(first_period, second_period, link_target_changed)
      link_target_changed ||
        first_period.observed_utc_offset != second_period.observed_utc_offset ||
        first_period.dst? != second_period.dst? ||
        first_period.abbreviation != second_period.abbreviation
    end

    def append_difference(result, from, to, first_period, second_period,
                          first_link_target, second_link_target,
                          first_canonical_link_target,
                          second_canonical_link_target,
                          link_target_changed)
      return unless different?(first_period, second_period, link_target_changed)

      difference = Difference.new(
        from, to, first_period, second_period,
        first_link_target, second_link_target,
        first_canonical_link_target, second_canonical_link_target
      )

      if result.last && result.last.to == from && result.last.same_content?(difference)
        result.last.extend_to(to)
      else
        result << difference
      end
    end
  end
end
end
