require File.dirname(__FILE__) + "/../spec_helper"

module IceCube
  describe SubsecondlyRule do
    # A start time with nanosecond precision
    let(:start_time) { Time.utc(2026, 1, 1, 12, 0, 0) + Rational(123_456_789, 1_000_000_000) }

    def schedule_with(rule, start = start_time)
      Schedule.new(start) { |s| s.rrule rule }
    end

    describe "interval input" do
      it "accepts an Integer number of seconds" do
        rule = Rule.subsecondly(2)
        expect(rule.validations_for(:interval).first.interval).to eq Rational(2, 1)
      end

      it "accepts a Rational" do
        rule = Rule.subsecondly(Rational(1, 8))
        expect(rule.validations_for(:interval).first.interval).to eq Rational(1, 8)
      end

      it "accepts a BigDecimal" do
        rule = Rule.subsecondly(BigDecimal("0.25"))
        expect(rule.validations_for(:interval).first.interval).to eq Rational(1, 4)
      end

      it "accepts an exact decimal String" do
        rule = Rule.subsecondly("0.05")
        expect(rule.validations_for(:interval).first.interval).to eq Rational(1, 20)
      end

      it "raises an ArgumentError for a Float, which cannot be exact" do
        expect {
          Rule.subsecondly(0.1)
        }.to raise_error(ArgumentError, /Float.*cannot represent an exact/)
      end

      it "raises an ArgumentError for precision finer than one nanosecond" do
        expect {
          Rule.subsecondly(Rational(1, 10_000_000_000))
        }.to raise_error(ArgumentError, /whole number of nanoseconds/)
      end

      it "raises an ArgumentError for a third of a second" do
        expect {
          Rule.subsecondly(Rational(1, 3))
        }.to raise_error(ArgumentError, /whole number of nanoseconds/)
      end

      it "raises an ArgumentError for zero and negative intervals" do
        expect { Rule.subsecondly(0) }.to raise_error(ArgumentError, /positive/)
        expect { Rule.subsecondly(Rational(-1, 2)) }.to raise_error(ArgumentError, /positive/)
      end

      it "raises an ArgumentError for invalid input" do
        expect { Rule.subsecondly("invalid") }.to raise_error(ArgumentError, /not a valid input/)
        expect { Rule.subsecondly(nil) }.to raise_error(ArgumentError, /not a valid input/)
      end
    end

    describe "occurrences" do
      it "advances by exact fractions of a second from the start time" do
        schedule = schedule_with(Rule.subsecondly(Rational(1, 8)))
        expect(schedule.first(4)).to eq [
          start_time,
          start_time + Rational(1, 8),
          start_time + Rational(2, 8),
          start_time + Rational(3, 8)
        ]
      end

      it "keeps the nanoseconds of the start time on the occurrence grid" do
        schedule = schedule_with(Rule.subsecondly(Rational(1, 2)))
        expect(schedule.first(3).map(&:nsec)).to eq [123_456_789, 623_456_789, 123_456_789]
      end

      it "stops after COUNT occurrences" do
        schedule = schedule_with(Rule.subsecondly(Rational(1, 8)).count(3))
        expect(schedule.all_occurrences).to eq [
          start_time, start_time + Rational(1, 8), start_time + Rational(2, 8)
        ]
      end

      it "includes an occurrence falling exactly on the UNTIL boundary" do
        schedule = schedule_with(Rule.subsecondly(Rational(1, 8)).until(start_time + Rational(3, 8)))
        expect(schedule.all_occurrences.size).to eq 4
      end

      it "excludes occurrences one nanosecond past the UNTIL boundary" do
        boundary = start_time + Rational(3, 8) - Rational(1, 1_000_000_000)
        schedule = schedule_with(Rule.subsecondly(Rational(1, 8)).until(boundary))
        expect(schedule.all_occurrences.size).to eq 3
      end

      it "finds the next occurrence after a previous one" do
        schedule = schedule_with(Rule.subsecondly(Rational(1, 8)))
        expect(schedule.next_occurrence(start_time)).to eq start_time + Rational(1, 8)
      end

      it "finds the previous occurrence before a given time" do
        schedule = schedule_with(Rule.subsecondly(Rational(1, 8)))
        expect(schedule.previous_occurrence(start_time + 1)).to eq start_time + Rational(7, 8)
      end

      it "combines with whole-second rules without emitting off-grid times" do
        schedule = Schedule.new(start_time) do |s|
          s.rrule Rule.subsecondly(Rational(1, 4)).count(3)
          s.rrule Rule.daily.count(2)
        end
        expect(schedule.all_occurrences).to eq [
          start_time,
          start_time + Rational(1, 4),
          start_time + Rational(2, 4),
          start_time + ONE_DAY
        ]
      end

      it "advances 100,000 times with no drift from the exact arithmetic baseline", timeout: 30 do
        interval = Rational(1, 10) # 0.1 cannot be represented exactly by a Float
        schedule = schedule_with(Rule.subsecondly(interval))

        occurrences = schedule.all_occurrences_enumerator.take(100_000)
        baseline = Array.new(100_000) { |k| start_time + k * interval }

        expect(occurrences).to eq baseline
        expect(occurrences.last.to_r).to eq(start_time.to_r + 99_999 * interval)
      end
    end

    describe "serialization" do
      let(:rule) { Rule.subsecondly(Rational(1, 8)).count(4) }
      let(:schedule) { schedule_with(rule) }

      it "round-trips through a hash preserving the same sequence" do
        restored = Schedule.from_hash(schedule.to_hash)
        expect(restored.all_occurrences).to eq schedule.all_occurrences
      end

      it "round-trips through YAML preserving the same sequence" do
        restored = Schedule.from_yaml(schedule.to_yaml)
        expect(restored.all_occurrences).to eq schedule.all_occurrences
      end

      it "round-trips the rule itself through YAML" do
        restored = Rule.from_yaml(rule.to_yaml)
        expect(restored.to_hash).to eq rule.to_hash
        expect(schedule_with(restored).all_occurrences).to eq schedule.all_occurrences
      end

      it "keeps the exact interval in the hash representation" do
        expect(rule.to_hash[:interval]).to eq Rational(1, 8)
        expect(rule.to_hash[:rule_type]).to eq "IceCube::SubsecondlyRule"
      end
    end

    describe "existing rules" do
      it "keeps integer intervals working for whole-second rules" do
        expect(Rule.secondly(2).validations_for(:interval).first.interval).to eq 2
        expect(Rule.secondly("3").validations_for(:interval).first.interval).to eq 3
      end
    end

    describe "#to_s" do
      it "describes the interval as an exact decimal" do
        expect(Rule.subsecondly(Rational(1, 8)).to_s).to eq "Every 0.125 seconds"
        expect(Rule.subsecondly(2).to_s).to eq "Every 2 seconds"
      end
    end

    describe "#to_ical" do
      it "raises since RFC 5545 cannot represent sub-second frequencies" do
        expect {
          Rule.subsecondly(Rational(1, 8)).to_ical
        }.to raise_error(NotImplementedError, /cannot be represented in iCalendar/)
      end
    end
  end
end
