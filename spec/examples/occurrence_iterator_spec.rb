require File.dirname(__FILE__) + "/../spec_helper"
require "json"

describe IceCube::OccurrenceIterator do
  START_TIME = Time.utc(2020, 1, 1, 9, 30, 15)
  CLOSING_TIME = Time.utc(2021, 1, 1)

  def count_schedule(count = 10)
    schedule = IceCube::Schedule.new(START_TIME)
    schedule.add_recurrence_rule IceCube::Rule.daily.count(count)
    schedule
  end

  def paged(builder, page_sizes, options = {})
    schedule = builder.call
    iterator = schedule.occurrence_iterator(START_TIME, CLOSING_TIME, options)
    collected = []
    page_sizes.each do |size|
      collected.concat iterator.next_occurrences(size)
      checkpoint = iterator.checkpoint
      schedule = builder.call
      iterator = schedule.resume_occurrence_iterator(JSON.generate(checkpoint))
    end
    [collected, iterator]
  end

  describe "fresh iteration" do
    it "matches occurrences_between for an unbounded rule cut off by closing time" do
      schedule = count_schedule(400)
      iterator = schedule.occurrence_iterator(START_TIME, CLOSING_TIME)

      expect(iterator.to_a).to eq(schedule.occurrences_between(START_TIME, CLOSING_TIME))
    end

    it "starts from the schedule start time by default" do
      schedule = count_schedule(3)
      iterator = schedule.occurrence_iterator

      expect(iterator.to_a.map(&:to_i)).to eq(schedule.all_occurrences.map(&:to_i))
      expect(iterator).to be_exhausted
    end

    it "is Enumerable" do
      iterator = count_schedule(4).occurrence_iterator

      expect(iterator.first(2).map(&:day)).to eq([1, 2])
    end

    it "supports each with a block and a block-less enumerator" do
      seen = []
      count_schedule(3).occurrence_iterator.each { |occurrence| seen << occurrence }

      expect(seen.length).to eq(3)
      expect(count_schedule(1).occurrence_iterator.each).to be_a(Enumerator)
    end

    it "returns fewer occurrences than requested when exhausting early" do
      iterator = count_schedule(2).occurrence_iterator

      page = iterator.next_occurrences(10)

      expect(page.length).to eq(2)
      expect(iterator).to be_exhausted
      expect(iterator.next_occurrence).to be_nil
      expect(iterator.next_occurrences(3)).to eq([])
    end
  end

  describe "checkpoint contents" do
    it "only contains JSON-safe primitives" do
      iterator = count_schedule(5).occurrence_iterator(START_TIME, CLOSING_TIME)
      iterator.next_occurrence
      checkpoint = iterator.checkpoint

      expect { JSON.generate(checkpoint) }.not_to raise_error
      expect(JSON.parse(JSON.generate(checkpoint))).to eq(checkpoint)
    end

    it "round-trips through JSON and continues after the last item" do
      schedule = count_schedule(5)
      iterator = schedule.occurrence_iterator(START_TIME, CLOSING_TIME)
      iterator.next_occurrences(2)

      restored = count_schedule(5).resume_occurrence_iterator(iterator.to_json)

      expect(restored.next_occurrence.day).to eq(3)
    end

    it "exposes the format version" do
      checkpoint = count_schedule(1).occurrence_iterator(START_TIME).checkpoint

      expect(checkpoint["version"]).to eq(IceCube::OccurrenceIterator::FORMAT_VERSION)
    end
  end

  describe "resuming" do
    it "continues strictly after the last emitted occurrence" do
      iterator = count_schedule(5).occurrence_iterator(START_TIME, CLOSING_TIME)
      first = iterator.next_occurrence

      resumed = count_schedule(5).resume_occurrence_iterator(iterator.checkpoint)
      following = resumed.next_occurrence

      expect(following.to_i).to eq(first.to_i + ONE_DAY)
      expect(following).not_to eq(first)
    end

    it "does not re-consume COUNT across pages" do
      iterator = count_schedule(3).occurrence_iterator
      iterator.next_occurrence

      resumed = count_schedule(3).resume_occurrence_iterator(iterator.checkpoint)

      expect(resumed.to_a.length).to eq(2)
    end

    it "stays exhausted after the final occurrence" do
      iterator = count_schedule(2).occurrence_iterator
      iterator.next_occurrences(2)

      resumed = count_schedule(2).resume_occurrence_iterator(iterator.checkpoint)

      expect(resumed.next_occurrence).to be_nil
      expect(resumed).to be_exhausted
    end

    it "can resume without emitting anything first" do
      checkpoint = count_schedule(3).occurrence_iterator(START_TIME, CLOSING_TIME).checkpoint

      resumed = count_schedule(3).resume_occurrence_iterator(checkpoint)

      expect(resumed.next_occurrence.day).to eq(1)
    end

    it "resumes against a schedule reconstructed from YAML" do
      schedule = IceCube::Schedule.new(START_TIME)
      schedule.add_recurrence_rule IceCube::Rule.daily(2).count(6)
      schedule.add_exception_time(START_TIME + ONE_DAY * 4)
      iterator = schedule.occurrence_iterator
      iterator.next_occurrences(2)

      rebuilt = IceCube::Schedule.from_yaml(schedule.to_yaml)
      resumed = rebuilt.resume_occurrence_iterator(iterator.checkpoint)

      expect(resumed.to_a.map(&:to_i)).to eq(schedule.all_occurrences.drop(2).map(&:to_i))
    end
  end

  describe "paging equivalence" do
    it "is independent of the page size chosen" do
      builder = proc { count_schedule(50) }
      expected = builder.call.occurrences_between(START_TIME, CLOSING_TIME)

      partition_a, = paged(builder, [1] * 60)
      partition_b, = paged(builder, [50, 50])
      partition_c, = paged(builder, [7, 13, 2, 9, 40])

      expect(partition_a.map(&:to_i)).to eq(expected.map(&:to_i))
      expect(partition_b.map(&:to_i)).to eq(expected.map(&:to_i))
      expect(partition_c.map(&:to_i)).to eq(expected.map(&:to_i))
    end

    {
      "daily with count" => proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_rule IceCube::Rule.daily.count(20)
        s
      },
      "weekly interval" => proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_rule IceCube::Rule.weekly(2).day(:monday, :wednesday).count(30)
        s
      },
      "monthly last day" => proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_rule IceCube::Rule.monthly.day_of_month(-1).count(18)
        s
      },
      "hour_of_day" => proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_rule IceCube::Rule.hourly.hour_of_day(9, 17).count(40)
        s
      },
      "count plus exception rule" => proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_rule IceCube::Rule.daily.count(30)
        s.add_exception_rule IceCube::Rule.weekly.day(:saturday, :sunday)
        s
      },
      "count plus exception times" => proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_rule IceCube::Rule.daily.count(30)
        s.add_exception_time(START_TIME + ONE_DAY * 2)
        s.add_exception_time(START_TIME + ONE_DAY * 5)
        s
      },
      "overlapping rules" => proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_rule IceCube::Rule.daily(2).count(12)
        s.add_recurrence_rule IceCube::Rule.weekly.day(:thursday).count(6)
        s
      },
      "extra recurrence times" => proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_time(START_TIME + ONE_DAY)
        s.add_recurrence_time(START_TIME + ONE_DAY * 3)
        s.add_recurrence_rule IceCube::Rule.daily.count(5)
        s
      },
      "only individual recurrence times" => proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_time(START_TIME + ONE_DAY)
        s.add_recurrence_time(START_TIME + ONE_DAY * 3)
        s.add_recurrence_time(START_TIME + ONE_DAY * 5)
        s
      },
      "no rules, start time only" => proc {
        IceCube::Schedule.new(START_TIME)
      },
      "until bound" => proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_rule IceCube::Rule.daily.until(Time.utc(2020, 1, 20))
        s
      },
      "monthly BYSETPOS with count" => proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_rule(
          IceCube::Rule.monthly.day_of_week(monday: [1, 2, 3, -1]).by_set_pos(-1).count(18)
        )
        s
      }
    }.each do |label, builder|
      it "matches a single enumeration for #{label}" do
        expected = builder.call.occurrences_between(START_TIME, CLOSING_TIME)

        collected, iterator = paged(builder, [1, 3, 1, 2, 5, 1, 4, 2, 10, 50])

        expect(collected.map(&:to_i)).to eq(expected.map(&:to_i))
        expect(iterator).to be_exhausted
        expect(iterator.next_occurrences(3)).to eq([])
      end
    end

    it "honors exception dates across a page boundary" do
      builder = proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_rule IceCube::Rule.daily.count(6)
        s.add_exception_time(START_TIME + ONE_DAY * 3)
        s
      }
      expected = builder.call.occurrences_between(START_TIME, CLOSING_TIME)

      collected, = paged(builder, [2, 2, 2, 2])

      expect(collected.map(&:day)).to eq(expected.map(&:day))
      expect(collected.map(&:day)).not_to include(4)
    end

    it "preserves overlapping rule de-duplication across a page boundary" do
      builder = proc {
        s = IceCube::Schedule.new(START_TIME)
        s.add_recurrence_rule IceCube::Rule.daily.count(5)
        s.add_recurrence_rule IceCube::Rule.daily.count(5)
        s
      }

      collected, = paged(builder, [2, 2, 2, 10])

      expect(collected.length).to eq(5)
    end

    it "matches a spans enumeration when the schedule has a duration" do
      builder = proc {
        s = IceCube::Schedule.new(START_TIME, duration: ONE_HOUR)
        s.add_recurrence_rule IceCube::Rule.daily(3).count(20)
        s
      }
      expected = builder.call.occurrences_between(START_TIME, CLOSING_TIME, spans: true)

      collected, iterator = paged(builder, [1, 4, 2, 3, 50], spans: true)

      expect(collected.map(&:to_i)).to eq(expected.map(&:to_i))
      expect(iterator).to be_exhausted
    end

    it "honors a custom opening time and skips occurrences before it" do
      builder = proc { count_schedule(40) }
      opening = Time.utc(2020, 1, 5, 0, 0, 0)
      expected = builder.call.occurrences_between(opening, CLOSING_TIME)

      schedule = builder.call
      iterator = schedule.occurrence_iterator(opening, CLOSING_TIME)
      first_page = iterator.next_occurrences(3)
      resumed = builder.call.resume_occurrence_iterator(iterator.checkpoint)
      collected = first_page + resumed.to_a

      expect(collected.map(&:to_i)).to eq(expected.map(&:to_i))
    end
  end

  describe "checkpoint binding" do
    let(:checkpoint) do
      count_schedule(5).occurrence_iterator(START_TIME, CLOSING_TIME).checkpoint
    end

    it "rejects an unknown format version" do
      data = JSON.parse(JSON.generate(checkpoint))
      data["version"] = 999

      expect {
        count_schedule(5).resume_occurrence_iterator(data.to_json)
      }.to raise_error(IceCube::InvalidCheckpoint, /version/i)
    end

    it "rejects when a rule count changes" do
      changed = IceCube::Schedule.new(START_TIME)
      changed.add_recurrence_rule IceCube::Rule.daily.count(6)

      expect {
        changed.resume_occurrence_iterator(checkpoint)
      }.to raise_error(IceCube::InvalidCheckpoint, /does not belong/)
    end

    it "rejects when a rule definition changes" do
      changed = IceCube::Schedule.new(START_TIME)
      changed.add_recurrence_rule IceCube::Rule.daily(2).count(5)

      expect {
        changed.resume_occurrence_iterator(checkpoint)
      }.to raise_error(IceCube::InvalidCheckpoint)
    end

    it "rejects when the start time changes" do
      changed = IceCube::Schedule.new(Time.utc(2019, 1, 1, 9, 30, 15))
      changed.add_recurrence_rule IceCube::Rule.daily.count(5)

      expect {
        changed.resume_occurrence_iterator(checkpoint)
      }.to raise_error(IceCube::InvalidCheckpoint)
    end

    it "rejects when a rule is added" do
      changed = count_schedule(5)
      changed.add_exception_rule IceCube::Rule.weekly.day(:sunday)

      expect {
        changed.resume_occurrence_iterator(checkpoint)
      }.to raise_error(IceCube::InvalidCheckpoint)
    end

    it "rejects an explicitly different closing time" do
      expect {
        IceCube::OccurrenceIterator.new(
          count_schedule(5), START_TIME, Time.utc(2022, 1, 1), resume: checkpoint
        )
      }.to raise_error(IceCube::InvalidCheckpoint, /closing time/i)
    end

    it "rejects an explicitly different opening time" do
      expect {
        IceCube::OccurrenceIterator.new(
          count_schedule(5), Time.utc(2020, 6, 1), CLOSING_TIME, resume: checkpoint
        )
      }.to raise_error(IceCube::InvalidCheckpoint, /opening time/i)
    end

    it "rejects a mismatched spans option" do
      builder = proc {
        s = IceCube::Schedule.new(START_TIME, duration: ONE_HOUR)
        s.add_recurrence_rule IceCube::Rule.daily.count(5)
        s
      }
      spans_checkpoint = builder.call.occurrence_iterator(
        START_TIME, CLOSING_TIME, spans: true
      ).checkpoint

      expect {
        IceCube::OccurrenceIterator.new(
          builder.call, START_TIME, CLOSING_TIME, spans: false, resume: spans_checkpoint
        )
      }.to raise_error(IceCube::InvalidCheckpoint, /spans/i)
    end

    it "accepts a checkpoint for an unchanged schedule" do
      expect {
        count_schedule(5).resume_occurrence_iterator(checkpoint)
      }.not_to raise_error
    end
  end

  describe "malformed input" do
    [
      "not json",
      "123",
      "null",
      "true",
      "[1, 2]",
      '{"version": 1}',
      ""
    ].each do |bad|
      it "rejects #{bad.inspect}" do
        expect {
          count_schedule(1).resume_occurrence_iterator(bad)
        }.to raise_error(IceCube::InvalidCheckpoint)
      end
    end

    it "rejects a non-string, non-hash checkpoint" do
      expect {
        count_schedule(1).resume_occurrence_iterator(1234)
      }.to raise_error(IceCube::InvalidCheckpoint)
    end

    it "does not instantiate arbitrary classes embedded in the payload" do
      payload = JSON.dump(
        "json_class" => "Foo",
        "version" => 1
      )

      expect {
        count_schedule(1).resume_occurrence_iterator(payload)
      }.to raise_error(IceCube::InvalidCheckpoint)
    end
  end
end

