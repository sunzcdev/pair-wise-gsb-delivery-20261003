# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  # The result of comparing the observance of a time zone between two
  # {DataSource} instances over a UTC window. Returned by
  # {DataSourceComparer#compare}.
  class TimezoneComparison
    include Enumerable

    # @return [String] the identifier of the time zone that was compared.
    attr_reader :identifier

    # @return [Timestamp] the UTC start of the compared window.
    attr_reader :from

    # @return [Timestamp] the UTC end of the compared window (exclusive).
    attr_reader :to

    # @return [Array<TimezoneDifference>] a frozen `Array` of the intervals
    #   over which the observance of the time zone differs between the two
    #   {DataSource} instances, ordered by increasing start time. Empty if
    #   there are no differences.
    attr_reader :differences

    # @return [String] the identifier of the time zone the compared zone
    #   links to according to the first {DataSource}, or `nil` if the zone is
    #   not a link in the first {DataSource}.
    attr_reader :first_link_target

    # @return [String] the identifier of the time zone the compared zone
    #   links to according to the second {DataSource}, or `nil` if the zone
    #   is not a link in the second {DataSource}.
    attr_reader :second_link_target

    # Initializes a new {TimezoneComparison}.
    #
    # @param identifier [String] the identifier of the compared time zone.
    # @param from [Timestamp] the UTC start of the compared window.
    # @param to [Timestamp] the UTC end of the compared window (exclusive).
    # @param differences [Array<TimezoneDifference>] the intervals over which
    #   the observance of the time zone differs.
    # @param first_link_target [String] the link target of the zone according
    #   to the first {DataSource} (`nil` if not a link).
    # @param second_link_target [String] the link target of the zone according
    #   to the second {DataSource} (`nil` if not a link).
    def initialize(identifier, from, to, differences, first_link_target, second_link_target)
      @identifier = identifier
      @from = from
      @to = to
      @differences = differences.freeze
      @first_link_target = first_link_target
      @second_link_target = second_link_target
    end

    # Calls the given block once for each {TimezoneDifference} in the
    # comparison.
    #
    # @yield [difference] each {TimezoneDifference} in turn.
    # @yieldparam difference [TimezoneDifference] a {TimezoneDifference}.
    # @return [Array<TimezoneDifference>] the {differences} `Array`.
    def each(&block)
      @differences.each(&block)
    end

    # Determines if the observance of the time zone is identical in both
    # {DataSource} instances over the compared window.
    #
    # @return [Boolean] `true` if there are no differences, otherwise `false`.
    def empty?
      @differences.empty?
    end

    # Determines if the target of the time zone link differs between the two
    # {DataSource} instances (including the zone being a link in one
    # {DataSource} and a data zone in the other).
    #
    # @return [Boolean] `true` if the link target differs, otherwise `false`.
    def link_target_changed?
      @first_link_target != @second_link_target
    end
  end
end
