require "spec_helper"

module IceCube
  describe Schedule, :compare_occurrences do
    let(:t0) { Time.utc(2020, 1, 1) }

    describe "rules written differently but producing the same times" do
      it "is equal for weekly(:weekend) vs two day_of_week rules" do
        one = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.weekly.day(:saturday, :sunday))
        end
        two = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.weekly.day(:sunday))
          s.add_recurrence_rule(IceCube::Rule.weekly.day(:saturday))
        end

        result = one.compare_occurrences(two, Time.utc(2024, 6, 1))

        expect(result).to be_equal
        expect(result.status).to eq(:equal)
        expect(one.to_hash[:rrules]).not_to eq(two.to_hash[:rrules])
      end

      it "is equal for monthly last day expressed two ways" do
        closing = Time.utc(2024, 12, 31)
        one = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.monthly.day_of_month(-1).until(closing))
        end
        two = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.monthly.day_of_month(-1).count(60))
        end

        result = one.compare_occurrences(two, closing)

        expect(result).to be_equal
      end

      it "is equal when single rules are split across two rules" do
        one = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.daily.day(:monday, :wednesday, :friday))
        end
        two = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.daily.day(:monday, :wednesday))
          s.add_recurrence_rule(IceCube::Rule.daily.day(:friday))
        end

        result = one.compare_occurrences(two, Time.utc(2022, 2, 1))

        expect(result).to be_equal
      end

      it "agrees after a serialization round-trip without comparing strings" do
        one = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.weekly.day(:saturday, :sunday))
        end
        two = IceCube::Schedule.from_hash(one.to_hash)

        expect(one.compare_occurrences(two, Time.utc(2023, 1, 1))).to be_equal
      end
    end

    describe "rules that only diverge years later" do
      it "stays equal across common years and reports the leap-year divergence" do
        # Feb 29 is skipped by non-leap years; both schedules agree for years
        # before the first leap year inside the interval.
        one = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.yearly.day_of_month(29).month_of_year(2))
        end
        two = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(
            IceCube::Rule.yearly.day_of_month(29).month_of_year(2).until(Time.utc(2024, 2, 28))
          )
        end

        equal_part = one.compare_occurrences(two, Time.utc(2024, 2, 28))
        expect(equal_part).to be_equal

        result = one.compare_occurrences(two, Time.utc(2024, 3, 1))
        expect(result).to be_different
        expect(result.time.start_time).to eq(Time.utc(2024, 2, 29))
        expect(result.side).to eq(:self)
        expect(result).to be_only_in_self
      end

      it "finds a divergence far beyond the first few occurrences" do
        # Identical until one schedule stops after a count; the 121st monthly
        # occurrence (Jan 2030) exists only on the unbounded side.
        bounded = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.monthly(1).count(120))
        end
        unbounded = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.monthly(1))
        end

        result = bounded.compare_occurrences(unbounded, Time.utc(2030, 6, 1))

        expect(result).to be_different
        expect(result.time.start_time).to eq(Time.utc(2030, 1, 1))
        expect(result.side).to eq(:other)
        expect(result).to be_only_in_other
        expect(result.examined).to eq(120)
      end
    end

    describe "work limits for sparse rules" do
      it "returns an incomplete result when the limit is exhausted" do
        one = IceCube::Schedule.new(t0) { |s| s.add_recurrence_rule(IceCube::Rule.daily) }
        two = IceCube::Schedule.new(t0) { |s| s.add_recurrence_rule(IceCube::Rule.daily) }

        result = one.compare_occurrences(two, Time.utc(2030, 1, 1), limit: 10)

        expect(result).to be_incomplete
        expect(result.examined).to eq(10)
        expect(result.verified_through).to eq(Time.utc(2020, 1, 11))
        expect(result.time).to be_nil
      end

      it "can resume from verified_through and finish as equal" do
        one = IceCube::Schedule.new(t0) { |s| s.add_recurrence_rule(IceCube::Rule.daily) }
        two = IceCube::Schedule.new(t0) { |s| s.add_recurrence_rule(IceCube::Rule.daily) }
        closing = Time.utc(2020, 1, 25)

        first = one.compare_occurrences(two, closing, t0, limit: 10)
        expect(first).to be_incomplete

        resumed = one.compare_occurrences(
          two,
          closing,
          first.verified_through,
          limit: 100
        )
        expect(resumed).to be_equal
      end

      it "reports a real difference without exhausting the limit" do
        one = IceCube::Schedule.new(t0) { |s| s.add_recurrence_rule(IceCube::Rule.monthly) }
        two = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.monthly.until(Time.utc(2020, 6, 1)))
        end

        result = one.compare_occurrences(two, Time.utc(2021, 1, 1), limit: 10)

        expect(result).to be_different
        expect(result.time.start_time).to eq(Time.utc(2020, 7, 1))
        expect(result.examined).to eq(6)
      end


      it "returns incomplete when the budget runs out before agreement is proven" do
        one = IceCube::Schedule.new(t0) { |s| s.add_recurrence_rule(IceCube::Rule.monthly) }
        two = IceCube::Schedule.new(t0) { |s| s.add_recurrence_rule(IceCube::Rule.monthly(1)) }

        result = one.compare_occurrences(two, Time.utc(2021, 1, 1), limit: 7)

        expect(result).to be_incomplete
        expect(result.examined).to eq(7)
        expect(result.verified_through).to eq(Time.utc(2020, 8, 1))
      end

      it "handles very sparse schedules without exhausting the budget" do
        one = IceCube::Schedule.new(t0) { |s| s.add_recurrence_rule(IceCube::Rule.yearly) }
        two = IceCube::Schedule.new(t0) { |s| s.add_recurrence_rule(IceCube::Rule.yearly) }

        result = one.compare_occurrences(two, Time.utc(2100, 1, 1), limit: 1000)

        expect(result).to be_equal
        expect(result.examined).to eq(81)
      end
    end

    describe "exceptions and isolated times" do
      it "attributes an extra recurrence time to the owning side" do
        one = IceCube::Schedule.new(t0) { |s| s.add_recurrence_rule(IceCube::Rule.daily) }
        two = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.daily)
          s.add_recurrence_time(Time.utc(2020, 1, 5, 12))
        end

        result = one.compare_occurrences(two, Time.utc(2020, 1, 10))

        expect(result).to be_different
        expect(result.time.start_time).to eq(Time.utc(2020, 1, 5, 12))
        expect(result.side).to eq(:other)
      end

      it "treats an exception time as a missing occurrence on one side" do
        one = IceCube::Schedule.new(t0) { |s| s.add_recurrence_rule(IceCube::Rule.daily) }
        two = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.daily)
          s.add_exception_time(Time.utc(2020, 1, 3))
        end

        result = one.compare_occurrences(two, Time.utc(2020, 1, 10))

        expect(result).to be_different
        expect(result.time.start_time).to eq(Time.utc(2020, 1, 3))
        expect(result.side).to eq(:self)
      end
    end

    describe "interval boundaries" do
      it "defaults the beginning to the earlier start time" do
        earlier = IceCube::Schedule.new(Time.utc(2019, 6, 1)) do |s|
          s.add_recurrence_rule(IceCube::Rule.monthly)
        end
        later = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.monthly)
        end

        result = earlier.compare_occurrences(later, Time.utc(2019, 8, 1))

        expect(result).to be_different
        expect(result.time.start_time).to eq(Time.utc(2019, 6, 1))
        expect(result.side).to eq(:self)
      end

      it "includes an occurrence exactly on the closing time" do
        one = IceCube::Schedule.new(t0) { |s| s.add_recurrence_rule(IceCube::Rule.daily) }
        two = IceCube::Schedule.new(t0) do |s|
          s.add_recurrence_rule(IceCube::Rule.daily.until(Time.utc(2020, 1, 4)))
        end

        equal_result = one.compare_occurrences(two, Time.utc(2020, 1, 4))
        expect(equal_result).to be_equal

        different_result = one.compare_occurrences(two, Time.utc(2020, 1, 5))
        expect(different_result).to be_different
        expect(different_result.time.start_time).to eq(Time.utc(2020, 1, 5))
      end
    end

    describe "argument validation" do
      it "raises when compared against a non-schedule" do
        schedule = IceCube::Schedule.new(t0)
        expect {
          schedule.compare_occurrences(:not_a_schedule, Time.utc(2020, 2, 1))
        }.to raise_error(ArgumentError)
      end

      it "raises without a closing time" do
        schedule = IceCube::Schedule.new(t0)
        other = IceCube::Schedule.new(t0)
        expect {
          schedule.compare_occurrences(other, nil)
        }.to raise_error(ArgumentError)
      end
    end
  end
end
