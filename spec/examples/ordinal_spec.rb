describe IceCube::Schedule do
  let(:start_time) { Time.utc(2026, 1, 1, 9, 30, 15) }

  def build_schedule(rule = nil, &block)
    IceCube::Schedule.new(start_time) do |schedule|
      schedule.add_recurrence_rule(rule) if rule
      block&.call(schedule)
    end
  end

  describe :occurrence_at do
    context "with pure-interval rules (fast path)" do
      [
        ["secondly", IceCube::Rule.secondly, 25],
        ["minutely", IceCube::Rule.minutely(7), 25],
        ["hourly", IceCube::Rule.hourly(6), 25],
        ["daily", IceCube::Rule.daily, 25],
        ["every third day", IceCube::Rule.daily(3), 25],
        ["weekly", IceCube::Rule.weekly, 25],
        ["every other week", IceCube::Rule.weekly(2, :monday), 15],
        ["monthly", IceCube::Rule.monthly, 25],
        ["every other month", IceCube::Rule.monthly(2), 25],
        ["yearly", IceCube::Rule.yearly, 10]
      ].each do |name, rule, limit|
        it "matches enumeration occurrence-by-occurrence for #{name} rules" do
          schedule = build_schedule(rule)
          expected = schedule.first(limit)
          (1..limit).each do |n|
            occurrence = schedule.occurrence_at(n)
            expect(occurrence.to_time).to eq expected[n - 1].to_time
            expect(occurrence.end_time).to eq expected[n - 1].end_time
          end
        end
      end

      it "clamps monthly occurrences to short months like enumeration does" do
        schedule = build_schedule(IceCube::Rule.monthly)
        schedule.start_time = Time.utc(2026, 1, 31, 8, 0, 0)
        expect(schedule.occurrence_at(1).to_time).to eq Time.utc(2026, 1, 31, 8)
        expect(schedule.occurrence_at(2).to_time).to eq Time.utc(2026, 2, 28, 8)
        expect(schedule.occurrence_at(3).to_time).to eq Time.utc(2026, 3, 31, 8)
        expect(schedule.occurrence_at(4).to_time).to eq Time.utc(2026, 4, 30, 8)
      end

      it "keeps the schedule duration on the returned occurrence" do
        schedule = IceCube::Schedule.new(start_time, duration: 3600) do |s|
          s.add_recurrence_rule IceCube::Rule.daily
        end
        occurrence = schedule.occurrence_at(3)
        expect(occurrence.start_time).to eq start_time + 2 * ONE_DAY
        expect(occurrence.end_time).to eq start_time + 2 * ONE_DAY + 3600
      end
    end

    context "with COUNT and UNTIL limits" do
      it "returns nil past the count" do
        schedule = build_schedule(IceCube::Rule.daily.count(7))
        expect(schedule.occurrence_at(7).to_time).to eq start_time + 6 * ONE_DAY
        expect(schedule.occurrence_at(8)).to be_nil
      end

      it "returns nil past the until time" do
        schedule = build_schedule(IceCube::Rule.daily.until(start_time + 10 * ONE_DAY))
        expect(schedule.occurrence_at(11).to_time).to eq start_time + 10 * ONE_DAY
        expect(schedule.occurrence_at(12)).to be_nil
      end

      it "honors an until Date as the end of that date" do
        schedule = build_schedule(IceCube::Rule.hourly.until(Date.new(2026, 1, 2)))
        last = schedule.occurrence_at(40)
        expect(last).to be_nil
        expect(schedule.occurrence_at(39).to_time).to eq Time.utc(2026, 1, 2, 23, 30, 15)
      end

      it "matches enumeration for every occurrence of a terminating rule" do
        schedule = build_schedule(IceCube::Rule.weekly(2).count(12))
        expected = schedule.all_occurrences
        (1..12).each do |n|
          expect(schedule.occurrence_at(n).to_time).to eq expected[n - 1].to_time
        end
        expect(schedule.occurrence_at(13)).to be_nil
      end
    end

    context "with far ordinals" do
      it "locates the 100,000th occurrence without walking the sequence" do
        rule = IceCube::Rule.daily
        schedule = build_schedule(rule)
        expect(schedule.occurrence_at(100_000).to_time).to eq start_time + 99_999 * ONE_DAY
        expect(rule.uses).to eq 0
      end

      it "locates far hourly and monthly occurrences arithmetically" do
        hourly = IceCube::Rule.hourly
        schedule = build_schedule(hourly)
        expect(schedule.occurrence_at(100_000).to_time).to eq start_time + 99_999 * ONE_HOUR
        expect(hourly.uses).to eq 0

        schedule = IceCube::Schedule.new(Time.utc(2026, 1, 31, 8)) do |s|
          s.add_recurrence_rule IceCube::Rule.monthly
        end
        expected_date = Date.new(2026, 1, 31) >> 99_999
        expect(schedule.occurrence_at(100_000).to_time).to eq(
          Time.utc(expected_date.year, expected_date.month, expected_date.day, 8)
        )
      end
    end

    context "with no recurrence rules" do
      it "exposes only the implicit start occurrence" do
        schedule = build_schedule
        expect(schedule.occurrence_at(1).to_time).to eq start_time
        expect(schedule.occurrence_at(2)).to be_nil
      end
    end

    context "with overlapping rules and exceptions (enumeration fallback)" do
      let(:schedule) do
        build_schedule do |s|
          s.add_recurrence_rule IceCube::Rule.daily
          s.add_recurrence_rule IceCube::Rule.weekly
          s.add_exception_time(start_time + 2 * ONE_DAY)
          s.add_exception_time(start_time + 5 * ONE_DAY)
        end
      end

      it "ordinals follow the final deduplicated sequence" do
        expected = schedule.first(30)
        (1..30).each do |n|
          expect(schedule.occurrence_at(n).to_time).to eq expected[n - 1].to_time
        end
      end

      it "skips exception times when assigning ordinals" do
        expect(schedule.occurrence_at(3).to_time).to eq start_time + 3 * ONE_DAY
        expect(schedule.occurrence_at(4).to_time).to eq start_time + 4 * ONE_DAY
        expect(schedule.occurrence_at(5).to_time).to eq start_time + 6 * ONE_DAY
      end

      it "walks the sequence instead of computing arithmetically" do
        schedule.occurrence_at(10)
        expect(schedule.recurrence_rules.map(&:uses)).to all(be > 0)
      end

      it "returns nil when a terminating schedule has fewer occurrences" do
        schedule = build_schedule do |s|
          s.add_recurrence_rule IceCube::Rule.daily.count(5)
          s.add_exception_time(start_time + ONE_DAY)
        end
        expect(schedule.occurrence_at(4).to_time).to eq start_time + 4 * ONE_DAY
        expect(schedule.occurrence_at(5)).to be_nil
      end
    end

    context "with a budget on the enumeration fallback" do
      let(:schedule) do
        build_schedule do |s|
          s.add_recurrence_rule IceCube::Rule.daily
          s.add_exception_time(start_time + ONE_DAY)
        end
      end

      it "raises BudgetExceeded when the walk would exceed the budget" do
        expect { schedule.occurrence_at(100, budget: 10) }
          .to raise_error(IceCube::BudgetExceeded, /budget/)
      end

      it "raises BudgetExceeded past the default budget", timeout: 10 do
        expect { schedule.occurrence_at(20_000) }
          .to raise_error(IceCube::BudgetExceeded)
      end

      it "succeeds within the budget" do
        expect(schedule.occurrence_at(10, budget: 10).to_time)
          .to eq start_time + 10 * ONE_DAY
      end

      it "allows an unbounded walk with budget: nil" do
        expect(schedule.occurrence_at(100, budget: nil).to_time)
          .to eq start_time + 100 * ONE_DAY
      end

      it "rejects invalid budgets" do
        expect { schedule.occurrence_at(1, budget: 0) }.to raise_error(ArgumentError)
      end
    end

    it "requires a positive integer ordinal" do
      schedule = build_schedule(IceCube::Rule.daily)
      expect { schedule.occurrence_at(0) }.to raise_error(ArgumentError)
      expect { schedule.occurrence_at(-3) }.to raise_error(ArgumentError)
      expect { schedule.occurrence_at(1.5) }.to raise_error(ArgumentError)
      expect { schedule.occurrence_at("10") }.to raise_error(ArgumentError)
    end
  end

  describe :occurrences_before do
    context "with pure-interval rules (fast path)" do
      it "matches enumeration counts at and between occurrences" do
        schedule = build_schedule(IceCube::Rule.daily(3))
        expected = schedule.first(25)
        expected.each_with_index do |occurrence, index|
          expect(schedule.occurrences_before(occurrence.to_time)).to eq index
          expect(schedule.occurrences_before(occurrence.to_time + 1)).to eq index + 1
          midpoint = occurrence.to_time + ONE_DAY
          expect(schedule.occurrences_before(midpoint)).to eq index + 1
        end
      end

      it "is zero at and before the start time" do
        schedule = build_schedule(IceCube::Rule.hourly)
        expect(schedule.occurrences_before(start_time)).to eq 0
        expect(schedule.occurrences_before(start_time - ONE_HOUR)).to eq 0
      end

      it "counts far offsets without walking the sequence" do
        rule = IceCube::Rule.daily
        schedule = build_schedule(rule)
        expect(schedule.occurrences_before(start_time + 99_999 * ONE_DAY)).to eq 99_999
        expect(schedule.occurrences_before(start_time + 100_000 * ONE_DAY)).to eq 100_000
        expect(rule.uses).to eq 0
      end

      it "round-trips with occurrence_at" do
        schedule = build_schedule(IceCube::Rule.weekly(2))
        occurrence = schedule.occurrence_at(500)
        expect(schedule.occurrences_before(occurrence.to_time)).to eq 499
        expect(schedule.occurrences_before(occurrence.to_time + 1)).to eq 500
      end
    end

    context "with COUNT and UNTIL limits" do
      it "caps the count at the rule's count" do
        schedule = build_schedule(IceCube::Rule.daily.count(7))
        expect(schedule.occurrences_before(start_time + 365 * ONE_DAY)).to eq 7
        expect(schedule.occurrences_before(start_time + 6 * ONE_DAY)).to eq 6
      end

      it "caps the count at the until time" do
        schedule = build_schedule(IceCube::Rule.daily.until(start_time + 10 * ONE_DAY))
        expect(schedule.occurrences_before(start_time + 365 * ONE_DAY)).to eq 11
        expect(schedule.occurrences_before(start_time + 10 * ONE_DAY)).to eq 10
        expect(schedule.occurrences_before(start_time + 10 * ONE_DAY + 1)).to eq 11
      end

      it "combines count and until by taking the tighter limit" do
        schedule = build_schedule(
          IceCube::Rule.daily.count(100).until(start_time + 10 * ONE_DAY)
        )
        expect(schedule.occurrences_before(start_time + 365 * ONE_DAY)).to eq 11
      end
    end

    context "with no recurrence rules" do
      it "counts only the implicit start occurrence" do
        schedule = build_schedule
        expect(schedule.occurrences_before(start_time)).to eq 0
        expect(schedule.occurrences_before(start_time + 1)).to eq 1
      end
    end

    context "with overlapping rules and exceptions (enumeration fallback)" do
      let(:schedule) do
        build_schedule do |s|
          s.add_recurrence_rule IceCube::Rule.daily
          s.add_recurrence_rule IceCube::Rule.weekly
          s.add_exception_time(start_time + 2 * ONE_DAY)
        end
      end

      it "counts the final deduplicated sequence" do
        # daily and weekly overlap every seventh day; the exception removes
        # a third occurrence, so 8 calendar days hold 7 occurrences
        expect(schedule.occurrences_before(start_time + 7 * ONE_DAY)).to eq 6
        expect(schedule.occurrences_before(start_time + 8 * ONE_DAY)).to eq 7
      end

      it "matches enumeration counts" do
        expected = schedule.first(30)
        expected.each do |occurrence|
          expect(schedule.occurrences_before(occurrence.to_time)).to eq(
            expected.count { |o| o.to_time < occurrence.to_time }
          )
        end
      end

      it "raises BudgetExceeded when the walk would exceed the budget" do
        expect { schedule.occurrences_before(start_time + 100 * ONE_DAY, budget: 10) }
          .to raise_error(IceCube::BudgetExceeded, /budget/)
      end

      it "allows an unbounded walk with budget: nil" do
        expect(schedule.occurrences_before(start_time + 100 * ONE_DAY, budget: nil))
          .to eq 99
      end
    end

    it "requires a time" do
      schedule = build_schedule(IceCube::Rule.daily)
      expect { schedule.occurrences_before(nil) }.to raise_error(ArgumentError)
    end
  end
end
