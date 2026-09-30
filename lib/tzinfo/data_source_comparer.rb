# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  # Compares the observance of time zones between two {DataSource} instances.
  # This can be used, for example, to determine on which dates the historical
  # local times reported for a time zone will change when upgrading to a new
  # version of the IANA Time Zone Database.
  #
  # The two {DataSource} instances to compare are specified explicitly when
  # creating a {DataSourceComparer}. The global default {DataSource} (as set
  # by {DataSource.set}) is neither used nor modified, so comparisons will not
  # affect other code running in the same process.
  #
  #     old_data_source = TZInfo::DataSources::ZoneinfoDataSource.new('/usr/share/zoneinfo.old')
  #     new_data_source = TZInfo::DataSources::ZoneinfoDataSource.new('/usr/share/zoneinfo')
  #
  #     comparer = TZInfo::DataSourceComparer.new(old_data_source, new_data_source)
  #     comparison = comparer.compare('America/New_York', Time.utc(2025, 1, 1), Time.utc(2027, 1, 1))
  #
  #     comparison.differences.each do |difference|
  #       puts "#{difference.starts_at} to #{difference.ends_at}: " \
  #         "#{difference.first.abbreviation} (#{difference.first.observed_utc_offset}) becomes " \
  #         "#{difference.second.abbreviation} (#{difference.second.observed_utc_offset})"
  #     end
  #
  # {DataSourceComparer} instances are thread-safe provided the compared
  # {DataSource} instances are thread-safe (the standard
  # {DataSources::RubyDataSource} and {DataSources::ZoneinfoDataSource}
  # implementations are).
  class DataSourceComparer
    # @return [DataSource] the first of the two compared {DataSource}
    #   instances.
    attr_reader :first_data_source

    # @return [DataSource] the second of the two compared {DataSource}
    #   instances.
    attr_reader :second_data_source

    # Initializes a new {DataSourceComparer}.
    #
    # @param first_data_source [DataSource] the first {DataSource} to compare.
    # @param second_data_source [DataSource] the second {DataSource} to
    #   compare.
    # @raise [ArgumentError] if either `first_data_source` or
    #   `second_data_source` is not a {DataSource}.
    def initialize(first_data_source, second_data_source)
      raise ArgumentError, 'first_data_source must be a DataSource' unless first_data_source.kind_of?(DataSource)
      raise ArgumentError, 'second_data_source must be a DataSource' unless second_data_source.kind_of?(DataSource)

      @first_data_source = first_data_source
      @second_data_source = second_data_source
    end

    # Compares the observance of a time zone between the two {DataSource}
    # instances over a UTC window, returning the intervals over which the
    # observed offset, daylight savings status or abbreviation differ.
    #
    # Adjacent intervals that have identical differences are merged. If the
    # observance is identical in both {DataSource} instances over the whole
    # window, the returned {TimezoneComparison} will contain no differences
    # ({TimezoneComparison#empty?} will return `true`).
    #
    # If the time zone is a link to another zone, the link is resolved within
    # each {DataSource} and the resulting {TimezoneComparison} indicates
    # whether the link target differs ({TimezoneComparison#link_target_changed?}).
    #
    # The global default {DataSource} is not used or modified.
    #
    # @param identifier [String] the identifier of the time zone to compare.
    # @param from [Object] a `Time`, `DateTime` or {Timestamp} with a
    #   specified UTC offset giving the start of the window to compare.
    # @param to [Object] a `Time`, `DateTime` or {Timestamp} with a specified
    #   UTC offset giving the end of the window to compare (exclusive).
    # @return [TimezoneComparison] the result of the comparison.
    # @raise [ArgumentError] if `identifier`, `from` or `to` is `nil`, if
    #   `from` or `to` does not have a specified UTC offset, or if `to` is not
    #   later than `from`.
    # @raise [InvalidTimezoneIdentifier] if the time zone is not found in one
    #   of the {DataSource} instances.
    def compare(identifier, from, to)
      raise ArgumentError, 'identifier must be specified' unless identifier
      raise ArgumentError, 'from must be specified' unless from
      raise ArgumentError, 'to must be specified' unless to

      from_timestamp = Timestamp.for(from)
      to_timestamp = Timestamp.for(to)
      raise ArgumentError, 'from must have a specified utc_offset' unless from_timestamp.utc_offset
      raise ArgumentError, 'to must have a specified utc_offset' unless to_timestamp.utc_offset
      raise ArgumentError, 'to must be greater than from' unless to_timestamp > from_timestamp

      from_timestamp = from_timestamp.utc
      to_timestamp = to_timestamp.utc

      first_info, first_link_target = resolve_timezone_info(@first_data_source, identifier)
      second_info, second_link_target = resolve_timezone_info(@second_data_source, identifier)

      differences = []

      each_segment(first_info, second_info, from_timestamp, to_timestamp) do |segment_start, segment_end, probe|
        first_observance = TimezoneObservance.for_period(first_info.period_for(probe))
        second_observance = TimezoneObservance.for_period(second_info.period_for(probe))

        next if first_observance == second_observance

        last = differences.last

        if last && last.first == first_observance && last.second == second_observance && last.ends_at == segment_start
          last.extend_to(segment_end)
        else
          differences << TimezoneDifference.new(segment_start, segment_end, first_observance, second_observance)
        end
      end

      TimezoneComparison.new(identifier, from_timestamp, to_timestamp, differences, first_link_target, second_link_target)
    end

    private

    # Resolves a time zone identifier to a {DataSources::DataTimezoneInfo}
    # using the given {DataSource}, following links within that same
    # {DataSource} (rather than using {Timezone.get}, which would use the
    # global default {DataSource}).
    #
    # @param data_source [DataSource] the {DataSource} to resolve the
    #   identifier with.
    # @param identifier [String] the time zone identifier to resolve.
    # @return [Array] a two element `Array` containing the resolved
    #   {DataSources::DataTimezoneInfo} and, if the zone is a link, the
    #   identifier of the final link target (`nil` if the zone is not a
    #   link).
    def resolve_timezone_info(data_source, identifier)
      info = data_source.get_timezone_info(identifier)
      link_target = nil

      while info.kind_of?(DataSources::LinkedTimezoneInfo)
        link_target = info.link_to_identifier
        info = data_source.get_timezone_info(link_target)
      end

      [info, link_target]
    end

    # Splits the window between `from_timestamp` and `to_timestamp` into
    # segments at each transition of either time zone and yields each segment
    # together with a {Timestamp} that can be used to probe the observance
    # within the segment.
    #
    # @param first_info [DataSources::DataTimezoneInfo] the time zone data
    #   from the first {DataSource}.
    # @param second_info [DataSources::DataTimezoneInfo] the time zone data
    #   from the second {DataSource}.
    # @param from_timestamp [Timestamp] the UTC start of the window.
    # @param to_timestamp [Timestamp] the UTC end of the window (exclusive).
    # @yield [segment_start, segment_end, probe] each segment of the window in
    #   ascending order.
    # @yieldparam segment_start [Timestamp] the UTC start of the segment.
    # @yieldparam segment_end [Timestamp] the UTC end of the segment
    #   (exclusive).
    # @yieldparam probe [Timestamp] a {Timestamp} contained within the
    #   segment.
    def each_segment(first_info, second_info, from_timestamp, to_timestamp)
      from_value = from_timestamp.value
      to_value = to_timestamp.value

      points = [from_value, to_value]

      [first_info, second_info].each do |info|
        info.transitions_up_to(to_timestamp, from_timestamp).each do |transition|
          value = transition.timestamp_value
          points << value if value > from_value && value < to_value
        end
      end

      points.uniq!
      points.sort!

      if points.length < 2
        # The window starts and ends within the same second. Since transitions
        # occur on whole seconds, the observance is constant over the window.
        yield from_timestamp, to_timestamp, from_timestamp
      else
        points.each_cons(2) do |low, high|
          segment_start = low == from_value ? from_timestamp : Timestamp.utc(low)
          segment_end = high == to_value ? to_timestamp : Timestamp.utc(high)

          # Any whole second within the segment observes the same offset
          # (transitions only occur on whole seconds and there are none within
          # the segment).
          probe = Timestamp.utc((low + high) / 2)

          yield segment_start, segment_end, probe
        end
      end
    end
  end
end
