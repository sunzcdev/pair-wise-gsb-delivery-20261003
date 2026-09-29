require "spec_helper"

module IceCube
  describe ScheduleComparison do
    let(:t0) { Time.utc(2026, 1, 5, 9, 0, 0) } # a Monday

    def build_schedule(rule = nil, start: t0, &block)
      Schedule.new(start) do |schedule|
        schedule.add_recurrence_rule rule if rule
        block&.call(schedule)
      end
    end

    def compare(left, right, closing, **options)
      described_class.compare(left, right, t0, closing, **options)
    end

    describe ".compare" do
      it "confirms identical schedules are equal within the window" do
        left = build_schedule(Rule.daily)
        right = build_schedule(Rule.daily)

        result = compare(left, right, t0 + 30 * ONE_DAY)

        expect(result.status).to eq :equal
        expect(result).to be_equal
        expect(result.time).to be_nil
        expect(result.side).to be_nil
      end

      it "treats differently written rules with the same occurrences as equal" do
        left = build_schedule(Rule.weekly.day(:monday, :wednesday))
        right = build_schedule(Rule.daily.day(:monday, :wednesday))
        expect(left.to_hash).not_to eq right.to_hash
        expect(left.to_s).not_to eq right.to_s

        result = compare(left, right, t0 + 2 * 365 * ONE_DAY)

        expect(result).to be_equal
      end

      it "treats differently factored intervals with the same occurrences as equal" do
        left = build_schedule(Rule.daily(2))
        right = build_schedule(Rule.hourly(48))

        result = compare(left, right, t0 + 90 * ONE_DAY)

        expect(result).to be_equal
      end

      it "confirms schedules with no rules are equal when start times match" do
        result = compare(Schedule.new(t0), Schedule.new(t0), t0 + ONE_DAY)

        expect(result).to be_equal
      end

      it "reports the earliest difference owned by the left schedule" do
        left = build_schedule(Rule.daily)
        right = build_schedule(Rule.daily) do |s|
          s.add_exception_time(t0 + 10 * ONE_DAY)
        end

        result = compare(left, right, t0 + 30 * ONE_DAY)

        expect(result.status).to eq :different
        expect(result).to be_different
        expect(result.side).to eq :left
        expect(result.time).to eq t0 + 10 * ONE_DAY
      end

      it "reports the earliest difference owned by the right schedule" do
        left = build_schedule(Rule.daily) do |s|
          s.add_exception_time(t0 + 10 * ONE_DAY)
        end
        right = build_schedule(Rule.daily)

        result = compare(left, right, t0 + 30 * ONE_DAY)

        expect(result).to be_different
        expect(result.side).to eq :right
        expect(result.time).to eq t0 + 10 * ONE_DAY
      end

      it "reports the earliest difference when several exist" do
        left = build_schedule(Rule.daily)
        right = build_schedule(Rule.daily) do |s|
          s.add_exception_time(t0 + 5 * ONE_DAY)
          s.add_exception_time(t0 + 20 * ONE_DAY)
        end

        result = compare(left, right, t0 + 30 * ONE_DAY)

        expect(result).to be_different
        expect(result.time).to eq t0 + 5 * ONE_DAY
      end

      it "reports differences in start times of rule-less schedules" do
        result = compare(Schedule.new(t0), Schedule.new(t0 + ONE_HOUR), t0 + ONE_DAY)

        expect(result).to be_different
        expect(result.side).to eq :left
        expect(result.time).to eq t0
      end

      context "with schedules that only diverge years later" do
        let(:left) { build_schedule(Rule.yearly) }
        let(:right) { build_schedule(Rule.yearly.count(5)) }
        let(:divergence) { Time.utc(2031, 1, 5, 9, 0, 0) } # 6th occurrence

        it "confirms equality when the window ends before the divergence" do
          result = compare(left, right, Time.utc(2030, 1, 1))

          expect(result).to be_equal
        end

        it "finds the divergence when the window extends past it" do
          result = compare(left, right, Time.utc(2036, 1, 1))

          expect(result).to be_different
          expect(result.side).to eq :left
          expect(result.time).to eq divergence
        end
      end

      context "with very sparse rules" do
        let(:left) { build_schedule(Rule.yearly) }
        let(:right) { build_schedule(Rule.yearly) }
        let(:closing) { Time.utc(2126, 1, 5, 9, 0, 0) }

        it "returns an incomplete result when the work budget runs out" do
          result = compare(left, right, closing, max_steps: 4)

          expect(result.status).to eq :incomplete
          expect(result).to be_incomplete
          expect(result.time).to be_nil
          expect(result.side).to be_nil
        end

        it "completes when the budget covers the whole window" do
          result = compare(left, right, closing, max_steps: 1_000)

          expect(result).to be_equal
        end

        it "still finds sparse differences within budget" do
          right = build_schedule(Rule.yearly.count(50))

          result = compare(left, right, closing, max_steps: 1_000)

          expect(result).to be_different
          expect(result.side).to eq :left
          expect(result.time).to eq Time.utc(2076, 1, 5, 9, 0, 0)
        end
      end

      it "rejects a non-positive work budget" do
        expect {
          compare(Schedule.new(t0), Schedule.new(t0), t0, max_steps: 0)
        }.to raise_error(ArgumentError)
      end
    end

    describe "Schedule#compare_occurrences" do
      it "compares from the receiver as the left side" do
        left = build_schedule(Rule.daily)
        right = build_schedule(Rule.daily) do |s|
          s.add_exception_time(t0 + 3 * ONE_DAY)
        end

        result = left.compare_occurrences(right, t0, t0 + 10 * ONE_DAY)

        expect(result).to be_different
        expect(result.side).to eq :left
        expect(result.time).to eq t0 + 3 * ONE_DAY
      end

      it "confirms equality over the window" do
        left = build_schedule(Rule.weekly.day(:saturday, :sunday))
        right = build_schedule(Rule.daily.day(:saturday, :sunday))

        result = left.compare_occurrences(right, t0, t0 + 60 * ONE_DAY)

        expect(result).to be_equal
      end
    end
  end
end
