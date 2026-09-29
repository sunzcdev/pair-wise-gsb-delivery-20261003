require File.dirname(__FILE__) + "/../spec_helper"

module IceCube
  describe SubsecondlyRule do
    # A start time with nanoseconds that must survive on every occurrence
    let(:start_time) { Time.utc(2026, 1, 1, 0, 0, 0) + Rational(123_456_789, 1_000_000_000) }

    def schedule_with(rule)
      Schedule.new(start_time) { |s| s.add_recurrence_rule rule }
    end

    describe "interval input" do
      it "accepts a Rational interval" do
        rule = Rule.subsecondly(Rational(1, 8))
        expect(rule.validations_for(:interval).first.interval).to eq Rational(1, 8)
      end

      it "accepts an exact decimal String interval" do
        rule = Rule.subsecondly("0.25")
        expect(rule.validations_for(:interval).first.interval).to eq Rational(1, 4)
      end

      it "accepts a BigDecimal interval" do
        require "bigdecimal"
        rule = Rule.subsecondly(BigDecimal("0.5"))
        expect(rule.validations_for(:interval).first.interval).to eq Rational(1, 2)
      end

      it "keeps accepting whole-second Integer intervals" do
        rule = Rule.subsecondly(2)
        expect(rule.validations_for(:interval).first.interval).to eq 2
        schedule = schedule_with(rule)
        expect(schedule.first(2)).to eq [start_time, start_time + 2]
      end

      it "raises an ArgumentError for inexact Float intervals" do
        expect {
          Rule.subsecondly(0.1)
        }.to raise_error(ArgumentError, /cannot be represented exactly/)
      end

      it "raises an ArgumentError for precision finer than nanoseconds" do
        expect {
          Rule.subsecondly(Rational(1, 10**10))
        }.to raise_error(ArgumentError, /finer than nanosecond precision/)
      end

      it "raises an ArgumentError for zero or negative intervals" do
        expect { Rule.subsecondly(0) }.to raise_error(ArgumentError, /not a valid input/)
        expect { Rule.subsecondly(Rational(-1, 2)) }.to raise_error(ArgumentError, /not a valid input/)
      end

      it "raises an ArgumentError for invalid input" do
        expect {
          Rule.subsecondly("invalid")
        }.to raise_error(ArgumentError, /not a valid input/)
      end
    end

    describe "occurrences" do
      it "lands on exact fractions of a second, keeping the start time's nanoseconds" do
        schedule = schedule_with(Rule.subsecondly(Rational(1, 8)))
        expected = (0...5).map { |k| start_time + k * Rational(1, 8) }
        expect(schedule.first(5)).to eq expected
        expect(schedule.first(5).map(&:nsec)).to all(satisfy { |n| (n - 123_456_789) % 125_000_000 == 0 })
      end

      it "does not drift over 100,000 advancements compared to exact arithmetic", timeout: 30 do
        interval = Rational(1, 8)
        schedule = schedule_with(Rule.subsecondly(interval))
        occurrences = schedule.first(100_000)
        occurrences.each_with_index do |time, k|
          expect(time).to eq start_time + k * interval
        end
        expect(occurrences.last.to_r).to eq start_time.to_r + 99_999 * interval
      end

      it "stops after COUNT occurrences" do
        schedule = schedule_with(Rule.subsecondly(Rational(1, 4)).count(3))
        expect(schedule.all_occurrences).to eq [
          start_time, start_time + Rational(1, 4), start_time + Rational(2, 4)
        ]
      end

      it "includes an until boundary that falls exactly on an occurrence" do
        schedule = schedule_with(Rule.subsecondly(Rational(1, 4)).until(start_time + 1))
        expect(schedule.all_occurrences.size).to eq 5
        expect(schedule.all_occurrences.last).to eq start_time + 1
      end

      it "excludes occurrences past an until boundary between grid points" do
        schedule = schedule_with(Rule.subsecondly(Rational(1, 4)).until(start_time + Rational(9, 10)))
        expect(schedule.all_occurrences.size).to eq 4
      end

      it "finds the next occurrence within the same second" do
        schedule = schedule_with(Rule.subsecondly(Rational(1, 8)))
        from = start_time + Rational(1, 8)
        expect(schedule.next_occurrence(from)).to eq start_time + Rational(2, 8)
      end

      it "checks occurrence at an exact sub-second instant" do
        schedule = schedule_with(Rule.subsecondly(Rational(1, 8)))
        expect(schedule.occurs_at?(start_time + Rational(3, 8))).to be true
        expect(schedule.occurs_at?(start_time + Rational(3, 8) + Rational(1, 100))).to be false
      end
    end

    describe "serialization" do
      let(:rule) { Rule.subsecondly(Rational(1, 8)).count(10) }
      let(:schedule) { schedule_with(rule) }

      it "keeps the same sequence after a rule YAML round trip" do
        restored = Rule.from_yaml(rule.to_yaml)
        expect(restored).to eq rule
        expect(schedule_with(restored).first(5)).to eq schedule.first(5)
      end

      it "keeps the same sequence after a schedule YAML round trip" do
        restored = Schedule.from_yaml(schedule.to_yaml)
        expect(restored.first(10)).to eq schedule.first(10)
      end

      it "keeps the same sequence after a hash round trip" do
        restored = Schedule.from_hash(schedule.to_hash)
        expect(restored.first(10)).to eq schedule.first(10)
      end

      it "serializes the interval exactly in the hash representation" do
        expect(rule.to_hash[:interval]).to eq Rational(1, 8)
      end

      it "raises a clear error when serialized to iCalendar" do
        expect {
          rule.to_ical
        }.to raise_error(ArgumentError, /cannot be represented in iCalendar/)
      end
    end
  end
end
