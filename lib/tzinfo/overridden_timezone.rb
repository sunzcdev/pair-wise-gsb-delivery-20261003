# encoding: UTF-8
# frozen_string_literal: true

module TZInfo
  # An {OverriddenTimezone} is an overlay view of an existing {Timezone} that
  # applies one or more errata ({TimezoneOverride} instances) over bounded UTC
  # time windows.
  #
  # Within each override window, the complete offset recorded by the
  # {TimezoneOverride} is used. Outside of the windows, the rules of the
  # underlying time zone continue to apply unchanged. When an override ends,
  # the view reverts to whatever offset the underlying time zone actually
  # observes at that instant (even if the override window spanned multiple
  # original transitions).
  #
  # Override windows may cut through the middle of an original period and may
  # span multiple original transitions. The results of {#period_for},
  # {#periods_for_local} and {#transitions_up_to} are always consistent with
  # each other.
  #
  # Override windows must not overlap each other (windows that only touch at a
  # boundary are permitted and merged into a single transition). Use
  # {Timezone#with_overrides} to construct an {OverriddenTimezone}.
  class OverriddenTimezone < Timezone
    # Initializes a new {OverriddenTimezone}.
    #
    # {OverriddenTimezone} instances should not normally be created directly.
    # Use the {Timezone#with_overrides} method instead.
    #
    # @param base_timezone [Timezone] the existing {Timezone} whose rules apply
    #   outside of the override windows.
    # @param overrides [Array<TimezoneOverride>] the errata to apply, each
    #   giving an inclusive UTC start time, an exclusive UTC end time and
    #   complete offset information.
    # @param identifier [String] an optional identifier for the overlay view.
    #   Defaults to `"``<base identifier>`` (overridden)"`.
    # @raise [ArgumentError] if `base_timezone` is not a {Timezone}.
    # @raise [ArgumentError] if `overrides` is not an `Array` of
    #   {TimezoneOverride} instances.
    # @raise [ArgumentError] if two override windows overlap.
    def initialize(base_timezone, overrides, identifier = nil)
      super()

      raise ArgumentError, 'base_timezone must be a TZInfo::Timezone' unless base_timezone.kind_of?(Timezone)
      raise ArgumentError, 'overrides must be an Array' unless overrides.kind_of?(Array)
      unless overrides.all? {|o| o.kind_of?(TimezoneOverride)}
        raise ArgumentError, 'overrides must contain TZInfo::TimezoneOverride instances'
      end

      @base_timezone = base_timezone
      @identifier = (identifier || "#{base_timezone.identifier} (overridden)").dup.freeze
      @overrides = overrides.sort_by(&:start_value).freeze

      validate_no_overlapping_overrides
      build_synthetic_transitions
      freeze
    end

    # @return [String] the identifier of the overlay view.
    def identifier
      @identifier
    end

    # (see Timezone#period_for)
    def period_for(time)
      raise ArgumentError, 'time must be specified' unless time
      timestamp = Timestamp.for(time)
      raise ArgumentError, 'time must have a specified utc_offset' unless timestamp.utc_offset
      period_for_utc_value(timestamp.value)
    end

    # (see Timezone#periods_for_local)
    def periods_for_local(local_time)
      raise ArgumentError, 'local_time must be specified' unless local_time
      local_timestamp = Timestamp.for(local_time, :ignore)
      local_value = local_timestamp.value

      results = []

      # Each override window contributes at most one period (the offset within
      # a window is constant).
      @overrides.each_with_index do |override, i|
        utc_value = local_value - override.offset.observed_utc_offset
        if utc_value >= override.start_value && utc_value < override.end_value
          results << period_for_utc_value(utc_value)
        end
      end

      # Periods from the underlying time zone are retained unless the UTC
      # instant they resolve to falls inside an override window. Their bounds
      # are clipped to the surrounding synthetic transitions.
      @base_timezone.periods_for_local(local_timestamp).each do |base_period|
        utc_value = local_value - base_period.observed_utc_offset
        unless override_index_for(utc_value)
          results << clip_base_period(base_period, utc_value)
        end
      end

      # Return in chronological order of the resolved UTC instants (the same
      # ordering used by TransitionsDataTimezoneInfo).
      results.sort_by! {|period| local_value - period.observed_utc_offset}
      results
    end

    # (see Timezone#transitions_up_to)
    def transitions_up_to(to, from = nil)
      raise ArgumentError, 'to must be specified' unless to
      to_timestamp = Timestamp.for(to)
      raise ArgumentError, 'to must have a specified utc_offset' unless to_timestamp.utc_offset

      if from
        from_timestamp = Timestamp.for(from)
        raise ArgumentError, 'from must have a specified utc_offset' unless from_timestamp.utc_offset
        raise ArgumentError, 'to must be greater than from' if to_timestamp <= from_timestamp
      else
        from_timestamp = nil
      end

      to_value = to_timestamp.value
      from_value = from_timestamp && from_timestamp.value

      result = []

      # Original transitions that fall on or inside an override window are
      # replaced by the synthetic boundary transitions (the synthetic
      # transition at the end of a window already records the offset the
      # underlying zone actually observes at that instant).
      @base_timezone.transitions_up_to(to_timestamp, from_timestamp).each do |transition|
        result << transition unless on_or_within_override?(transition.timestamp_value)
      end

      @boundary_transitions.each do |transition|
        value = transition.timestamp_value
        next if value >= to_value
        next if from_value && value < from_value
        result << transition
      end

      result.sort_by!(&:timestamp_value)
      result
    end

    # The canonical zone of an {OverriddenTimezone} is the canonical zone of
    # the underlying time zone.
    #
    # @return [Timezone] the canonical {Timezone} of the base time zone.
    def canonical_zone
      @base_timezone.canonical_zone
    end

    # Overlay views cannot be marshalled: the override windows are not part of
    # any {DataSource}, so restoring the identifier alone would not reproduce
    # the view.
    #
    # @raise [TypeError] always.
    def _dump(_limit)
      raise TypeError, 'TZInfo::OverriddenTimezone cannot be marshalled (construct the view with Timezone#with_overrides instead)'
    end

    private

    # Validates that the sorted override windows do not overlap. Windows that
    # only touch (the end of one equals the start of the next) are allowed.
    #
    # @raise [ArgumentError] if two windows overlap.
    def validate_no_overlapping_overrides
      @overrides.each_cons(2) do |a, b|
        if b.start_value < a.end_value
          raise ArgumentError, "override windows overlap: #{a.inspect} and #{b.inspect}"
        end
      end
    end

    # Builds the synthetic transitions that mark the start and end of each
    # override window. Adjacent windows that touch at a boundary share a single
    # transition directly from one override offset to the next.
    def build_synthetic_transitions
      @start_values = @overrides.map(&:start_value)
      @end_values = @overrides.map(&:end_value)

      start_transitions = Array.new(@overrides.length)
      end_transitions = Array.new(@overrides.length)
      boundary_transitions = []

      # One shared transition for each boundary where two windows touch.
      shared_touching_transitions = {}
      @overrides.each_cons(2) do |previous_window, next_window|
        if previous_window.end_value == next_window.start_value
          shared_touching_transitions[previous_window.end_value] =
            TimezoneTransition.new(next_window.offset, previous_window.offset, previous_window.end_value)
        end
      end

      @overrides.each_with_index do |override, i|
        if i > 0 && @overrides[i - 1].end_value == override.start_value
          # Two windows touch: the end transition of the previous window and
          # the start transition of this window are one and the same (so it is
          # stored in both parallel arrays).
          transition = shared_touching_transitions[override.start_value]
          start_transitions[i] = transition
        else
          # The previous offset is the offset observed by the underlying zone
          # immediately before the window (at start - 1), so that the
          # transition chain remains correct even when a window starts on an
          # original transition.
          previous_offset = offset_of_base_at(override.start_value - 1)
          start_transition = TimezoneTransition.new(override.offset, previous_offset, override.start_value)
          start_transitions[i] = start_transition
          boundary_transitions << start_transition
        end

        if touching_next?(i)
          # The end transition of this window is the shared transition at the
          # touching boundary (also used as the next window's start).
          end_transitions[i] = shared_touching_transitions[override.end_value]
        else
          # Reconnect to the offset the underlying zone observes at the end
          # instant (the end boundary is the start of the next period).
          next_offset = offset_of_base_at(override.end_value)
          end_transition = TimezoneTransition.new(next_offset, override.offset, override.end_value)
          end_transitions[i] = end_transition
          boundary_transitions << end_transition
        end
      end

      @start_transitions = start_transitions.freeze
      @end_transitions = end_transitions.freeze

      boundary_transitions.concat(shared_touching_transitions.values)
      boundary_transitions.sort_by!(&:timestamp_value)
      @boundary_transitions = boundary_transitions.freeze
      @boundary_values = boundary_transitions.map(&:timestamp_value).freeze
    end

    # @param value [Integer] a UTC timestamp value.
    # @return [Boolean] `true` if `value` is the boundary of, or falls within,
    #   any override window.
    def on_or_within_override?(value)
      !!override_index_for(value) || !!boundary_index_for(value)
    end

    # @param value [Integer] a UTC timestamp value.
    # @return [Integer, nil] the index of the override window within which
    #   `value` falls (inclusive start, exclusive end), or `nil`.
    def override_index_for(value)
      # Index of the first window starting after value, or the number of
      # windows if every window starts at or before value.
      index = @start_values.bsearch_index {|start_value| start_value > value } || @overrides.length
      candidate = index - 1
      candidate if candidate >= 0 && value < @end_values[candidate]
    end

    # @param value [Integer] a UTC timestamp value.
    # @return [Integer, nil] the index of the synthetic boundary transition at
    #   `value`, or `nil` if there is no such boundary.
    def boundary_index_for(value)
      index = @boundary_values.bsearch_index {|boundary_value| boundary_value >= value }
      index if index && @boundary_values[index] == value
    end

    # @param value [Integer] a UTC timestamp value.
    # @return [TimezoneOffset] the offset observed by the underlying time zone
    #   at `value` (inclusive at a transition instant, matching period_for).
    def offset_of_base_at(value)
      @base_timezone.period_for(Timestamp.utc(value)).offset
    end

    # @param i [Integer] an override window index.
    # @return [Boolean] `true` if window `i` ends exactly where window `i + 1`
    #   starts.
    def touching_next?(i)
      i + 1 < @overrides.length && @overrides[i + 1].start_value == @end_values[i]
    end

    # Returns the {TimezonePeriod} of the overlay view that is valid at the
    # given UTC timestamp value.
    #
    # @param value [Integer] a UTC timestamp value.
    # @return [TimezonePeriod] the period valid at `value`.
    def period_for_utc_value(value)
      i = override_index_for(value)

      if i
        TransitionsTimezonePeriod.new(@start_transitions[i], @end_transitions[i])
      else
        clip_base_period(@base_timezone.period_for(Timestamp.utc(value)), value)
      end
    end

    # Clips the bounds of a period from the underlying time zone to the
    # synthetic override boundaries surrounding `value`.
    #
    # @param base_period [TimezonePeriod] a period of the underlying time zone.
    # @param value [Integer] the UTC timestamp value covered by `base_period`.
    # @return [TimezonePeriod] the clipped period.
    def clip_base_period(base_period, value)
      start_transition = later_start_transition(base_period.start_transition, value)
      end_transition = earlier_end_transition(base_period.end_transition, value)
      TransitionsTimezonePeriod.new(start_transition, end_transition)
    end

    # @param base_start_transition [TimezoneTransition, nil] the start
    #   transition of the underlying period, or `nil` if unbounded.
    # @param value [Integer] the UTC timestamp value covered by the period.
    # @return [TimezoneTransition, nil] the later of the underlying start
    #   transition and the latest synthetic boundary at or before `value`. A
    #   synthetic boundary replaces an original transition occurring at the
    #   same timestamp.
    def later_start_transition(base_start_transition, value)
      synthetic = boundary_at_or_before(value)
      return base_start_transition unless synthetic
      return synthetic if base_start_transition.nil?

      if synthetic.timestamp_value >= base_start_transition.timestamp_value
        synthetic
      else
        base_start_transition
      end
    end

    # @param base_end_transition [TimezoneTransition, nil] the end transition
    #   of the underlying period, or `nil` if unbounded.
    # @param value [Integer] the UTC timestamp value covered by the period.
    # @return [TimezoneTransition, nil] the earlier of the underlying end
    #   transition and the earliest synthetic boundary strictly after `value`
    #   (a period's end is exclusive). A synthetic boundary replaces an
    #   original transition occurring at the same timestamp.
    def earlier_end_transition(base_end_transition, value)
      synthetic = boundary_after(value)
      return base_end_transition unless synthetic
      return synthetic if base_end_transition.nil?

      if synthetic.timestamp_value <= base_end_transition.timestamp_value
        synthetic
      else
        base_end_transition
      end
    end

    # @param value [Integer] a UTC timestamp value.
    # @return [TimezoneTransition, nil] the latest synthetic boundary
    #   transition whose timestamp is less than or equal to `value`, or `nil`.
    def boundary_at_or_before(value)
      # Index of the first boundary after value.
      index = @boundary_values.bsearch_index {|boundary_value| boundary_value > value }
      index = index ? index - 1 : @boundary_transitions.length - 1
      @boundary_transitions[index] if index && index >= 0
    end

    # @param value [Integer] a UTC timestamp value.
    # @return [TimezoneTransition, nil] the earliest synthetic boundary
    #   transition whose timestamp is greater than `value`, or `nil`.
    def boundary_after(value)
      index = @boundary_values.bsearch_index {|boundary_value| boundary_value > value }
      @boundary_transitions[index] if index
    end
  end
end
