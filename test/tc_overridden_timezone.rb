# encoding: UTF-8
# frozen_string_literal: true

require_relative 'test_utils'

class TCOverriddenTimezone < Minitest::Test
  include TZInfo

  # A time zone with a handful of transitions, recorded independently of the
  # data source so the tests don't depend on tzinfo-data being installed.
  class ControlledTimezone < Timezone
    def initialize(identifier, periods)
      super()
      @identifier = identifier
      @periods = periods # sorted TransitionsTimezonePeriod / OffsetTimezonePeriod
    end

    def identifier
      @identifier
    end

    def period_for(time)
      ts = TZInfo::Timestamp.for(time)
      raise ArgumentError, 'time must have a specified utc_offset' unless ts.utc_offset
      v = ts.value
      p = @periods.find do |period|
        (!period.start_transition || period.start_transition.timestamp_value <= v) &&
          (!period.end_transition || v < period.end_transition.timestamp_value)
      end
      raise "no controlled period for #{v}" unless p
      p
    end

    def periods_for_local(local_time)
      ts = TZInfo::Timestamp.for(local_time, :ignore)
      @periods.find_all do |period|
        v = ts.value - period.observed_utc_offset
        (!period.start_transition || period.start_transition.timestamp_value <= v) &&
          (!period.end_transition || v < period.end_transition.timestamp_value)
      end
    end

    def transitions_up_to(to, from = nil)
      to_ts = TZInfo::Timestamp.for(to)
      from_ts = from && TZInfo::Timestamp.for(from)
      transitions = @periods.map(&:start_transition).compact
      transitions.find_all do |t|
        t.timestamp_value < to_ts.value && (!from_ts || t.timestamp_value >= from_ts.value)
      end
    end

    def canonical_zone
      self
    end
  end

  # Builds a transition timeline from [[at_value, offset], ...], using the
  # given offset as the offset in effect before the first transition.
  def self.build_timeline(identifier, initial_offset, entries)
    transitions = []
    previous = initial_offset
    entries.each do |at_value, offset|
      transitions << TimezoneTransition.new(offset, previous, at_value)
      previous = offset
    end

    periods = []
    transitions.each_with_index do |transition, i|
      periods << TransitionsTimezonePeriod.new(i == 0 ? nil : transitions[i - 1], transition)
    end
    periods << TransitionsTimezonePeriod.new(transitions.last, nil)
    ControlledTimezone.new(identifier, periods)
  end

  ST = TimezoneOffset.new(0, 0, 'ST')      # observed +00:00
  DT = TimezoneOffset.new(0, 3600, 'DT')  # observed +01:00
  X1 = TimezoneOffset.new(-7200, 0, 'X1') # observed -02:00 (erratum)
  X2 = TimezoneOffset.new(0, 7200, 'X2')  # observed +02:00 (erratum, DST)

  def controlled_zone
    # 2020-01-01 00:00 UTC ST(+0) -> 2020-07-01 00:00 UTC DT(+1) -> 2021-01-01 ST.
    self.class.build_timeline('Controlled/Zone', ST, [
      [Time.utc(2020, 7, 1).to_i, DT],
      [Time.utc(2021, 1, 1).to_i, ST]
    ])
  end

  def test_identifier_default_and_custom
    tz = controlled_zone
    view = tz.with_overrides([])
    assert_equal('Controlled/Zone (overridden)', view.identifier)

    view2 = tz.with_overrides([], 'Custom/Erratum')
    assert_equal('Custom/Erratum', view2.identifier)

    view3 = Timezone.override(tz, [], 'Class/Entry')
    assert_equal('Class/Entry', view3.identifier)
    assert_kind_of(OverriddenTimezone, view3)
  end

  def test_empty_overrides_match_base
    tz = controlled_zone
    view = tz.with_overrides([])

    [Time.utc(2020, 3), Time.utc(2020, 8), Time.utc(2021, 6)].each do |t|
      assert_equal(tz.period_for(t).offset, view.period_for(t).offset)
    end

    assert_equal(tz.transitions_up_to(Time.utc(2022), Time.utc(2019)).map(&:timestamp_value),
      view.transitions_up_to(Time.utc(2022), Time.utc(2019)).map(&:timestamp_value))
  end

  def test_initialize_validates_arguments
    tz = controlled_zone
    override = TimezoneOverride.new(Time.utc(2020, 3), Time.utc(2020, 4), X1)

    assert_raises(ArgumentError) { OverriddenTimezone.new(nil, []) }
    assert_raises(ArgumentError) { OverriddenTimezone.new('Controlled/Zone', []) }
    assert_raises(ArgumentError) { OverriddenTimezone.new(tz, nil) }
    assert_raises(ArgumentError) { OverriddenTimezone.new(tz, [override, :junk]) }
  end

  def test_rejects_overlapping_overrides
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 3, 1), Time.utc(2020, 5, 1), X1)
    b = TimezoneOverride.new(Time.utc(2020, 4, 1), Time.utc(2020, 6, 1), X2)

    error = assert_raises(ArgumentError) { tz.with_overrides([a, b]) }
    assert_match(/overlap/i, error.message)
    # Ordering must not hide the overlap.
    assert_raises(ArgumentError) { tz.with_overrides([b, a]) }
  end

  def test_touching_windows_are_permitted
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 3, 1), Time.utc(2020, 4, 1), X1)
    b = TimezoneOverride.new(Time.utc(2020, 4, 1), Time.utc(2020, 5, 1), X2)
    view = tz.with_overrides([b, a])

    boundary = Time.utc(2020, 4, 1).to_i
    assert_equal('X1', view.period_for(Time.utc(2020, 3, 31, 23, 59, 59)).abbreviation)
    at_boundary = view.period_for(Time.utc(2020, 4, 1))
    assert_equal('X2', at_boundary.abbreviation)
    assert_equal(boundary, at_boundary.start_transition.timestamp_value)
    # The two windows share a single transition at the touching boundary.
    assert_same(at_boundary.start_transition,
      view.period_for(Time.utc(2020, 3, 31, 23, 59, 59)).end_transition)

    transitions = view.transitions_up_to(Time.utc(2020, 6), Time.utc(2020, 2))
    assert_equal(1, transitions.count {|t| t.timestamp_value == boundary})
  end

  def test_period_for_window_cuts_middle_of_original_period
    tz = controlled_zone
    # Window entirely inside the first ST period.
    a = TimezoneOverride.new(Time.utc(2020, 3, 1), Time.utc(2020, 4, 1), X1)
    view = tz.with_overrides([a])

    before = view.period_for(Time.utc(2020, 2, 29))
    inside = view.period_for(Time.utc(2020, 3, 15))
    after = view.period_for(Time.utc(2020, 4, 2))

    assert_equal('ST', before.abbreviation)
    assert_nil(before.start_transition) # original period was unbounded at start
    assert_equal(Time.utc(2020, 3, 1).to_i, before.end_transition.timestamp_value)

    assert_equal('X1', inside.abbreviation)
    assert_equal(-7200, inside.observed_utc_offset)
    assert_equal(Time.utc(2020, 3, 1).to_i, inside.start_transition.timestamp_value)
    assert_equal(Time.utc(2020, 4, 1).to_i, inside.end_transition.timestamp_value)
    assert_equal('ST', inside.start_transition.previous_offset.abbreviation)
    assert_equal('ST', inside.end_transition.offset.abbreviation)

    assert_equal('ST', after.abbreviation)
    assert_equal(Time.utc(2020, 4, 1).to_i, after.start_transition.timestamp_value)
    assert_equal(Time.utc(2020, 7, 1).to_i, after.end_transition.timestamp_value)
  end

  def test_period_for_window_spans_multiple_original_transitions
    tz = controlled_zone
    # Spans both the 2020-07 ST->DT and 2021-01 DT->ST transitions.
    a = TimezoneOverride.new(Time.utc(2020, 6, 1), Time.utc(2021, 3, 1), X1)
    view = tz.with_overrides([a])

    assert_equal('X1', view.period_for(Time.utc(2020, 7, 15)).abbreviation)
    assert_equal('X1', view.period_for(Time.utc(2020, 12, 25)).abbreviation)

    # When the window ends, reconnect to the offset actually in effect then (ST).
    end_period = view.period_for(Time.utc(2021, 3, 1))
    assert_equal('ST', end_period.abbreviation)
    end_transition = end_period.start_transition
    assert_equal(Time.utc(2021, 3, 1).to_i, end_transition.timestamp_value)
    assert_equal('X1', end_transition.previous_offset.abbreviation)
    assert_equal('ST', end_transition.offset.abbreviation)
    assert_nil(end_period.end_transition) # unbounded tail
  end

  def test_period_for_boundary_inclusivity
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 3, 1), Time.utc(2020, 4, 1), X1)
    view = tz.with_overrides([a])

    assert_equal('ST', view.period_for(Time.utc(2020, 2, 29, 23, 59, 59)).abbreviation)
    assert_equal('X1', view.period_for(Time.utc(2020, 3, 1, 0, 0, 0)).abbreviation)
    assert_equal('X1', view.period_for(Time.utc(2020, 3, 31, 23, 59, 59)).abbreviation)
    assert_equal('ST', view.period_for(Time.utc(2020, 4, 1, 0, 0, 0)).abbreviation)
  end

  def test_window_start_on_original_transition
    # Window starting exactly when ST -> DT: the original transition is
    # replaced by the boundary, whose previous offset is ST.
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 7, 1), Time.utc(2020, 8, 1), X1)
    view = tz.with_overrides([a])

    assert_equal('ST', view.period_for(Time.utc(2020, 6, 30, 23, 59, 59)).abbreviation)
    at_start = view.period_for(Time.utc(2020, 7, 1))
    assert_equal('X1', at_start.abbreviation)
    assert_equal('ST', at_start.start_transition.previous_offset.abbreviation)
    assert_equal(Time.utc(2020, 7, 1).to_i, at_start.start_transition.timestamp_value)

    # The original ST -> DT transition must not appear separately.
    values = view.transitions_up_to(Time.utc(2021), Time.utc(2020, 6)).map(&:timestamp_value)
    assert_equal(1, values.count(Time.utc(2020, 7, 1).to_i))
  end

  def test_window_end_on_original_transition
    # Window ending exactly when DT -> ST: the clipped period after the window
    # starts on the synthetic boundary (X1 -> ST), not the original transition.
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 12, 1), Time.utc(2021, 1, 1), X1)
    view = tz.with_overrides([a])

    at_end = view.period_for(Time.utc(2021, 1, 1))
    assert_equal('ST', at_end.abbreviation)
    start_transition = at_end.start_transition
    assert_equal('X1', start_transition.previous_offset.abbreviation)
    assert_equal('ST', start_transition.offset.abbreviation)
    assert_equal(Time.utc(2021, 1, 1).to_i, start_transition.timestamp_value)

    values = view.transitions_up_to(Time.utc(2021, 2), Time.utc(2020, 11)).map(&:timestamp_value)
    assert_equal(1, values.count(Time.utc(2021, 1, 1).to_i))

    # period_for and periods_for_local agree at the boundary.
    periods = view.periods_for_local(Time.utc(2021, 1, 1, 0, 0))
    assert_equal(1, periods.length)
    assert_equal('ST', periods.first.abbreviation)
  end

  def test_three_touching_windows_share_transitions
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 3, 1), Time.utc(2020, 4, 1), X1)
    b = TimezoneOverride.new(Time.utc(2020, 4, 1), Time.utc(2020, 5, 1), X2)
    c = TimezoneOverride.new(Time.utc(2020, 5, 1), Time.utc(2020, 6, 1),
      TimezoneOffset.new(3600, 0, 'X3'))
    view = tz.with_overrides([c, a, b])

    assert_equal('X1', view.period_for(Time.utc(2020, 3, 15)).abbreviation)
    assert_equal('X2', view.period_for(Time.utc(2020, 4, 1)).abbreviation)
    assert_equal('X3', view.period_for(Time.utc(2020, 5, 1)).abbreviation)

    first = view.period_for(Time.utc(2020, 3, 31, 23, 59, 59))
    second = view.period_for(Time.utc(2020, 4, 1))
    assert_same(first.end_transition, second.start_transition)

    transitions = view.transitions_up_to(Time.utc(2020, 7), Time.utc(2020, 2))
    assert_equal([Time.utc(2020, 3, 1).to_i, Time.utc(2020, 4, 1).to_i,
      Time.utc(2020, 5, 1).to_i, Time.utc(2020, 6, 1).to_i],
      transitions.map(&:timestamp_value))
  end

  def test_period_for_validations
    view = controlled_zone.with_overrides([])
    assert_raises(ArgumentError) { view.period_for(nil) }
    assert_raises(ArgumentError) { view.period_for(Timestamp.send(:new!, 1, 0, nil)) }
  end

  def test_sub_second_times_are_supported
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 3, 1), Time.utc(2020, 4, 1), X1)
    view = tz.with_overrides([a])

    assert_equal('X1', view.period_for(Timestamp.utc(Time.utc(2020, 3, 1).to_i, Rational(5, 10))).abbreviation)
    assert_equal('ST', view.period_for(Timestamp.utc(Time.utc(2020, 2, 29, 23, 59, 59).to_i, Rational(5, 10))).abbreviation)
  end

  def test_transitions_up_to_hides_originals_and_adds_boundaries
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 6, 1), Time.utc(2021, 3, 1), X1)
    view = tz.with_overrides([a])

    transitions = view.transitions_up_to(Time.utc(2021, 6), Time.utc(2019, 6))
    values = transitions.map(&:timestamp_value)

    # The two original transitions inside the window are replaced.
    refute_includes(values, Time.utc(2020, 7, 1).to_i)
    refute_includes(values, Time.utc(2021, 1, 1).to_i)
    assert_includes(values, Time.utc(2020, 6, 1).to_i)
    assert_includes(values, Time.utc(2021, 3, 1).to_i)
    assert_equal(values.sort, values)

    start_transition = transitions.find {|t| t.timestamp_value == Time.utc(2020, 6, 1).to_i}
    assert_equal('ST', start_transition.previous_offset.abbreviation)
    assert_equal('X1', start_transition.offset.abbreviation)

    end_transition = transitions.find {|t| t.timestamp_value == Time.utc(2021, 3, 1).to_i}
    assert_equal('X1', end_transition.previous_offset.abbreviation)
    assert_equal('ST', end_transition.offset.abbreviation) # actual offset at end
  end

  def test_transitions_up_to_boundary_semantics
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 3, 1), Time.utc(2020, 4, 1), X1)
    view = tz.with_overrides([a])
    boundary = Time.utc(2020, 3, 1).to_i

    # `to` is exclusive.
    refute_includes(view.transitions_up_to(Time.utc(2020, 3, 1)).map(&:timestamp_value), boundary)
    assert_includes(view.transitions_up_to(Time.utc(2020, 3, 1, 0, 0, 1)).map(&:timestamp_value), boundary)

    # `from` is inclusive.
    assert_equal(boundary,
      view.transitions_up_to(Time.utc(2020, 5), Time.utc(2020, 3, 1)).first.timestamp_value)
  end

  def test_transitions_up_to_validations
    view = controlled_zone.with_overrides([])
    assert_raises(ArgumentError) { view.transitions_up_to(nil) }
    assert_raises(ArgumentError) { view.transitions_up_to(Time.utc(2020), Time.utc(2021)) }
  end

  def test_periods_for_local_unambiguous
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 3, 1), Time.utc(2020, 4, 1), X1)
    view = tz.with_overrides([a])

    # 2020-03-15 10:00 local with X1(-2) -> 12:00 UTC, inside window.
    periods = view.periods_for_local(Time.utc(2020, 3, 15, 10, 0))
    assert_equal(1, periods.length)
    assert_equal('X1', periods.first.abbreviation)

    # Outside the window, the original period resolves.
    periods = view.periods_for_local(Time.utc(2020, 2, 15, 10, 0))
    assert_equal(1, periods.length)
    assert_equal('ST', periods.first.abbreviation)
  end

  def test_periods_for_local_new_setback_is_visible
    # Window ending at 2020-03-01 00:00 UTC with X2(+2) while the original
    # ST(+0) is in effect: a new 2-hour setback is produced at the boundary.
    # Local 01:30 occurs twice: 23:30 UTC (X2) and 01:30 UTC (ST).
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 2, 1), Time.utc(2020, 3, 1), X2)
    view = tz.with_overrides([a])

    ambiguous_local = Time.utc(2020, 3, 1, 1, 30)
    periods = view.periods_for_local(ambiguous_local)

    assert_equal(2, periods.length)
    assert_equal([7200, 0], periods.map(&:observed_utc_offset)) # chronological order
    assert_equal('X2', periods[0].abbreviation)
    assert_equal('ST', periods[1].abbreviation)

    # Each candidate is consistent with period_for at its resolved UTC instant.
    assert_equal(view.period_for(Time.utc(2020, 2, 29, 23, 30)), periods[0])
    assert_equal(view.period_for(Time.utc(2020, 3, 1, 1, 30)), periods[1])

    # period_for_local can resolve the ambiguity with dst.
    assert_equal('X2', view.period_for_local(ambiguous_local, true).abbreviation)
    assert_equal('ST', view.period_for_local(ambiguous_local, false).abbreviation)

    # Without resolution, AmbiguousTime is raised.
    assert_raises(AmbiguousTime) { view.period_for_local(ambiguous_local) }

    # local_to_utc also sees the overlay.
    assert_equal(Time.utc(2020, 2, 29, 23, 30), view.local_to_utc(ambiguous_local, true))
    assert_equal(Time.utc(2020, 3, 1, 1, 30), view.local_to_utc(ambiguous_local, false))

    # A block can resolve it.
    chosen = view.period_for_local(ambiguous_local) {|candidates| candidates.first }
    assert_equal(periods.first, chosen)
  end

  def test_periods_for_local_new_forward_gap_is_visible
    # Window starting 2020-03-01 00:00 UTC with X1(-2) replacing ST(+0): local
    # jumps from 22:00 Feb 29 back... use +2 to create a forward gap instead:
    # ST(+0) local 00:00 -> X2(+2) local 02:00, so [01:00, 02:00) is invalid.
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 3, 1), Time.utc(2020, 4, 1), X2)
    view = tz.with_overrides([a])

    assert_empty(view.periods_for_local(Time.utc(2020, 3, 1, 1, 30)))
    assert_raises(PeriodNotFound) { view.period_for_local(Time.utc(2020, 3, 1, 1, 30)) }

    assert_equal(1, view.periods_for_local(Time.utc(2020, 3, 1, 2, 30)).length)
  end

  def test_periods_for_local_consistent_with_period_for
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 6, 1), Time.utc(2021, 3, 1), X1)
    view = tz.with_overrides([a])

    [Time.utc(2020, 1, 1), Time.utc(2020, 6, 15), Time.utc(2020, 12, 1),
     Time.utc(2021, 3, 1), Time.utc(2021, 6, 1)].each do |local|
      view.periods_for_local(local).each do |period|
        utc_value = local.to_i - period.observed_utc_offset
        assert_equal(period, view.period_for(Timestamp.utc(utc_value)))
      end
    end
  end

  def test_canonical_zone
    tz = controlled_zone
    view = tz.with_overrides([])
    assert_same(tz, view.canonical_zone)
    assert_equal('Controlled/Zone', view.canonical_identifier)
  end

  def test_marshal_is_not_supported
    tz = controlled_zone
    a = TimezoneOverride.new(Time.utc(2020, 3), Time.utc(2020, 4), X1)
    view = tz.with_overrides([a])
    assert_raises(TypeError) { Marshal.dump(view) }
  end

  def test_constant_offset_base_zone
    # A base zone without any transitions.
    offset_period = OffsetTimezonePeriod.new(TimezoneOffset.new(0, 0, 'UTC'))
    tz = ControlledTimezone.new('Controlled/Constant', [offset_period])
    a = TimezoneOverride.new(Time.utc(2024, 1, 1), Time.utc(2024, 2, 1), X1)
    view = tz.with_overrides([a])

    assert_equal('X1', view.period_for(Time.utc(2024, 1, 15)).abbreviation)
    assert_equal('UTC', view.period_for(Time.utc(2025)).abbreviation)

    transitions = view.transitions_up_to(Time.utc(2025), Time.utc(2023))
    assert_equal([Time.utc(2024, 1, 1).to_i, Time.utc(2024, 2, 1).to_i],
      transitions.map(&:timestamp_value))
  end

  # Integration test against a real data source: an erratum spanning both the
  # 2024 spring-forward and fall-back transitions of America/New_York.
  def test_real_zone_erratum_spans_two_transitions
    tz = Timezone.get('America/New_York')

    # Observed -04:30, presented as -05:00 standard + 00:30 DST.
    erratum = TimezoneOffset.new(-18000, 1800, 'XDT')
    a = TimezoneOverride.new(Time.utc(2024, 3, 1), Time.utc(2024, 12, 15), erratum)
    view = tz.with_overrides([a])

    # Before the window: original EST, ending at the override start.
    before = view.period_for(Time.utc(2024, 2, 29, 23, 59, 59))
    assert_equal('EST', before.abbreviation)
    assert_equal(Time.utc(2024, 3, 1).to_i, before.end_transition.timestamp_value)

    # Inside the window: the erratum applies even at instants that would have
    # been EDT under the original rules.
    [Time.utc(2024, 3, 10, 8), Time.utc(2024, 8, 1), Time.utc(2024, 11, 4, 6)].each do |t|
      period = view.period_for(t)
      assert_equal('XDT', period.abbreviation)
      assert_equal(-16200, period.observed_utc_offset)
    end

    # After the window: reconnect to the offset actually in effect then (EST),
    # not the offset in effect when the window started.
    after = view.period_for(Time.utc(2024, 12, 15))
    assert_equal('EST', after.abbreviation)
    assert_equal(-18000, after.observed_utc_offset)
    assert_equal(Time.utc(2024, 12, 15).to_i, after.start_transition.timestamp_value)

    # The original transitions inside the window are suppressed.
    transitions = view.transitions_up_to(Time.utc(2025, 1, 1), Time.utc(2024, 1, 1)).map(&:timestamp_value)
    refute_includes(transitions, Time.utc(2024, 3, 10, 7).to_i)
    refute_includes(transitions, Time.utc(2024, 11, 3, 6).to_i)
    assert_includes(transitions, Time.utc(2024, 3, 1).to_i)
    assert_includes(transitions, Time.utc(2024, 12, 15).to_i)

    # The override ending at 00:00 UTC introduces a fresh setback: local time
    # 19:15 on Dec 14 is seen twice (19:30 XDT local at end vs 19:00 EST), so
    # local-time reverse resolution observes the overlay.
    ambiguous_local = Time.utc(2024, 12, 14, 19, 15)
    periods = view.periods_for_local(ambiguous_local)
    assert_equal(2, periods.length)
    assert_equal(['XDT', 'EST'], periods.map(&:abbreviation))
    assert_equal(Time.utc(2024, 12, 14, 23, 45), view.local_to_utc(ambiguous_local, true))
    assert_equal(Time.utc(2024, 12, 15, 0, 15), view.local_to_utc(ambiguous_local, false))
    assert_raises(AmbiguousTime) { view.local_to_utc(ambiguous_local) }

    # The original zone is unaffected outside the view.
    assert_equal('EDT', tz.period_for(Time.utc(2024, 8, 1)).abbreviation)
  end
end
