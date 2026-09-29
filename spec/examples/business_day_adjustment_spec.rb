require "spec_helper"

describe IceCube::BusinessDayAdjustment do
  # Weekdays are business days; caller-supplied holidays are not.
  def business_days(holidays = [])
    ->(date) { (1..5).cover?(date.wday) && !holidays.include?(date) }
  end

  def schedule_with(start_time, direction:, max_days: 7, holidays: [], &block)
    IceCube::Schedule.new(start_time) do |s|
      block.call(s) if block
      s.adjust_to_business_days(direction: direction, max_days: max_days, &business_days(holidays))
    end
  end

  describe "configuration validation" do
    it "requires a known direction" do
      expect {
        IceCube::BusinessDayAdjustment.new(direction: :sideways) { true }
      }.to raise_error(ArgumentError, /direction/)
    end

    it "requires a positive integer max_days" do
      expect {
        IceCube::BusinessDayAdjustment.new(max_days: 0) { true }
      }.to raise_error(ArgumentError, /max_days/)
    end

    it "requires a business day callback" do
      expect {
        IceCube::BusinessDayAdjustment.new
      }.to raise_error(ArgumentError, /block/)
    end

    it "rejects non-adjustment values on the schedule" do
      expect {
        IceCube::Schedule.new.business_day_adjustment = :nope
      }.to raise_error(ArgumentError, /BusinessDayAdjustment/)
    end

    it "accepts the adjustment as a constructor option" do
      adjustment = IceCube::BusinessDayAdjustment.new { true }
      schedule = IceCube::Schedule.new(Time.utc(2026, 1, 1), business_day_adjustment: adjustment)
      expect(schedule.business_day_adjustment).to eq adjustment
    end
  end

  describe "shifting" do
    it "moves a weekend occurrence forward to Monday, preserving time of day" do
      # 2026-01-31 is a Saturday
      schedule = schedule_with(Time.utc(2026, 1, 31, 9, 30), direction: :forward) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
      end
      first = schedule.first
      expect(first).to eq Time.utc(2026, 2, 2, 9, 30)
    end

    it "moves a weekend occurrence backward to Friday" do
      schedule = schedule_with(Time.utc(2026, 1, 31, 9, 30), direction: :backward) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
      end
      expect(schedule.first).to eq Time.utc(2026, 1, 30, 9, 30)
    end

    it "leaves occurrences that already fall on a business day untouched" do
      # 2026-03-31 is a Tuesday
      schedule = schedule_with(Time.utc(2026, 3, 31, 12, 0), direction: :forward) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
      end
      expect(schedule.first).to eq Time.utc(2026, 3, 31, 12, 0)
    end
  end

  describe "month-end consecutive holidays" do
    # 2026-01-31 (Sat) followed by holidays on Feb 2-4
    let(:holidays) { [Date.new(2026, 2, 2), Date.new(2026, 2, 3), Date.new(2026, 2, 4)] }

    it "shifts forward across the month boundary past the holiday run" do
      schedule = schedule_with(Time.utc(2026, 1, 31, 10, 0), direction: :forward, holidays: holidays) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
      end
      expect(schedule.first).to eq Time.utc(2026, 2, 5, 10, 0)
    end

    it "shifts backward to the last business day of the month" do
      schedule = schedule_with(Time.utc(2026, 1, 31, 10, 0), direction: :backward, holidays: holidays) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
      end
      expect(schedule.first).to eq Time.utc(2026, 1, 30, 10, 0)
    end

    it "raises a recognizable error when no business day fits the budget" do
      schedule = schedule_with(Time.utc(2026, 1, 31, 10, 0), direction: :forward, max_days: 3, holidays: holidays) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
      end
      expect { schedule.first }.to raise_error(IceCube::BusinessDayAdjustmentError) do |error|
        expect(error.occurrence_time).to eq Time.utc(2026, 1, 31, 10, 0)
        expect(error.direction).to eq :forward
        expect(error.max_days).to eq 3
      end
    end
  end

  describe "collisions" do
    # 2026-02-28 (Sat) and 2026-03-01 (Sun) both move to Monday 2026-03-02
    let(:schedule) do
      schedule_with(Time.utc(2026, 2, 28, 9, 0), direction: :forward) do |s|
        s.add_recurrence_time Time.utc(2026, 3, 1, 9, 0)
      end
    end

    it "merges raw occurrences that move to the same day into one" do
      occurrences = schedule.occurrences(Time.utc(2026, 3, 31))
      expect(occurrences).to eq [Time.utc(2026, 3, 2, 9, 0)]
    end

    it "records the provenance of every merged raw occurrence" do
      entries = schedule.occurrences_with_provenance(Time.utc(2026, 2, 1), Time.utc(2026, 3, 31))
      expect(entries.size).to eq 1
      entry = entries.first
      expect(entry).to be_merged
      expect(entry.start_time).to eq Time.utc(2026, 3, 2, 9, 0)
      expect(entry.sources).to eq [Time.utc(2026, 2, 28, 9, 0), Time.utc(2026, 3, 1, 9, 0)]
    end

    it "reports single-source provenance for unmerged occurrences" do
      plain = IceCube::Schedule.new(Time.utc(2026, 3, 2, 9, 0))
      entries = plain.occurrences_with_provenance(Time.utc(2026, 3, 1), Time.utc(2026, 3, 31))
      expect(entries.size).to eq 1
      expect(entries.first).not_to be_merged
      expect(entries.first.sources).to eq [Time.utc(2026, 3, 2, 9, 0)]
    end
  end

  describe "interaction with count, until and exceptions" do
    it "applies count to raw occurrences, so merges can reduce the emitted total" do
      # Raw: Feb 28 (Sat) -> Mar 2, Mar 1 (Sun) -> Mar 2 (merged), Mar 28 (Sat) -> Mar 30
      schedule = schedule_with(Time.utc(2026, 2, 15, 9, 0), direction: :forward) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(28, 1).count(3)
      end
      expect(schedule.all_occurrences).to eq [Time.utc(2026, 3, 2, 9, 0), Time.utc(2026, 3, 30, 9, 0)]
    end

    it "applies until to the raw occurrence time, before adjustment" do
      # Raw 2026-01-31 (Sat) is before the until boundary but shifts past it
      schedule = schedule_with(Time.utc(2026, 1, 31, 9, 0), direction: :forward) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31).until(Time.utc(2026, 2, 1))
      end
      expect(schedule.all_occurrences).to eq [Time.utc(2026, 2, 2, 9, 0)]
    end

    it "applies exception times to the raw occurrence time" do
      schedule = schedule_with(Time.utc(2026, 2, 28, 9, 0), direction: :forward) do |s|
        s.add_recurrence_time Time.utc(2026, 2, 28, 9, 0)
        s.add_exception_time Time.utc(2026, 2, 28, 9, 0)
      end
      expect(schedule.occurrences(Time.utc(2026, 3, 31))).to eq []
    end

    it "does not match exception times against the adjusted time" do
      schedule = schedule_with(Time.utc(2026, 2, 28, 9, 0), direction: :forward) do |s|
        s.add_recurrence_time Time.utc(2026, 2, 28, 9, 0)
        s.add_exception_time Time.utc(2026, 3, 2, 9, 0) # the adjusted time
      end
      expect(schedule.occurrences(Time.utc(2026, 3, 31))).to eq [Time.utc(2026, 3, 2, 9, 0)]
    end
  end

  describe "enumeration windows" do
    it "includes occurrences that shift into the window from before it" do
      schedule = schedule_with(Time.utc(2026, 1, 31, 9, 0), direction: :forward) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
      end
      expect(schedule.occurrences_between(Time.utc(2026, 2, 1), Time.utc(2026, 2, 28)))
        .to eq [Time.utc(2026, 2, 2, 9, 0)]
    end

    it "includes occurrences that shift backward into the window from after it" do
      # 2026-03-01 (Sun) shifts back to Friday 2026-02-27
      schedule = schedule_with(Time.utc(2026, 3, 1, 9, 0), direction: :backward) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(1)
      end
      expect(schedule.occurrences_between(Time.utc(2026, 2, 1), Time.utc(2026, 2, 28)))
        .to eq [Time.utc(2026, 2, 27, 9, 0)]
    end

    it "excludes occurrences that shift forward past the closing time" do
      schedule = schedule_with(Time.utc(2026, 1, 31, 9, 0), direction: :forward) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
      end
      expect(schedule.occurrences(Time.utc(2026, 2, 1))).to eq []
    end
  end

  describe "system time zone independence" do
    it "produces identical results regardless of the system zone", system_time_zone: "Pacific/Auckland" do
      schedule = schedule_with(Time.utc(2026, 1, 31, 9, 30), direction: :forward) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
      end
      expect(schedule.first).to eq Time.utc(2026, 2, 2, 9, 30)
    end
  end
end
