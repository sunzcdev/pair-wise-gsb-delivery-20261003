require File.dirname(__FILE__) + "/../spec_helper"

describe IceCube::Schedule, "#explain" do
  # 2026-01-05 is a Monday
  let(:start_time) { Time.utc(2026, 1, 5, 9, 0, 0) }

  it "reports a matching occurrence with no reasons" do
    schedule = IceCube::Schedule.new(start_time) do |s|
      s.add_recurrence_rule IceCube::Rule.daily
    end
    report = schedule.explain(start_time + 3 * ONE_DAY)
    expect(report[:occurs]).to be true
    expect(report[:reasons]).to be_empty
    expect(report[:recurrence][:matched]).to be true
    expect(report[:exceptions][:matched]).to be false
  end

  it "explains a date that is both included and excluded" do
    schedule = IceCube::Schedule.new(start_time) do |s|
      s.add_recurrence_rule IceCube::Rule.daily
      s.add_exception_rule IceCube::Rule.weekly.day(:monday)
    end
    monday = start_time + 7 * ONE_DAY
    report = schedule.explain(monday)

    expect(report[:occurs]).to be false
    expect(report[:reasons]).to eq [:excluded_by_exception]
    expect(report[:recurrence][:matched]).to be true
    expect(report[:recurrence][:rules].first[:matched]).to be true
    expect(report[:exceptions][:matched]).to be true
    expect(report[:exceptions][:rules].first[:matched]).to be true
  end

  it "explains a date just past the COUNT limit" do
    schedule = IceCube::Schedule.new(start_time) do |s|
      s.add_recurrence_rule IceCube::Rule.daily.count(3)
    end
    time = start_time + 3 * ONE_DAY # would have been the 4th occurrence
    report = schedule.explain(time)

    expect(report[:occurs]).to be false
    expect(report[:reasons]).to eq [:no_recurrence_match]

    rule_report = report[:recurrence][:rules].first
    expect(rule_report[:matched]).to be false
    expect(rule_report[:blockers]).to eq [:count_exceeded]
    expect(rule_report[:limits][:count]).to eq(limit: 3, used: 3, remaining: 0, exceeded: true)
    # the pattern itself would still generate this time
    expect(rule_report[:conditions].all? { |c| c[:matched] }).to be true
  end

  it "reports COUNT usage before the limit is reached" do
    schedule = IceCube::Schedule.new(start_time) do |s|
      s.add_recurrence_rule IceCube::Rule.daily.count(3)
    end
    report = schedule.explain(start_time + 2 * ONE_DAY)

    expect(report[:occurs]).to be true
    count = report[:recurrence][:rules].first[:limits][:count]
    expect(count).to eq(limit: 3, used: 2, remaining: 1, exceeded: false)
  end

  it "locates the failing sub-conditions of a combined rule" do
    schedule = IceCube::Schedule.new(start_time) do |s|
      s.add_recurrence_rule IceCube::Rule.weekly.day(:monday, :wednesday).hour_of_day(10)
    end
    tuesday_at_ten = Time.utc(2026, 1, 6, 10, 0, 0)
    report = schedule.explain(tuesday_at_ten)

    expect(report[:occurs]).to be false
    rule_report = report[:recurrence][:rules].first
    expect(rule_report[:blockers]).to include :condition_mismatch

    day_conditions = rule_report[:conditions].select { |c| c[:name] == :day }
    expect(day_conditions.map { |c| c[:expected] }).to contain_exactly 1, 3
    expect(day_conditions.none? { |c| c[:matched] }).to be true
    expect(day_conditions.first[:actual]).to eq 2 # Tuesday

    hour_condition = rule_report[:conditions].find { |c| c[:name] == :hour_of_day }
    expect(hour_condition[:matched]).to be true
    expect(hour_condition[:expected]).to eq 10
  end

  it "explains a time before the schedule start" do
    schedule = IceCube::Schedule.new(start_time) do |s|
      s.add_recurrence_rule IceCube::Rule.daily
    end
    report = schedule.explain(start_time - ONE_DAY)

    expect(report[:occurs]).to be false
    expect(report[:reasons]).to include :before_start_time
    expect(report[:recurrence][:rules].first[:blockers]).to include :before_start_time
  end

  it "explains a date past the UNTIL limit" do
    schedule = IceCube::Schedule.new(start_time) do |s|
      s.add_recurrence_rule IceCube::Rule.daily.until(start_time + 2 * ONE_DAY)
    end
    report = schedule.explain(start_time + 5 * ONE_DAY)

    expect(report[:occurs]).to be false
    rule_report = report[:recurrence][:rules].first
    expect(rule_report[:blockers]).to include :until_exceeded
    expect(rule_report[:limits][:until][:exceeded]).to be true
    expect(rule_report[:limits][:until][:time]).to eq start_time + 2 * ONE_DAY
  end

  it "matches single recurrence and exception times" do
    included = start_time + 10 * ONE_DAY
    schedule = IceCube::Schedule.new(start_time) do |s|
      s.add_recurrence_time included
      s.add_exception_time start_time
    end

    report = schedule.explain(included)
    expect(report[:occurs]).to be true
    expect(report[:recurrence][:times].first[:matched]).to be true

    report = schedule.explain(start_time)
    expect(report[:occurs]).to be false
    expect(report[:exceptions][:times].first[:matched]).to be true
  end

  it "treats the start time as an implicit occurrence when no rules exist" do
    schedule = IceCube::Schedule.new(start_time)
    report = schedule.explain(start_time)
    expect(report[:occurs]).to be true
    expect(report[:recurrence][:implicit_start][:matched]).to be true
  end

  it "does not change subsequent enumeration results" do
    schedule = IceCube::Schedule.new(start_time) do |s|
      s.add_recurrence_rule IceCube::Rule.daily.count(3)
      s.add_exception_time start_time + ONE_DAY
    end
    before = schedule.first(2)
    schedule.explain(start_time + 3 * ONE_DAY)
    schedule.explain(start_time)
    expect(schedule.first(2)).to eq before
    expect(schedule.next_occurrence(start_time - ONE_DAY)).to eq before.first
  end

  it "agrees with #occurs_at? across a range of times" do
    schedule = IceCube::Schedule.new(start_time) do |s|
      s.add_recurrence_rule IceCube::Rule.weekly.day(:monday, :friday).count(6)
      s.add_exception_time start_time + 7 * ONE_DAY
    end
    (0...30).each do |i|
      time = start_time + i * ONE_DAY
      expect(schedule.explain(time)[:occurs]).to eq schedule.occurs_at?(time)
    end
  end

  it "raises ArgumentError without a time" do
    schedule = IceCube::Schedule.new(start_time)
    expect { schedule.explain(nil) }.to raise_error ArgumentError
  end
end
