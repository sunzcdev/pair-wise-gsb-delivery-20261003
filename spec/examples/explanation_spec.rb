require "spec_helper"

describe IceCube::Schedule, "#explain" do
  let(:monday) { Time.utc(2026, 1, 5, 9, 0, 0) }

  describe "a date that is both included and excluded" do
    let(:schedule) do
      IceCube::Schedule.new(monday) do |s|
        s.add_recurrence_rule IceCube::Rule.weekly.day(:monday)
        s.add_exception_rule IceCube::Rule.weekly.day(:monday)
      end
    end

    let(:other_monday) { Time.utc(2026, 2, 2, 9, 0, 0) }
    let(:explanation) { schedule.explain(other_monday) }

    it "reports the exclusion as the reason" do
      expect(explanation[:occurs]).to be false
      expect(explanation[:included]).to be true
      expect(explanation[:excluded]).to be true
      expect(explanation[:reasons]).to eq [:excluded_by_exception]
    end

    it "identifies the matching recurrence and exception rules" do
      expect(explanation[:recurrence].map { |entry| entry[:matched] }).to eq [true]
      expect(explanation[:exceptions].map { |entry| entry[:matched] }).to eq [true]
    end

    it "agrees with occurs_at?" do
      expect(explanation[:occurs]).to eq schedule.occurs_at?(other_monday)
    end
  end

  describe "a date just past the count limit" do
    let(:start_time) { Time.utc(2026, 1, 1, 9, 0, 0) }
    let(:schedule) do
      IceCube::Schedule.new(start_time) do |s|
        s.add_recurrence_rule IceCube::Rule.daily.count(3)
      end
    end

    let(:past_the_limit) { Time.utc(2026, 1, 4, 9, 0, 0) } # would be the 4th
    let(:explanation) { schedule.explain(past_the_limit) }

    it "reports the count limit as the reason" do
      expect(explanation[:occurs]).to be false
      expect(explanation[:included]).to be false
      expect(explanation[:reasons]).to include(:count_exceeded)
    end

    it "reports count usage against the limit" do
      entry = explanation[:recurrence].first
      expect(entry[:matched]).to be false
      expect(entry[:blocked_by]).to eq [:count]
      expect(entry[:limits][:count]).to eq({max: 3, used: 3, exceeded: true})
    end

    it "still reports the last allowed occurrence as occurring" do
      last_allowed = schedule.explain(Time.utc(2026, 1, 3, 9, 0, 0))
      expect(last_allowed[:occurs]).to be true
      expect(last_allowed[:recurrence].first[:limits][:count]).to eq({max: 3, used: 2, exceeded: false})
    end

    it "agrees with occurs_at?" do
      expect(explanation[:occurs]).to eq schedule.occurs_at?(past_the_limit)
    end
  end

  describe "composite rule sub-conditions" do
    let(:schedule) do
      IceCube::Schedule.new(monday) do |s|
        s.add_recurrence_rule IceCube::Rule.weekly.day(:monday).hour_of_day(9)
      end
    end

    it "locates the rejecting sub-condition" do
      tuesday_at_nine = Time.utc(2026, 1, 6, 9, 0, 0)
      entry = schedule.explain(tuesday_at_nine)[:recurrence].first

      expect(entry[:matched]).to be false
      expect(entry[:blocked_by]).to include(:conditions)

      day_condition = entry[:conditions].find { |c| c[:key] == :day }
      expect(day_condition[:matched]).to be false
      expect(day_condition[:value]).to eq 1 # Monday

      hour_condition = entry[:conditions].find { |c| c[:key] == :hour_of_day }
      expect(hour_condition[:matched]).to be true
    end
  end

  describe "limits" do
    it "reports UNTIL when the time is past it" do
      schedule = IceCube::Schedule.new(monday) do |s|
        s.add_recurrence_rule IceCube::Rule.daily.until(Time.utc(2026, 1, 10, 9, 0, 0))
      end
      explanation = schedule.explain(Time.utc(2026, 1, 11, 9, 0, 0))
      entry = explanation[:recurrence].first

      expect(explanation[:occurs]).to be false
      expect(explanation[:reasons]).to include(:until_exceeded)
      expect(entry[:blocked_by]).to include(:until)
      expect(entry[:limits][:until][:exceeded]).to be true
    end

    it "reports the start_time limit for times before the schedule starts" do
      schedule = IceCube::Schedule.new(monday) do |s|
        s.add_recurrence_rule IceCube::Rule.daily
      end
      explanation = schedule.explain(monday - ONE_DAY)

      expect(explanation[:occurs]).to be false
      expect(explanation[:reasons]).to eq [:before_start_time, :no_matching_recurrence]
    end
  end

  describe "read-only guarantees" do
    it "does not change subsequent enumeration results" do
      schedule = IceCube::Schedule.new(monday) do |s|
        s.add_recurrence_rule IceCube::Rule.daily.count(3)
      end
      before = schedule.first(3)
      schedule.explain(monday + 3 * ONE_DAY)
      schedule.explain(monday)
      expect(schedule.first(3)).to eq before
    end

    it "does not consume count uses" do
      rule = IceCube::Rule.daily.count(2)
      schedule = IceCube::Schedule.new(monday) { |s| s.add_recurrence_rule rule }
      schedule.explain(monday + ONE_DAY)
      expect(rule.uses).to eq 0
    end

    it "explains non-terminating rules without enumerating forever" do
      schedule = IceCube::Schedule.new(monday) do |s|
        s.add_recurrence_rule IceCube::Rule.weekly.day(:monday)
      end
      explanation = schedule.explain(Time.utc(2036, 1, 8, 9, 0, 0)) # a Tuesday
      expect(explanation[:occurs]).to be false
      expect(explanation[:recurrence].first[:limits]).to eq({})
    end
  end

  describe "single occurrence times" do
    it "explains recurrence and exception times" do
      schedule = IceCube::Schedule.new(monday) do |s|
        s.add_recurrence_time monday + ONE_DAY
        s.add_exception_time monday + ONE_DAY
      end
      explanation = schedule.explain(monday + ONE_DAY)

      expect(explanation[:occurs]).to be false
      expect(explanation[:reasons]).to eq [:excluded_by_exception]
      expect(explanation[:recurrence].first[:kind]).to eq :single_occurrence
      expect(explanation[:exceptions].first[:kind]).to eq :single_occurrence
    end
  end
end
