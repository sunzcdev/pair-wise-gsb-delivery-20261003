# encoding: UTF-8
# frozen_string_literal: true

require_relative 'test_utils'

class TCCorrectedTimezone < Minitest::Test
  include TZInfo

  # Builds a DataTimezone from an explicit ordered list of transitions. Each
  # transition is [timestamp_value, offset, previous_offset].
  def build_data_timezone(identifier, transitions)
    built = transitions.map do |at, offset, previous_offset|
      TimezoneTransition.new(offset, previous_offset, at)
    end
    info = DataSources::TransitionsDataTimezoneInfo.new(identifier, built)
    info.create_timezone
  end

  # A simple time zone with two offsets used by most tests:
  # STD (-04:00) up to 2020-03-08 07:00 UTC, DST (-03:00) until
  # 2020-11-01 06:00 UTC, then STD again.
  STD_OFFSET = TimezoneOffset.new(-4 * 3600, 0, 'STD')
  DST_OFFSET = TimezoneOffset.new(-4 * 3600, 3600, 'DST')

  SPRING = Time.utc(2020, 3, 8, 7).to_i
  FALL   = Time.utc(2020, 11, 1, 6).to_i

  def simple_zone(identifier = 'Test/Simple')
    build_data_timezone(identifier, [
      [SPRING, DST_OFFSET, STD_OFFSET],
      [FALL, STD_OFFSET, DST_OFFSET]
    ])
  end

  def correction(s, e, offset)
    TimezoneCorrection.new(Time.at(s).utc, Time.at(e).utc, offset)
  end

  # ------------------------------------------------------------------
  # TimezoneCorrection
  # ------------------------------------------------------------------

  def test_correction_requires_start_and_end
    error = assert_raises(ArgumentError) { TimezoneCorrection.new(nil, Time.utc(2020), STD_OFFSET) }
    assert_match(/start_at/, error.message)
    error = assert_raises(ArgumentError) { TimezoneCorrection.new(Time.utc(2020), nil, STD_OFFSET) }
    assert_match(/end_at/, error.message)
  end

  def test_correction_requires_end_after_start
    assert_raises(ArgumentError) do
      TimezoneCorrection.new(Time.utc(2020, 2), Time.utc(2020, 1), STD_OFFSET)
    end
    assert_raises(ArgumentError) do
      TimezoneCorrection.new(Time.utc(2020, 1), Time.utc(2020, 1), STD_OFFSET)
    end
  end

  def test_correction_rejects_sub_second_boundaries
    assert_raises(ArgumentError) do
      TimezoneCorrection.new(Timestamp.utc(0, Rational(1, 2)), Time.utc(2020, 2), STD_OFFSET)
    end
    assert_raises(ArgumentError) do
      TimezoneCorrection.new(Time.utc(2020), Time.at(Time.utc(2020, 2).to_i, 500, :millisecond).utc, STD_OFFSET)
    end
  end

  def test_correction_requires_complete_offset_information
    assert_raises(ArgumentError) do
      TimezoneCorrection.new(Time.utc(2020), Time.utc(2020, 2), nil, 0, nil, 'X')
    end

    c = TimezoneCorrection.new(Time.utc(2020), Time.utc(2020, 2), nil, -4 * 3600, 3600, 'XYZ')
    assert_equal(-4 * 3600, c.offset.base_utc_offset)
    assert_equal(3600, c.offset.std_offset)
    assert_equal('XYZ', c.offset.abbreviation)
  end

  def test_correction_half_open_interval
    s = Time.utc(2020, 6, 1)
    e = Time.utc(2020, 6, 2)
    c = TimezoneCorrection.new(s, e, STD_OFFSET)
    assert_equal(Timestamp, c.start_at.class)
    assert(c.start_at.utc?)
    assert_equal(s.to_i, c.start_at.value)
    assert_equal(e.to_i, c.end_at.value)
  end

  def test_correction_equality
    c1 = TimezoneCorrection.new(Time.utc(2020), Time.utc(2020, 2), STD_OFFSET)
    c2 = TimezoneCorrection.new(Time.utc(2020), Time.utc(2020, 2), TimezoneOffset.new(-4*3600, 0, 'STD'))
    c3 = TimezoneCorrection.new(Time.utc(2020), Time.utc(2020, 3), STD_OFFSET)
    assert_equal(c1, c2)
    assert_equal(c1.hash, c2.hash)
    refute_equal(c1, c3)
  end

  # ------------------------------------------------------------------
  # Construction and entry point
  # ------------------------------------------------------------------

  def test_with_corrections_from_array
    zone = simple_zone
    c = correction(Time.utc(2020, 6).to_i, Time.utc(2020, 7).to_i, STD_OFFSET)
    view = zone.with_corrections([c], 'Test/View')
    assert_kind_of(CorrectedTimezone, view)
    assert_equal('Test/View', view.identifier)
    assert_same(zone, view.base_timezone)
    assert_equal([c], view.corrections)
  end

  def test_with_corrections_default_identifier
    view = simple_zone('Test/Base').with_corrections([
      correction(Time.utc(2020, 6).to_i, Time.utc(2020, 7).to_i, STD_OFFSET)
    ])
    assert_equal('Test/Base (corrected)', view.identifier)
  end

  def test_with_corrections_with_block
    zone = simple_zone
    view = zone.with_corrections do |b|
      assert_kind_of(CorrectionBuilder, b)
      b.add(Time.utc(2020, 6), Time.utc(2020, 7), STD_OFFSET)
      b.correction(Time.utc(2020, 8), Time.utc(2020, 9), DST_OFFSET)
    end
    assert_equal(2, view.corrections.length)
    assert_equal(Time.utc(2020, 6).to_i, view.corrections[0].start_at.value)
  end

  def test_with_corrections_block_component_offsets
    view = simple_zone.with_corrections do |b|
      b.add(Time.utc(2020, 6), Time.utc(2020, 7), nil, 0, 3600, 'ZZ')
    end
    assert_equal(3600, view.period_for(Timestamp.utc(Time.utc(2020, 6, 15).to_i)).observed_utc_offset)
  end

  def test_requires_corrections
    assert_raises(ArgumentError) { simple_zone.with_corrections }
    assert_raises(ArgumentError) { simple_zone.with_corrections([]) }
  end

  def test_rejects_block_and_corrections
    assert_raises(ArgumentError) do
      simple_zone.with_corrections([correction(1, 2, STD_OFFSET)]) { }
    end
  end

  def test_rejects_non_correction_entries
    assert_raises(ArgumentError) { simple_zone.with_corrections([Object.new]) }
  end

  def test_rejects_overlapping_corrections
    c1 = correction(Time.utc(2020, 1).to_i, Time.utc(2020, 3).to_i, STD_OFFSET)
    c2 = correction(Time.utc(2020, 2).to_i, Time.utc(2020, 4).to_i, DST_OFFSET)
    assert_raises(ArgumentError) { simple_zone.with_corrections([c1, c2]) }

    # Overlap must also be detected when supplied out of order.
    assert_raises(ArgumentError) { simple_zone.with_corrections([c2, c1]) }

    # And through the builder.
    assert_raises(ArgumentError) do
      simple_zone.with_corrections do |b|
        b.add(Time.utc(2020, 2), Time.utc(2020, 4), DST_OFFSET)
        b.add(Time.utc(2020, 1), Time.utc(2020, 3), STD_OFFSET)
      end
    end
  end

  def test_accepts_abutting_corrections
    c1 = correction(Time.utc(2020, 1).to_i, Time.utc(2020, 2).to_i, STD_OFFSET)
    c2 = correction(Time.utc(2020, 2).to_i, Time.utc(2020, 3).to_i, DST_OFFSET)
    view = simple_zone.with_corrections([c1, c2])
    assert_equal('DST', view.period_for(Timestamp.utc(Time.utc(2020, 2).to_i)).abbreviation)
    assert_equal('STD', view.period_for(Timestamp.utc(Time.utc(2020, 1, 31, 23, 59, 59).to_i)).abbreviation)
  end

  # ------------------------------------------------------------------
  # period_for semantics
  # ------------------------------------------------------------------

  def test_period_for_outside_window_unchanged
    zone = simple_zone
    view = zone.with_corrections([
      correction(Time.utc(2020, 6, 1).to_i, Time.utc(2020, 6, 30).to_i, STD_OFFSET)
    ])

    assert_equal(zone.period_for(Timestamp.utc(Time.utc(2020, 5).to_i)).offset,
                 view.period_for(Timestamp.utc(Time.utc(2020, 5).to_i)).offset)
    assert_equal(zone.period_for(Timestamp.utc(Time.utc(2020, 8).to_i)).offset,
                 view.period_for(Timestamp.utc(Time.utc(2020, 8).to_i)).offset)
  end

  def test_period_for_window_boundaries_half_open
    fixed = TimezoneOffset.new(-5 * 3600, 0, 'FIX')
    s = Time.utc(2020, 6, 1, 12).to_i
    e = Time.utc(2020, 6, 30, 12).to_i
    view = simple_zone.with_corrections([correction(s, e, fixed)])

    assert_equal('DST', view.period_for(Timestamp.utc(s - 1)).abbreviation)
    assert_equal('FIX', view.period_for(Timestamp.utc(s)).abbreviation)
    assert_equal('FIX', view.period_for(Timestamp.utc(e - 1)).abbreviation)
    assert_equal('DST', view.period_for(Timestamp.utc(e)).abbreviation)
  end

  def test_period_for_window_spanning_multiple_original_transitions
    fixed = TimezoneOffset.new(-3 * 3600, 0, 'FIX')
    view = simple_zone.with_corrections([
      correction(Time.utc(2020, 1, 1).to_i, Time.utc(2021, 1, 1).to_i, fixed)
    ])

    [Time.utc(2020, 2), Time.utc(2020, 6), Time.utc(2020, 12)].each do |t|
      assert_equal('FIX', view.period_for(Timestamp.utc(t.to_i)).abbreviation)
    end

    # After the window ends, the offset genuinely in effect at that instant
    # (STD in January 2021) is rejoined.
    assert_equal('STD', view.period_for(Timestamp.utc(Time.utc(2021, 1, 1).to_i)).abbreviation)
    assert_equal('STD', view.period_for(Timestamp.utc(Time.utc(2021, 2).to_i)).abbreviation)
  end

  def test_periods_before_and_after_range_remain_correct
    fixed = TimezoneOffset.new(0, 3600, 'LMT')
    view = simple_zone.with_corrections([
      correction(Time.utc(2020, 6, 1).to_i, Time.utc(2020, 6, 30).to_i, fixed)
    ])

    assert_equal('STD', view.period_for(Timestamp.utc(Time.utc(2020, 1).to_i)).abbreviation)
    assert_equal('DST', view.period_for(Timestamp.utc(Time.utc(2020, 5).to_i)).abbreviation)
    assert_equal('STD', view.period_for(Timestamp.utc(Time.utc(2020, 12).to_i)).abbreviation)
  end

  # ------------------------------------------------------------------
  # transitions_up_to
  # ------------------------------------------------------------------

  def test_transitions_include_synthetic_boundaries_and_suppress_inner
    fixed = TimezoneOffset.new(-3 * 3600, 0, 'FIX')
    s = Time.utc(2020, 1, 1).to_i
    e = Time.utc(2021, 1, 1).to_i
    view = simple_zone.with_corrections([correction(s, e, fixed)])

    transitions = view.transitions_up_to(Timestamp.utc(Time.utc(2022).to_i),
                                         Timestamp.utc(Time.utc(2019, 6).to_i))

    # The SPRING and FALL transitions inside the window are suppressed; only
    # the synthetic boundaries and the outer transitions remain.
    at_values = transitions.map(&:timestamp_value)
    assert_includes(at_values, s)
    assert_includes(at_values, e)
    refute_includes(at_values, SPRING)
    refute_includes(at_values, FALL)

    start_t = transitions.find { |t| t.timestamp_value == s }
    end_t   = transitions.find { |t| t.timestamp_value == e }
    assert_equal('FIX', start_t.offset.abbreviation)
    assert_equal('STD', start_t.previous_offset.abbreviation)
    assert_equal('STD', end_t.offset.abbreviation)
    assert_equal('FIX', end_t.previous_offset.abbreviation)
  end

  def test_transitions_from_to_semantics
    s = Time.utc(2020, 6, 1).to_i
    e = Time.utc(2020, 9, 1).to_i
    view = simple_zone.with_corrections([correction(s, e, STD_OFFSET)])

    from_inclusive = view.transitions_up_to(Timestamp.utc(e), Timestamp.utc(s)).map(&:timestamp_value)
    assert_includes(from_inclusive, s)

    to_exclusive = view.transitions_up_to(Timestamp.utc(e)).map(&:timestamp_value)
    refute_includes(to_exclusive, e)
  end

  def test_transitions_consistent_with_period_for
    fixed = TimezoneOffset.new(-2 * 3600, 0, 'FIX')
    view = simple_zone.with_corrections([
      correction(Time.utc(2020, 5, 1).to_i, Time.utc(2020, 10, 1).to_i, fixed)
    ])

    transitions = view.transitions_up_to(Timestamp.utc(Time.utc(2021).to_i),
                                         Timestamp.utc(Time.utc(2019, 12).to_i))

    transitions.each do |t|
      assert_equal(t.previous_offset, view.period_for(Timestamp.utc(t.timestamp_value - 1)).offset)
      assert_equal(t.offset, view.period_for(Timestamp.utc(t.timestamp_value)).offset)
    end
  end

  # ------------------------------------------------------------------
  # periods_for_local: fall-back and gap introduced by corrections
  # ------------------------------------------------------------------

  FALL_S = Time.utc(2020, 7, 1, 0).to_i
  FALL_B = Time.utc(2020, 7, 1, 6).to_i
  FALL_F = Time.utc(2020, 7, 1, 7).to_i
  FALL_E = Time.utc(2020, 8, 1).to_i

  XDT = TimezoneOffset.new(-5 * 3600, 3600, 'XDT')
  XST = TimezoneOffset.new(-5 * 3600, 0, 'XST')

  def fallback_view
    simple_zone.with_corrections([
      correction(FALL_S, FALL_B, XDT),
      correction(FALL_B, FALL_F, XST),
      correction(FALL_F, FALL_E, XDT)
    ])
  end

  def test_correction_introduced_fall_back_repeats_local_hour
    view = fallback_view

    # Local 01:30 occurs once with XDT (UTC 05:30) and again with XST
    # (UTC 06:30).
    periods = view.periods_for_local(Timestamp.create(2020, 7, 1, 1, 30))
    assert_equal(['XDT', 'XST'], periods.map(&:abbreviation))
    offsets = periods.map(&:observed_utc_offset)
    assert_equal([-4 * 3600, -5 * 3600], offsets)

    # The same two UTC instants resolve via period_for to the matching
    # periods, proving the override layer is visible in local resolution.
    assert_equal('XDT', view.period_for(Timestamp.utc(FALL_B - 1800)).abbreviation)
    assert_equal('XST', view.period_for(Timestamp.utc(FALL_B + 1800)).abbreviation)
  end

  def test_correction_introduced_fall_back_ambiguity_resolution
    view = fallback_view
    local = Timestamp.create(2020, 7, 1, 1, 30)

    assert_raises(AmbiguousTime) { view.period_for_local(local) }
    assert_equal('XDT', view.period_for_local(local, true).abbreviation)
    assert_equal('XST', view.period_for_local(local, false).abbreviation)

    assert_equal(Time.at(FALL_B - 1800).utc, view.local_to_utc(Time.new(2020, 7, 1, 1, 30), true).utc)
    assert_equal(Time.at(FALL_B + 1800).utc, view.local_to_utc(Time.new(2020, 7, 1, 1, 30), false).utc)
  end

  def test_base_zone_does_not_see_introduced_ambiguity
    zone = simple_zone
    view = fallback_view
    local = Timestamp.create(2020, 7, 1, 1, 30)

    assert_equal(1, zone.periods_for_local(local).length)
    assert_equal(2, view.periods_for_local(local).length)
  end

  def test_correction_introduced_gap_has_no_periods
    # Spring forward from STD (-4) to DST (-3) at 2020-06-01 06:00 UTC:
    # local clocks jump from 02:00 to 03:00; local 02:30 does not exist.
    view = simple_zone.with_corrections([
      correction(Time.utc(2020, 5, 1).to_i, Time.utc(2020, 6, 1, 6).to_i, STD_OFFSET),
      correction(Time.utc(2020, 6, 1, 6).to_i, Time.utc(2020, 7, 1).to_i, DST_OFFSET)
    ])

    assert_equal([], view.periods_for_local(Timestamp.create(2020, 6, 1, 2, 30)))
    assert_raises(PeriodNotFound) { view.period_for_local(Timestamp.create(2020, 6, 1, 2, 30)) }
  end

  def test_period_for_local_consistent_with_period_for_elsewhere
    fixed = TimezoneOffset.new(-2 * 3600, 0, 'FIX')
    view = simple_zone.with_corrections([
      correction(Time.utc(2020, 5, 1).to_i, Time.utc(2020, 10, 1).to_i, fixed)
    ])

    # An unambiguous local time inside the window resolves to the fixed
    # period and the matching UTC instant.
    local = Timestamp.create(2020, 7, 15, 12)
    period = view.period_for_local(local)
    assert_equal('FIX', period.abbreviation)
    utc_value = local.value - (-2 * 3600)
    assert_equal('FIX', view.period_for(Timestamp.utc(utc_value)).abbreviation)
  end

  # ------------------------------------------------------------------
  # Constant offset base zone
  # ------------------------------------------------------------------

  def test_constant_offset_base_zone
    const_info = DataSources::ConstantOffsetDataTimezoneInfo.new('Test/Const', STD_OFFSET)
    const = const_info.create_timezone

    view = const.with_corrections([
      correction(Time.utc(2020, 1).to_i, Time.utc(2020, 2).to_i, DST_OFFSET)
    ])

    assert_equal('STD', view.period_for(Timestamp.utc(Time.utc(2019).to_i)).abbreviation)
    assert_equal('DST', view.period_for(Timestamp.utc(Time.utc(2020, 1, 15).to_i)).abbreviation)
    assert_equal('STD', view.period_for(Timestamp.utc(Time.utc(2020, 3).to_i)).abbreviation)

    assert_equal(2, view.transitions_up_to(Timestamp.utc(Time.utc(2021).to_i),
                                           Timestamp.utc(Time.utc(2019).to_i)).length)
    assert_equal(['DST'], view.periods_for_local(Timestamp.create(2020, 1, 15, 12)).map(&:abbreviation))
  end

  # ------------------------------------------------------------------
  # Delegation
  # ------------------------------------------------------------------

  def test_delegates_other_timezone_methods
    zone = simple_zone('Test/Simple')
    view = zone.with_corrections([
      correction(Time.utc(2020, 6).to_i, Time.utc(2020, 7).to_i, STD_OFFSET)
    ])

    t = Time.utc(2020, 6, 15)
    assert_equal(-4 * 3600, view.observed_utc_offset(t))
    assert_equal('STD', view.abbreviation(t))
    refute(view.dst?(t))

    local = view.utc_to_local(t)
    assert_equal(-4 * 3600, local.utc_offset)
  end
end
