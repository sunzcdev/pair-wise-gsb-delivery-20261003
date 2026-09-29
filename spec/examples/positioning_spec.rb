require "spec_helper"

describe IceCube::Schedule do
  let(:start_time) { Time.utc(2024, 1, 15, 9, 30, 0) }

  def build_schedule(rule = nil, &block)
    IceCube::Schedule.new(start_time) do |schedule|
      schedule.add_recurrence_rule(rule) if rule
      block&.call(schedule)
    end
  end

  def enumerated(schedule, n)
    schedule.first(n).map(&:to_time)
  end

  describe "#occurrence_at" do
    context "with a single plain interval rule" do
      it "answers arithmetically without enumerating any occurrences" do
        schedule = build_schedule(IceCube::Rule.daily(3))
        result = schedule.occurrence_at(100_000)
        expect(result).to be_exact
        expect(result.strategy).to eq :arithmetic
        expect(result.iterations).to eq 0
        expect(result.time).to eq start_time + 100_000 * 3 * ONE_DAY
      end

      it "matches enumeration for the beginning of the sequence" do
        schedule = build_schedule(IceCube::Rule.daily(3))
        expected = enumerated(schedule, 25)
        (0...25).each do |index|
          expect(schedule.occurrence_at(index).time).to eq expected[index]
        end
      end

      it "matches enumeration for every frequency" do
        rules = [
          IceCube::Rule.secondly(7),
          IceCube::Rule.minutely(13),
          IceCube::Rule.hourly(5),
          IceCube::Rule.daily(2),
          IceCube::Rule.weekly(3, :tuesday),
          IceCube::Rule.monthly(2),
          IceCube::Rule.yearly(1)
        ]
        rules.each do |rule|
          schedule = build_schedule(rule)
          expected = enumerated(schedule, 12)
          (0...12).each do |index|
            result = schedule.occurrence_at(index)
            expect(result).to be_exact
            expect(result.time).to eq expected[index]
          end
        end
      end

      it "returns the occurrence at index 0 as the start time" do
        schedule = build_schedule(IceCube::Rule.hourly)
        expect(schedule.occurrence_at(0).time).to eq start_time
      end

      it "proves there is no linear traversal for very distant ordinals" do
        schedule = build_schedule(IceCube::Rule.minutely(7))
        result = schedule.occurrence_at(10_000_000)
        expect(result).to be_exact
        expect(result.iterations).to eq 0
        expect(result.time).to eq start_time + 10_000_000 * 7 * ONE_MINUTE
      end

      it "raises ArgumentError for a negative index" do
        schedule = build_schedule(IceCube::Rule.daily)
        expect { schedule.occurrence_at(-1) }.to raise_error(ArgumentError)
      end
    end

    context "with COUNT and UNTIL limits" do
      it "honors COUNT and returns nil beyond it" do
        schedule = build_schedule(IceCube::Rule.daily.count(5))
        expect(enumerated(schedule, 5)).to eq (0...5).map { |i| schedule.occurrence_at(i).time }
        expect(schedule.occurrence_at(4).time).to eq start_time + 4 * ONE_DAY
        expect(schedule.occurrence_at(5).time).to be_nil
        expect(schedule.occurrence_at(100_000).time).to be_nil
      end

      it "honors UNTIL and returns nil beyond it" do
        until_time = start_time + 30 * ONE_DAY
        schedule = build_schedule(IceCube::Rule.weekly(2).until(until_time))
        all = schedule.all_occurrences
        all.each_with_index do |time, index|
          expect(schedule.occurrence_at(index).time).to eq time
        end
        expect(schedule.occurrence_at(all.size).time).to be_nil
      end

      it "honors COUNT and UNTIL together, stopping at whichever comes first" do
        schedule = build_schedule(IceCube::Rule.daily.count(100).until(start_time + 9 * ONE_DAY))
        expect(schedule.all_occurrences.size).to eq 10
        expect(schedule.occurrence_at(9).time).to eq start_time + 9 * ONE_DAY
        expect(schedule.occurrence_at(10).time).to be_nil
      end
    end

    context "with combinations requiring the enumeration fallback" do
      it "reports the fallback strategy to the caller" do
        schedule = build_schedule(IceCube::Rule.daily.day(:monday, :wednesday))
        result = schedule.occurrence_at(3)
        expect(result).to be_fallback
        expect(result.strategy).to eq :enumeration
        expect(result.iterations).to eq 3
      end

      it "positions by the final deduplicated result of overlapping rules and exceptions" do
        schedule = build_schedule(IceCube::Rule.daily(2)) do |s|
          s.add_recurrence_rule IceCube::Rule.weekly
          s.add_exception_time(start_time + 4 * ONE_DAY)
          s.add_exception_time(start_time + 10 * ONE_DAY)
        end
        expected = enumerated(schedule, 30)
        (0...30).each do |index|
          result = schedule.occurrence_at(index)
          expect(result.strategy).to eq :enumeration
          expect(result.time).to eq expected[index]
        end
      end

      it "positions correctly when COUNT on one rule forces walking from the start" do
        schedule = build_schedule(IceCube::Rule.daily.count(10)) do |s|
          s.add_recurrence_rule IceCube::Rule.weekly(2)
        end
        expected = enumerated(schedule, 20)
        (0...20).each do |index|
          expect(schedule.occurrence_at(index).time).to eq expected[index]
        end
      end

      it "falls back for monthly rules clamped by short months" do
        schedule = IceCube::Schedule.new(Time.utc(2024, 1, 31, 12, 0, 0)) do |s|
          s.add_recurrence_rule IceCube::Rule.monthly
        end
        expected = enumerated(schedule, 13)
        (0...13).each do |index|
          result = schedule.occurrence_at(index)
          expect(result.strategy).to eq :enumeration
          expect(result.time).to eq expected[index]
        end
      end

      it "falls back when recurrence times are present" do
        schedule = build_schedule do |s|
          s.add_recurrence_time(start_time + 3 * ONE_DAY)
          s.add_recurrence_time(start_time + ONE_DAY)
        end
        expect(schedule.occurrence_at(0).time).to eq start_time
        expect(schedule.occurrence_at(1).time).to eq start_time + ONE_DAY
        expect(schedule.occurrence_at(2).time).to eq start_time + 3 * ONE_DAY
        expect(schedule.occurrence_at(3).time).to be_nil
      end

      it "raises BudgetExceeded when the budget is too small" do
        schedule = build_schedule(IceCube::Rule.daily.day(:monday))
        expect { schedule.occurrence_at(50, budget: 10) }.to raise_error(IceCube::BudgetExceeded)
        expect(schedule.occurrence_at(50, budget: 100).time).to eq enumerated(schedule, 51).last
      end
    end

    context "with no recurrence rules" do
      it "only has the implicit start time occurrence" do
        schedule = build_schedule
        expect(schedule.occurrence_at(0).time).to eq start_time
        expect(schedule.occurrence_at(0)).to be_exact
        expect(schedule.occurrence_at(1).time).to be_nil
      end
    end
  end

  describe "#occurrences_before" do
    context "with a single plain interval rule" do
      it "counts arithmetically without enumerating any occurrences" do
        schedule = build_schedule(IceCube::Rule.daily(2))
        result = schedule.occurrences_before(start_time + 100_000 * 2 * ONE_DAY)
        expect(result).to be_exact
        expect(result.iterations).to eq 0
        expect(result.count).to eq 100_000
      end

      it "is the exact inverse of #occurrence_at" do
        schedule = build_schedule(IceCube::Rule.hourly(6))
        [0, 1, 2, 41, 1_000].each do |index|
          time = schedule.occurrence_at(index).time
          expect(schedule.occurrences_before(time).count).to eq index
          expect(schedule.occurrences_before(time + 1).count).to eq index + 1
        end
      end

      it "matches enumeration for every frequency" do
        rules = [
          IceCube::Rule.secondly(7),
          IceCube::Rule.minutely(13),
          IceCube::Rule.hourly(5),
          IceCube::Rule.daily(2),
          IceCube::Rule.weekly(3, :tuesday),
          IceCube::Rule.monthly(2),
          IceCube::Rule.yearly(1)
        ]
        rules.each do |rule|
          schedule = build_schedule(rule)
          occurrences = enumerated(schedule, 12)
          occurrences.each_with_index do |time, index|
            expect(schedule.occurrences_before(time).count).to eq index
            expect(schedule.occurrences_before(time + 1).count).to eq index + 1
          end
        end
      end

      it "counts nothing at or before the start time" do
        schedule = build_schedule(IceCube::Rule.daily)
        expect(schedule.occurrences_before(start_time).count).to eq 0
        expect(schedule.occurrences_before(start_time - ONE_DAY).count).to eq 0
      end

      it "caps the count at COUNT" do
        schedule = build_schedule(IceCube::Rule.daily.count(5))
        expect(schedule.occurrences_before(start_time + 1_000 * ONE_DAY).count).to eq 5
        expect(schedule.occurrences_before(start_time + 3 * ONE_DAY).count).to eq 3
      end

      it "caps the count at UNTIL" do
        until_time = start_time + 10 * ONE_DAY
        schedule = build_schedule(IceCube::Rule.daily.until(until_time))
        expect(schedule.occurrences_before(start_time + 1_000 * ONE_DAY).count).to eq 11
        expect(schedule.occurrences_before(until_time).count).to eq 10
        expect(schedule.occurrences_before(until_time + 1).count).to eq 11
      end
    end

    context "with combinations requiring the enumeration fallback" do
      it "counts the final deduplicated result of overlapping rules and exceptions" do
        schedule = build_schedule(IceCube::Rule.daily(2)) do |s|
          s.add_recurrence_rule IceCube::Rule.weekly
          s.add_exception_time(start_time + 4 * ONE_DAY)
        end
        expected = enumerated(schedule, 30)
        expected.each_with_index do |time, index|
          result = schedule.occurrences_before(time)
          expect(result.strategy).to eq :enumeration
          expect(result.count).to eq index
        end
        expect(schedule.occurrences_before(expected.last + 1).count).to eq expected.size
      end

      it "raises BudgetExceeded when the budget is too small" do
        schedule = build_schedule(IceCube::Rule.daily.day(:monday))
        expect {
          schedule.occurrences_before(start_time + 1_000 * ONE_DAY, budget: 10)
        }.to raise_error(IceCube::BudgetExceeded)
      end
    end

    context "with no recurrence rules" do
      it "only counts the implicit start time occurrence" do
        schedule = build_schedule
        expect(schedule.occurrences_before(start_time).count).to eq 0
        expect(schedule.occurrences_before(start_time + 1).count).to eq 1
      end
    end

    it "requires a time" do
      schedule = build_schedule(IceCube::Rule.daily)
      expect { schedule.occurrences_before(nil) }.to raise_error(ArgumentError)
    end
  end
end
