# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  # A {CorrectedTimezone} is an overriding view of an existing {Timezone}
  # (the "base" time zone) that replaces the offsets observed within specified
  # intervals of UTC time with historical errata entries
  # ({TimezoneCorrection}). Outside of those intervals the base time zone's
  # original rules continue to apply unchanged, including after a correction
  # ends (the offset that genuinely took effect at that instant in the base
  # time zone is rejoined, even when the correction spans multiple original
  # transitions).
  #
  # Instances should not normally be created directly. Use the
  # {Timezone#with_corrections} method instead.
  #
  # The resulting view behaves as any other {Timezone}:
  #
  # * {#period_for} returns the effective period at a UTC instant;
  # * {#periods_for_local} returns the effective periods when resolving a
  #   local time, including the gaps and repeated hours introduced by the
  #   corrections;
  # * {#transitions_up_to} returns the effective sequence of transitions,
  #   including synthetic transitions at correction boundaries.
  #
  # The results of these methods are mutually consistent.
  class CorrectedTimezone < Timezone
    # @return [Timezone] the time zone this view is based on. Outside of the
    #   corrected intervals, its original rules apply.
    attr_reader :base_timezone

    # Initializes a new {CorrectedTimezone}.
    #
    # {CorrectedTimezone} instances should not normally be created directly.
    # Use the {Timezone#with_corrections} method instead.
    #
    # @param base_timezone [Timezone] the {Timezone} whose rules are to be
    #   overridden within the corrected intervals.
    # @param corrections [Enumerable<TimezoneCorrection>] the errata entries to
    #   apply. The entries must not overlap each other (touching
    #   end-to-start boundaries are allowed).
    # @param identifier [String] an optional identifier for the new view. If
    #   not specified, an identifier derived from the base time zone is used.
    # @raise [ArgumentError] if `base_timezone` is `nil`.
    # @raise [ArgumentError] if `corrections` is `nil` or empty.
    # @raise [ArgumentError] if any member of `corrections` is not a
    #   {TimezoneCorrection}.
    # @raise [ArgumentError] if two corrections overlap.
    def initialize(base_timezone, corrections, identifier = nil)
      raise ArgumentError, 'base_timezone must be specified' unless base_timezone
      raise ArgumentError, 'corrections must be specified' unless corrections

      super()

      @base_timezone = base_timezone
      @corrections = normalize_corrections(corrections)
      @identifier = (identifier || "#{base_timezone.identifier} (corrected)").freeze
      @timezone = build_timezone
    end

    # @return [Array<TimezoneCorrection>] the {TimezoneCorrection} entries
    #   applied by this view, sorted by ascending start time.
    def corrections
      @corrections.dup
    end

    # (see Timezone#identifier)
    def identifier
      @identifier
    end

    # (see Timezone#period_for)
    def period_for(time)
      @timezone.period_for(time)
    end

    # (see Timezone#periods_for_local)
    def periods_for_local(local_time)
      @timezone.periods_for_local(local_time)
    end

    # (see Timezone#transitions_up_to)
    def transitions_up_to(to, from = nil)
      @timezone.transitions_up_to(to, from)
    end

    # Returns the canonical {Timezone} of the base time zone. The
    # {CorrectedTimezone} itself is a view rather than an independently
    # defined time zone.
    #
    # @return [Timezone] the canonical {Timezone} of {#base_timezone}.
    def canonical_zone
      @base_timezone.canonical_zone
    end

    private

    # Validates, sorts and freezes the supplied {TimezoneCorrection} entries.
    #
    # @param corrections [Enumerable<TimezoneCorrection>] the errata entries to
    #   apply.
    # @return [Array<TimezoneCorrection>] the entries sorted by ascending start
    #   time.
    # @raise [ArgumentError] if `corrections` is empty or contains non-entries
    #   or overlapping entries.
    def normalize_corrections(corrections)
      list = corrections.to_a.dup
      raise ArgumentError, 'corrections must not be an empty Array' if list.empty?

      list.each do |correction|
        unless correction.kind_of?(TimezoneCorrection)
          raise ArgumentError, 'each correction must be a TZInfo::TimezoneCorrection'
        end
      end

      list.sort! {|a, b| a.start_at.value <=> b.start_at.value }

      (1...list.length).each do |i|
        if list[i].start_at.value < list[i - 1].end_at.value
          raise ArgumentError, "corrections must not overlap: #{list[i].inspect} overlaps #{list[i - 1].inspect}"
        end
      end

      list.freeze
    end

    # Builds the {DataTimezone} that implements the effective sequence of
    # periods for the view.
    #
    # @return [DataTimezone] a {DataTimezone} that returns the effective
    #   periods, local period resolutions and transitions of the view.
    def build_timezone
      info = DataSources::TransitionsDataTimezoneInfo.new(@identifier, merged_transitions)
      info.create_timezone
    end

    # Constructs the effective sequence of transitions for the view by merging
    # the base time zone's transitions with synthetic transitions at each
    # correction boundary.
    #
    # Each UTC timestamp on the resulting timeline is evaluated once. At a
    # correction start the correction's offset applies; at a correction end
    # the base time zone's offset (at that same instant) rejoins. Original
    # transitions that fall strictly within a corrected interval are removed.
    #
    # @return [Array<TimezoneTransition>] the effective transitions of the
    #   view in ascending timestamp order.
    def merged_transitions
      events = {}

      relevant_base_transitions.each do |transition|
        events[transition.timestamp_value] = {base: transition}
      end

      @corrections.each do |correction|
        (events[correction.start_at.value] ||= {})[:start] = correction
        (events[correction.end_at.value] ||= {})[:end] = correction
      end

      first_time = @corrections.first.start_at.value

      result = []

      events.keys.sort.each do |time|
        event = events[time]

        if time < first_time
          # The leading anchor transition (the last original transition
          # before the first correction start) is emitted unchanged so that
          # periods preceding the first correction remain correct.
          result << event[:base]
          next
        end

        if event.key?(:start)
          # At a correction start the correction applies (inclusive boundary).
          new_offset = event[:start].offset
          boundary = true
        elsif event.key?(:end)
          # At a correction end the base time zone's offset rejoins (the end
          # boundary is exclusive).
          new_offset = base_offset_at(time)
          boundary = true
        else
          # A purely original transition. It is suppressed while a correction
          # is in effect (the correction overrides the offset throughout its
          # interval, even when the interval spans multiple original
          # transitions).
          next if correction_at(time)

          new_offset = event[:base].offset
          boundary = false
        end

        previous_offset = result.empty? ? offset_before(first_time) : result.last.offset

        # Synthetic boundary transitions are always emitted so that each
        # corrected interval is represented as its own distinct period, even
        # when the observed offset is unchanged from the adjacent periods.
        # Original transitions that do not change the offset are omitted.
        next if !boundary && new_offset == previous_offset

        result << TimezoneTransition.new(new_offset, previous_offset, time)
      end

      result
    end

    # @return [Array<TimezoneTransition>] the base time zone transitions
    #   relevant to the corrected range. These are:
    #
    #   * the last original transition strictly before the first correction
    #     start (if any), which anchors the effective sequence so that
    #     periods before the first correction remain correct;
    #   * transitions at or after the start of the first correction and
    #     strictly before the last correction end (those strictly inside a
    #     corrected interval are suppressed later);
    #   * the first original transition at or after the last correction end.
    #
    # The trailing transition is required even when it occurs after the last
    # correction end, because the synthetic end transition may introduce no
    # offset change; the first subsequent original transition must still be
    # represented in the effective sequence. A transition exactly at the
    # last correction end is handled by the end event itself.
    def relevant_base_transitions
      first = @corrections.first.start_at.value
      last = @corrections.last.end_at.value

      transitions = []

      leading = @base_timezone.transitions_up_to(Timestamp.utc(first)).last
      transitions << leading if leading

      transitions.concat(@base_timezone.transitions_up_to(Timestamp.utc(last), Timestamp.utc(first)))

      # The first original transition at or after the last correction end.
      # Only the first is required (the {DataTimezone#period_for} lookup is
      # used beyond that point). The upper bound is advanced by a year and
      # the result truncated to the first match; `to` must be later than
      # `from`, which would not hold for an equal bound.
      trailing = @base_timezone.transitions_up_to(Timestamp.utc(last + 31_536_000), Timestamp.utc(last)).first
      transitions << trailing if trailing

      transitions.uniq(&:timestamp_value).sort_by(&:timestamp_value)
    end

    # Determines the offset observed by the base time zone at and after the
    # given UTC timestamp.
    #
    # @param timestamp_value [Integer] a UTC timestamp value.
    # @return [TimezoneOffset] the offset observed by the base time zone at
    #   `timestamp_value`.
    def base_offset_at(timestamp_value)
      @base_timezone.period_for(Timestamp.utc(timestamp_value)).offset
    end

    # Determines the offset observed immediately before the given UTC
    # timestamp.
    #
    # @param timestamp_value [Integer] a UTC timestamp value.
    # @return [TimezoneOffset] the offset observed before `timestamp_value`.
    def offset_before(timestamp_value)
      @base_timezone.period_for(Timestamp.utc(timestamp_value - 1)).offset
    end

    # Returns the correction in effect at the given UTC timestamp, or `nil` if
    # the timestamp falls outside every corrected interval.
    #
    # Correction intervals are half-open (`[start_at, end_at)`).
    #
    # @param timestamp_value [Integer] a UTC timestamp value.
    # @return [TimezoneCorrection] the correction in effect, or `nil`.
    def correction_at(timestamp_value)
      @corrections.bsearch do |correction|
        if timestamp_value < correction.start_at.value
          1
        elsif timestamp_value >= correction.end_at.value
          -1
        else
          0
        end
      end
    end
  end
end
