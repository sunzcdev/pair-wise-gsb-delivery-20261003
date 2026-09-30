require File.dirname(__FILE__) + "/../spec_helper"
require "json"

describe IceCube::OccurrenceIterator do
  include IceCube

  let(:start_time) { Time.utc(2020, 1, 1) }

  def paginate(schedule, closing_time, page_sizes, spans: false)
    occurrences = []
    enumerator = schedule.occurrence_enumerator(
      schedule.start_time,
      closing_time,
      spans: spans
    )

    page_sizes.cycle do |page_size|
      page = enumerator.take(page_size)
      break if page.empty?

      occurrences.concat(page)
      enumerator = IceCube::OccurrenceIterator.resume(
        schedule,
        enumerator.checkpoint_json,
        spans: spans
      )
    end

    occurrences
  end

  it "continues after the last emitted occurrence without repeating it" do
    schedule = Schedule.new(start_time)
    schedule.rrule Rule.daily.count(3)

    enumerator = schedule.occurrence_enumerator(start_time, start_time + 5 * ONE_DAY)
    expect(enumerator.take(2)).to eq([start_time, start_time + ONE_DAY])

    resumed = IceCube::OccurrenceIterator.resume(schedule, enumerator.checkpoint)
    expect(resumed.take(2)).to eq([start_time + 2 * ONE_DAY])
  end

  it "does not consume count again when resuming" do
    schedule = Schedule.new(start_time)
    rule = Rule.daily.count(3)
    schedule.rrule rule

    enumerator = schedule.occurrence_enumerator(start_time, start_time + 5 * ONE_DAY)
    enumerator.take(2)

    resumed = schedule.occurrence_enumerator(start_time, start_time + 5 * ONE_DAY, checkpoint: enumerator.checkpoint)
    expect(resumed.to_a).to eq([start_time + 2 * ONE_DAY])
    expect(rule.uses).to eq(3)
  end

  it "accepts an explicit non-spanning resume option" do
    schedule = Schedule.new(start_time, duration: ONE_HOUR)
    schedule.rrule Rule.daily.count(2)

    enumerator = schedule.occurrence_enumerator(
      start_time,
      start_time + 2 * ONE_DAY,
      spans: false
    )
    enumerator.next

    resumed = IceCube::OccurrenceIterator.resume(
      schedule,
      enumerator.checkpoint_json,
      spans: false
    )
    expect(resumed.to_a).to eq([start_time + ONE_DAY])
  end

  it "matches one enumeration across different page sizes with exceptions and overlap" do
    schedule = Schedule.new(start_time)
    schedule.rrule Rule.daily.count(4)
    schedule.rrule Rule.daily(2).count(3)
    schedule.extime start_time + 2 * ONE_DAY
    closing_time = start_time + 20 * ONE_DAY
    expected = schedule.occurrence_enumerator(start_time, closing_time).to_a

    [1, 2, 3, 5, 10, 100].each do |page_size|
      expect(paginate(schedule, closing_time, [page_size])).to eq(expected)
    end

    expect(paginate(schedule, closing_time, [1, 3, 2, 7])).to eq(expected)
  end

  it "counts occurrences removed by exception rules before resume" do
    schedule = Schedule.new(start_time)
    schedule.rrule Rule.daily.count(3)
    schedule.extime start_time + ONE_DAY
    closing_time = start_time + 5 * ONE_DAY

    enumerator = schedule.occurrence_enumerator(start_time, closing_time)
    enumerator.next

    resumed = IceCube::OccurrenceIterator.resume(schedule, enumerator.checkpoint_json)
    expect(resumed.to_a).to eq([start_time + 2 * ONE_DAY])
  end

  it "preserves interval anchoring when resuming a count rule" do
    schedule = Schedule.new(start_time)
    schedule.rrule Rule.daily.count(4)
    schedule.rrule Rule.weekly(2).day(:tuesday, :thursday).count(5)
    closing_time = start_time + 60 * ONE_DAY
    expected = schedule.occurrence_enumerator(start_time, closing_time).to_a

    enumerator = schedule.occurrence_enumerator(start_time, closing_time)
    first_page = enumerator.take(1)
    resumed = IceCube::OccurrenceIterator.resume(schedule, enumerator.checkpoint_json)

    expect(first_page + resumed.to_a).to eq(expected)
  end

  it "resumes count-limited exception rules after their removed occurrences" do
    schedule = Schedule.new(start_time)
    schedule.rrule Rule.daily.until(start_time + 5 * ONE_DAY)
    schedule.exrule Rule.daily.count(2)
    closing_time = start_time + 5 * ONE_DAY

    enumerator = schedule.occurrence_enumerator(start_time, closing_time)
    first_page = enumerator.next
    resumed = IceCube::OccurrenceIterator.resume(schedule, enumerator.checkpoint_json)

    expect([first_page] + resumed.to_a).to eq(
      schedule.occurrence_enumerator(start_time, closing_time).to_a
    )
  end

  it "preserves the spans boundary on resume" do
    schedule = Schedule.new(start_time, duration: ONE_HOUR)
    schedule.rrule Rule.hourly.count(3)
    opening_time = start_time + 30 * ONE_MINUTE
    closing_time = start_time + 4 * ONE_HOUR
    expected = schedule.occurrence_enumerator(opening_time, closing_time, spans: true).to_a

    enumerator = schedule.occurrence_enumerator(opening_time, closing_time, spans: true)
    first_page = enumerator.take(2)
    resumed = IceCube::OccurrenceIterator.resume(schedule, enumerator.checkpoint, spans: true)

    expect(first_page + resumed.to_a).to eq(expected)
  end

  it "rejects a checkpoint when the schedule rules change" do
    schedule = Schedule.new(start_time)
    schedule.rrule Rule.daily.count(3)
    enumerator = schedule.occurrence_enumerator(start_time, start_time + 5 * ONE_DAY)
    enumerator.next
    checkpoint = enumerator.checkpoint_json
    schedule.rrule Rule.daily(2).count(3)

    expect { IceCube::OccurrenceIterator.resume(schedule, checkpoint) }.to raise_error(ArgumentError)
  end

  it "rejects a checkpoint when enumeration boundaries change" do
    schedule = Schedule.new(start_time)
    schedule.rrule Rule.daily.count(3)
    enumerator = schedule.occurrence_enumerator(start_time, start_time + 5 * ONE_DAY)
    enumerator.next
    checkpoint = enumerator.checkpoint

    expect do
      schedule.occurrence_enumerator(
        start_time + ONE_DAY,
        start_time + 5 * ONE_DAY,
        checkpoint: checkpoint
      )
    end.to raise_error(ArgumentError)
  end

  it "rejects a checkpoint when the closing boundary changes" do
    schedule = Schedule.new(start_time)
    schedule.rrule Rule.daily.count(3)
    enumerator = schedule.occurrence_enumerator(start_time, start_time + 5 * ONE_DAY)
    enumerator.next

    expect do
      schedule.occurrence_enumerator(
        start_time,
        start_time + 6 * ONE_DAY,
        checkpoint: enumerator.checkpoint
      )
    end.to raise_error(ArgumentError)
  end

  it "rejects a checkpoint when the format version changes" do
    schedule = Schedule.new(start_time)
    schedule.rrule Rule.daily.count(3)
    enumerator = schedule.occurrence_enumerator(start_time, start_time + 5 * ONE_DAY)
    enumerator.next
    checkpoint = JSON.parse(enumerator.checkpoint_json)
    checkpoint["version"] = 2

    expect { IceCube::OccurrenceIterator.resume(schedule, checkpoint) }.to raise_error(ArgumentError)
  end

  it "rejects a checkpoint with tampered contents" do
    schedule = Schedule.new(start_time)
    schedule.rrule Rule.daily.count(3)
    enumerator = schedule.occurrence_enumerator(start_time, start_time + 5 * ONE_DAY)
    enumerator.next
    checkpoint = JSON.parse(enumerator.checkpoint_json)
    checkpoint["last_occurrence_time"] = (start_time + ONE_DAY).utc.iso8601(9)

    expect { IceCube::OccurrenceIterator.resume(schedule, checkpoint) }.to raise_error(ArgumentError)
  end

  it "exports only JSON-safe data and round-trips through JSON" do
    schedule = Schedule.new(start_time, duration: ONE_HOUR)
    schedule.rrule Rule.daily.count(2)
    schedule.rrule Rule.weekly.day(:wednesday).count(1)
    schedule.extime start_time + 7 * ONE_DAY
    enumerator = schedule.occurrence_enumerator(
      start_time,
      start_time + 14 * ONE_DAY,
      spans: true
    )
    enumerator.next

    checkpoint = JSON.parse(enumerator.checkpoint_json)
    expect(checkpoint).to eq(enumerator.checkpoint)
    expect(JSON.generate(checkpoint)).to eq(enumerator.checkpoint_json)
  end
end
