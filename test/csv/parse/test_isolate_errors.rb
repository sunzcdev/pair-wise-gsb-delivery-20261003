# frozen_string_literal: true

require_relative "../helper"

class TestCSVParseIsolateErrors < Test::Unit::TestCase
  include CSVHelper

  def events(data, **options)
    records = []
    CSV.new(data, **options.merge(isolate_errors: true)).each do |record|
      records << record
    end
    records
  end

  def bad?(record)
    record.is_a?(CSV::MalformedRecord)
  end

  def assert_byte_range(data, event)
    slice = data.byteslice(event.byte_range.begin,
                           event.byte_range.end - event.byte_range.begin)
    assert_equal(slice, event.raw)
  end

  # A short file that contains a legitimate multiline (quoted) field,
  # a recoverable malformed record, and more data afterwards.
  def test_recovers_after_malformed_record_with_multiline_field
    data = <<~CSV
      a,b
      "multi
      line",2
      x,"bad"q
      c,d
    CSV
    records = events(data)

    assert_equal(["a", "b"], records[0])
    assert_equal(["multi\nline", "2"], records[1])

    event = records[2]
    assert_kind_of(CSV::MalformedRecord, event)
    assert_equal(3, event.lineno)
    assert_equal(true, event.recoverable?)
    assert_equal(true, event.valid? == false)
    assert_equal("x,\"bad\"q\n", event.raw)
    assert_equal(19...28, event.byte_range)
    assert_byte_range(data, event)
    assert_match(/Any value after quoted field/, event.reason)

    # The following good record is not lost.
    assert_equal(["c", "d"], records[3])
    assert_equal(4, records.size)
  end

  def test_record_numbers_are_continuous_after_recovery
    data = "ok,1\n\"x\"y\n\"p\"q\nz,9\n"
    records = events(data)

    assert_equal([["ok", "1"]], records[0..0])
    assert_equal(2, records[1].lineno)
    assert_equal(3, records[2].lineno)
    assert_equal(["z", "9"], records[3])
    assert_equal(true, records[1].recoverable?)
    assert_equal(true, records[2].recoverable?)
    records.grep(CSV::MalformedRecord) do |event|
      assert_byte_range(data, event)
    end
  end

  def test_following_data_is_not_dropped
    data = "a,1\nbad,\"q\"z\nafter,3\n"
    records = events(data)
    assert_equal(3, records.size)
    assert_equal(["a", "1"], records[0])
    assert_kind_of(CSV::MalformedRecord, records[1])
    assert_equal(["after", "3"], records[2])
  end

  def test_unclosed_quote_is_unrecoverable_and_stops
    data = "a,b\nx,\"never closed\ngood,2\n"
    records = events(data)

    assert_equal(2, records.size)
    assert_equal(["a", "b"], records[0])

    event = records[1]
    assert_kind_of(CSV::MalformedRecord, event)
    assert_equal(2, event.lineno)
    assert_equal(false, event.recoverable?)
    assert_equal("Unclosed quoted field", event.reason)
    # Everything from the record start to EOF is attached, because no
    # boundary after the open quote can be confirmed.
    assert_equal("x,\"never closed\ngood,2\n", event.raw)
    assert_byte_range(data, event)
  end

  def test_unclosed_quote_consumes_multiline_remainder
    data = "a,b\n\"multi\nline\",2\ne,\"unclosed\nmore\ndata"
    records = events(data)
    event = records.last
    assert_kind_of(CSV::MalformedRecord, event)
    assert_equal(false, event.recoverable?)
    assert_equal("e,\"unclosed\nmore\ndata", event.raw)
    assert_byte_range(data, event)
  end

  def test_recoverable_record_at_eof_without_row_separator
    data = "a,1\nb,\"x\"y"
    records = events(data)
    event = records[1]
    assert_kind_of(CSV::MalformedRecord, event)
    assert_equal(true, event.recoverable?)
    assert_equal("b,\"x\"y", event.raw)
    assert_byte_range(data, event)
  end

  def test_byte_ranges_are_byte_based_not_character_based
    data = "あ,い\nb,\"x\"y\nc,3\n"
    records = events(data)
    event = records[1]
    assert_kind_of(CSV::MalformedRecord, event)
    assert_equal(8...15, event.byte_range)
    assert_byte_range(data, event)
    assert_equal(Encoding::UTF_8, event.raw.encoding)
  end

  def test_strict_mode_is_unchanged_by_default
    data = "a,1\nb,\"x\"y\nc,3\n"
    assert_raise(CSV::MalformedCSVError) do
      CSV.parse(data)
    end
    error = assert_raise(CSV::MalformedCSVError) do
      CSV.new(data).each { }
    end
    assert_match(/Any value after quoted field/, error.message)
  end

  def test_liberal_parsing_results_are_not_isolation_events
    data = "is,this \"three, or four\",fields\n"
    records = events(data, liberal_parsing: true)
    assert_equal(1, records.size)
    assert_equal(false, bad?(records[0]))
  end

  def test_liberal_unclosed_quote_still_reported
    data = "a,\"b"
    records = events(data, liberal_parsing: true)
    event = records.last
    assert_kind_of(CSV::MalformedRecord, event)
    assert_equal(false, event.recoverable?)
  end

  def test_crlf_row_separator
    data = "a,b\r\nbad,\"q\"z\r\nc,d\r\n"
    records = events(data, row_sep: "\r\n")
    event = records[1]
    assert_kind_of(CSV::MalformedRecord, event)
    assert_equal(2, event.lineno)
    assert_equal("bad,\"q\"z\r\n", event.raw)
    assert_byte_range(data, event)
    assert_equal(["c", "d"], records[2])
  end

  def test_headers_mode
    data = "h1,h2\na,1\nb,\"x\"y\nc,3\n"
    records = events(data, headers: true)
    assert_equal("a", records[0]["h1"])
    event = records[1]
    assert_kind_of(CSV::MalformedRecord, event)
    assert_equal(3, event.lineno)
    assert_equal("c", records[2]["h1"])
  end

  def test_shift_returns_events_and_resumes
    csv = CSV.new("a,1\nb,\"x\"y\nc,3\n", isolate_errors: true)
    assert_equal(["a", "1"], csv.shift)
    event = csv.shift
    assert_kind_of(CSV::MalformedRecord, event)
    assert_equal(2, event.lineno)
    assert_equal(["c", "3"], csv.shift)
    assert_nil(csv.shift)
  end

  def test_lineno_tracks_isolated_records
    csv = CSV.new("a,1\nb,\"x\"y\nc,3\n", isolate_errors: true)
    seen = []
    csv.each do |record|
      seen << [csv.lineno, bad?(record) ? :event : :row]
    end
    assert_equal([[1, :row], [2, :event], [3, :row]], seen)
  end

  def test_byte_ranges_with_chunked_input_scanner
    data = "a,b\n\"multi\nline\",2\nx,\"bad\"q\nc,d\ne,\"unclosed\nrest"
    with_chunk_size("1") do
      records = events(data)
      records.each do |record|
        next unless bad?(record)
        assert_byte_range(data, record)
      end
      assert_equal(19...28, records[2].byte_range)
      assert_equal(true, records[2].recoverable?)
      assert_equal(32...48, records[4].byte_range)
      assert_equal(false, records[4].recoverable?)
    end
  end

  def test_file_input
    Tempfile.create(["isolate", ".csv"]) do |file|
      data = "a,b\n\"multi\nline\",2\nx,\"bad\"q\nc,d\n"
      file.binmode
      file.write(data)
      file.flush
      file.rewind

      rows = []
      events = []
      CSV.new(file, isolate_errors: true).each do |record|
        if bad?(record)
          events << record
        else
          rows << record
        end
      end
      assert_equal([["a", "b"], ["multi\nline", "2"], ["c", "d"]], rows)
      assert_equal(1, events.size)
      event = events.first
      assert_equal(3, event.lineno)
      assert_equal("x,\"bad\"q\n", event.raw)
      assert_byte_range(data, event)
    end
  end

  def test_inspect
    event = CSV::MalformedRecord.new(2, 4...10, "b,\"x\"y\n",
                                     "Illegal quoting", true)
    assert_match(/MalformedRecord/, event.inspect)
    assert_match(/line_number: 2/, event.inspect)
  end
end
