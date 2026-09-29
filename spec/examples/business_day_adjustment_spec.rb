require "spec_helper"

describe IceCube::Schedule, "business day adjustment" do
  # 2026-01-29 Thursday, 2026-01-30 Friday, 2026-01-31 Saturday
  # 2026-02-01 Sunday,  2026-02-02 Monday, 2026-02-03 Tuesday
  # 2026-02-28 Saturday, 2026-03-02 Monday
  let(:start_time) { Time.utc(2026, 1, 31, 9, 0, 0) }

  def weekend_predicate
    ->(time) { (1..5).cover?(time.wday) }
  end

  describe "#adjust_to_business_days" do
    it "requires a valid direction" do
      schedule = IceCube::Schedule.new(start_time)
      expect {
        schedule.adjust_to_business_days(direction: :sideways) { true }
      }.to raise_error(ArgumentError, /direction/)
    end

    it "requires a non-negative integer max_days" do
      schedule = IceCube::Schedule.new(start_time)
      expect {
        schedule.adjust_to_business_days(max_days: -1) { true }
      }.to raise_error(ArgumentError, /max_days/)
    end

    it "requires a predicate block" do
      schedule = IceCube::Schedule.new(start_time)
      expect {
        schedule.adjust_to_business_days
      }.to raise_error(ArgumentError, /predicate/)
    end

    it "returns the schedule for chaining and exposes the adjustment" do
      schedule = IceCube::Schedule.new(start_time)
      expect(schedule.adjust_to_business_days { true }).to eq schedule
      expect(schedule.business_day_adjustment).to be_a IceCube::BusinessDayAdjustment
    end
  end

  describe "month-end consecutive holidays" do
    let(:holidays) { [Date.new(2026, 1, 30), Date.new(2026, 2, 2)] }
    let(:predicate) do
      ->(time) { (1..5).cover?(time.wday) && !holidays.include?(Date.new(time.year, time.month, time.day)) }
    end

    it "shifts forward past a run of non-business days into the next month" do
      schedule = IceCube::Schedule.new(start_time) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
        s.adjust_to_business_days(direction: :forward, max_days: 3, &predicate)
      end
      # 2026-01-31 Sat, 02-01 Sun, 02-02 holiday -> 2026-02-03 Tue
      expect(schedule.first(1)).to eq [Time.utc(2026, 2, 3, 9, 0, 0)]
    end

    it "shifts backward past a run of non-business days within the month" do
      schedule = IceCube::Schedule.new(start_time) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
        s.adjust_to_business_days(direction: :backward, max_days: 3, &predicate)
      end
      # 2026-01-31 Sat, 01-30 holiday -> 2026-01-29 Thu
      expect(schedule.first(1)).to eq [Time.utc(2026, 1, 29, 9, 0, 0)]
    end

    it "leaves business day occurrences untouched" do
      schedule = IceCube::Schedule.new(Time.utc(2026, 1, 28, 9, 0, 0)) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(28)
        s.adjust_to_business_days(direction: :forward, max_days: 3, &predicate)
      end
      occurrence = schedule.first
      expect(occurrence).to eq Time.utc(2026, 1, 28, 9, 0, 0)
      expect(occurrence).not_to be_adjusted
      expect(occurrence.original_times).to eq [Time.utc(2026, 1, 28, 9, 0, 0)]
    end

    it "marks shifted occurrences with their raw source time" do
      schedule = IceCube::Schedule.new(start_time) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
        s.adjust_to_business_days(direction: :forward, max_days: 3, &predicate)
      end
      occurrence = schedule.first
      expect(occurrence).to be_adjusted
      expect(occurrence.original_times).to eq [Time.utc(2026, 1, 31, 9, 0, 0)]
    end
  end

  describe "candidate collisions" do
    let(:predicate) do
      # 2026-01-30 (Friday) is a holiday, weekends are non-business
      ->(time) { (1..5).cover?(time.wday) && time.yday != 30 }
    end

    it "merges two raw occurrences shifted onto the same day" do
      schedule = IceCube::Schedule.new(Time.utc(2026, 1, 30, 9, 0, 0)) do |s|
        s.add_recurrence_time Time.utc(2026, 1, 31, 10, 0, 0)
        s.adjust_to_business_days(direction: :forward, max_days: 3, &predicate)
      end
      occurrences = schedule.occurrences(Time.utc(2026, 3, 1))
      expect(occurrences.length).to eq 1
      merged = occurrences.first
      # keeps the earliest adjusted time-of-day
      expect(merged).to eq Time.utc(2026, 2, 2, 9, 0, 0)
      # and reports every raw source in chronological order
      expect(merged.original_times).to eq [
        Time.utc(2026, 1, 30, 9, 0, 0),
        Time.utc(2026, 1, 31, 10, 0, 0)
      ]
      expect(merged).to be_adjusted
    end

    it "emits merged results in adjusted-time order" do
      schedule = IceCube::Schedule.new(Time.utc(2026, 1, 30, 9, 0, 0)) do |s|
        s.add_recurrence_time Time.utc(2026, 1, 31, 10, 0, 0)
        s.add_recurrence_time Time.utc(2026, 2, 4, 8, 0, 0)
        s.adjust_to_business_days(direction: :forward, max_days: 3, &predicate)
      end
      occurrences = schedule.occurrences(Time.utc(2026, 3, 1))
      expect(occurrences).to eq [
        Time.utc(2026, 2, 2, 9, 0, 0),
        Time.utc(2026, 2, 4, 8, 0, 0)
      ]
    end
  end

  describe "interaction with count, until and exceptions" do
    it "applies count to raw occurrences before adjustment" do
      schedule = IceCube::Schedule.new(start_time) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(-1).count(2)
        s.adjust_to_business_days(direction: :forward, max_days: 3, &weekend_predicate)
      end
      # raw: 2026-01-31 (Sat), 2026-02-28 (Sat, clamped from 31st)
      expect(schedule.all_occurrences).to eq [
        Time.utc(2026, 2, 2, 9, 0, 0),
        Time.utc(2026, 3, 2, 9, 0, 0)
      ]
    end

    it "applies until to raw occurrences, so results may pass it by max_days" do
      schedule = IceCube::Schedule.new(start_time) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(-1).until(Time.utc(2026, 3, 1))
        s.adjust_to_business_days(direction: :forward, max_days: 3, &weekend_predicate)
      end
      # raw 2026-02-28 is before until, but adjusts to 2026-03-02
      expect(schedule.all_occurrences).to eq [
        Time.utc(2026, 2, 2, 9, 0, 0),
        Time.utc(2026, 3, 2, 9, 0, 0)
      ]
    end

    it "removes excepted raw occurrences without adjusting them" do
      schedule = IceCube::Schedule.new(start_time) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(-1).count(2)
        s.add_exception_time Time.utc(2026, 1, 31, 9, 0, 0)
        s.adjust_to_business_days(direction: :forward, max_days: 3, &weekend_predicate)
      end
      expect(schedule.all_occurrences).to eq [Time.utc(2026, 3, 2, 9, 0, 0)]
    end

    it "does not match exceptions against adjusted dates" do
      schedule = IceCube::Schedule.new(start_time) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(-1).count(2)
        s.add_exception_time Time.utc(2026, 2, 2, 9, 0, 0) # the adjusted date
        s.adjust_to_business_days(direction: :forward, max_days: 3, &weekend_predicate)
      end
      expect(schedule.all_occurrences).to eq [
        Time.utc(2026, 2, 2, 9, 0, 0),
        Time.utc(2026, 3, 2, 9, 0, 0)
      ]
    end
  end

  describe "when no business day exists within max_days" do
    it "raises a BusinessDayAdjustmentError identifying the failure" do
      schedule = IceCube::Schedule.new(start_time) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
        s.adjust_to_business_days(direction: :forward, max_days: 2) { false }
      end
      expect { schedule.first }.to raise_error(IceCube::BusinessDayAdjustmentError) do |error|
        expect(error.occurrence_time).to eq start_time
        expect(error.direction).to eq :forward
        expect(error.max_days).to eq 2
        expect(error.message).to include("No business day found within 2 day(s)")
      end
    end

    it "raises when max_days is zero and the raw date is not a business day" do
      schedule = IceCube::Schedule.new(start_time) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
        s.adjust_to_business_days(direction: :backward, max_days: 0, &weekend_predicate)
      end
      expect { schedule.first }.to raise_error(IceCube::BusinessDayAdjustmentError)
    end
  end

  describe "time zone handling" do
    it "keeps occurrences in the schedule's own zone without touching the system zone" do
      schedule = IceCube::Schedule.new(start_time) do |s|
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(31)
        s.adjust_to_business_days(direction: :forward, max_days: 3, &weekend_predicate)
      end
      occurrence = schedule.first
      expect(occurrence.utc_offset).to eq 0
      expect(occurrence.hour).to eq 9
    end
  end
end
