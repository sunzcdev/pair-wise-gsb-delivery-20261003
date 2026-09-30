# frozen_string_literal: false

require_relative "../helper"

class TestCSVParseIsolateErrors < Test::Unit::TestCase
  include CSVHelper

  # Input used for recovery tests.  It contains a *legal* multiline quoted
  # field (rows 1 and 2), a malformed record (row 3) whose quoting is still
  # balanced, and a healthy record after it (row 4).
  RECOVERABLE_DATA = "a,b\n" \
                     "\"multi\nline\",x\n" \
                     "\"closed\"tail,y\n" \
                     "zz,ww\n"

  def test_default_strict_mode_raises_and_does_not_isolate
    error = assert_raise(CSV::MalformedCSVError) do
      CSV.parse(RECOVERABLE_DATA)
    end
    assert_equal("Any value after quoted field isn't allowed in line 3.",
                 error.message)
    assert_equal(false, CSV.new("").isolate_errors?)
  end

  def test_isolate_yields_records_and_structured_event_in_order
    events = []
    CSV.new(RECOVERABLE_DATA, isolate_errors: true).each do |record|
      events << record
    end
    assert_equal(4, events.size)
    assert_equal(["a", "b"], events[0])
    assert_equal(["multi\nline", "x"], events[1])

    event = events[2]
    assert_instance_of(CSV::MalformedRecord, event)
    assert_equal(3, event.lineno)
    assert_equal("\"closed\"tail,y\n", event.raw)
    assert_equal([19, 34], event.byte_range)
    assert_equal("Any value after quoted field isn't allowed in line 3.",
                 event.error.message)
    assert_kind_of(CSV::MalformedCSVError, event.error)
    assert_predicate(event, :malformed?)

    # The record following the bad one is not lost and numbering continues.
    assert_equal(["zz", "ww"], events[3])
  end

  def test_byte_range_slices_the_exact_original_bytes
    CSV.new(RECOVERABLE_DATA, isolate_errors: true).each do |record|
      next unless record.is_a?(CSV::MalformedRecord)
      start_byte, end_byte = record.byte_range
      assert_equal(record.raw,
                   RECOVERABLE_DATA.byteslice(start_byte,
                                              end_byte - start_byte))
    end
  end

  def test_lineno_is_continuous_after_recovery
    linenos = []
    CSV.new(RECOVERABLE_DATA, isolate_errors: true).each do |record|
      linenos << (record.is_a?(CSV::MalformedRecord) ? record.lineno : nil)
    end
    assert_equal([nil, nil, 3, nil], linenos)
  end

  def test_multiple_malformed_records_are_each_isolated
    data = "ok,1\nmid\"quote,2\nok,3\n\"q\"tail,4\nok,5\n"
    collected = []
    CSV.new(data, isolate_errors: true).each do |record|
      collected << record
    end
    events = collected.grep(CSV::MalformedRecord)
    assert_equal(2, events.size)
    assert_equal(["ok", "1"], collected[0])
    assert_equal(2, events[0].lineno)
    assert_equal("mid\"quote,2\n", events[0].raw)
    assert_equal(["ok", "3"], collected[2])
    assert_equal(4, events[1].lineno)
    assert_equal("\"q\"tail,4\n", events[1].raw)
    assert_equal(["ok", "5"], collected[4])
    # every emitted raw range is exact
    events.each do |event|
      s, e = event.byte_range
      assert_equal(event.raw, data.byteslice(s, e - s))
    end
  end

  def test_illegal_quoting_with_a_mid_field_quote_is_recoverable
    # A stray quote inside an unquoted field never opens a quoted region, so
    # the following row separator is still a confirmed out-of-quote boundary.
    data = "head\nv\"w\nnext\n"
    collected = []
    CSV.new(data, isolate_errors: true).each {|r| collected << r}
    event = collected.grep(CSV::MalformedRecord).first
    assert_not_nil(event)
    assert_equal(2, event.lineno)
    assert_equal("v\"w\n", event.raw)
    assert_equal("Illegal quoting in line 2.", event.error.message)
    assert_equal(["next"], collected.last)
  end

  def test_unclosed_quote_reports_unrecoverable_and_stops
    data = "a,b\n\"never closes\nc,d\ne,f\n"
    yielded = []
    error = assert_raise(CSV::UnrecoverableCSVError) do
      CSV.new(data, isolate_errors: true).each {|r| yielded << r}
    end
    # Records before the failure are already delivered.
    assert_equal([["a", "b"]], yielded)
    # Parsing stops: the healthy-looking rows inside the open quote must not
    # be emitted, and the raw tail is preserved.
    assert_equal("\"never closes\nc,d\ne,f\n", error.raw)
    assert_equal([4, error.byte_range[1]], error.byte_range)
    s, e = error.byte_range
    assert_equal(data.byteslice(s, e - s), error.raw)
    assert_equal(2, error.lineno)
    assert_equal("Unclosed quoted field in line 2.", error.message)
    assert_kind_of(CSV::MalformedCSVError, error)
  end

  def test_single_stray_quote_at_field_start_without_closure_is_unrecoverable
    data = "a,b\n\"quote never closes,zz\nww\n"
    yielded = []
    error = assert_raise(CSV::UnrecoverableCSVError) do
      CSV.new(data, isolate_errors: true).each {|r| yielded << r}
    end
    assert_equal([["a", "b"]], yielded)
    # The quote opens at field start and never closes: everything after is
    # consumed as the unclosed field and no boundary can be confirmed.
    assert_equal("\"quote never closes,zz\nww\n", error.raw)
  end

  def test_unrecoverable_error_is_a_malformed_csv_error
    data = "x\n\"oops"
    error = assert_raise(CSV::UnrecoverableCSVError) do
      CSV.new(data, isolate_errors: true).each {}
    end
    assert_kind_of(CSV::MalformedCSVError, error)
  end

  def test_liberal_parsing_does_not_produce_isolation_events
    # The same input parses under liberal_parsing and must yield ordinary
    # records only; tolerant results are never reported as isolated events.
    collected = []
    CSV.new(RECOVERABLE_DATA, liberal_parsing: true).each {|r| collected << r}
    assert_equal(4, collected.size)
    assert(collected.none? {|r| r.is_a?(CSV::MalformedRecord)})
    assert_equal(["\"closed\"tail", "y"], collected[2])
  end

  def test_liberal_parsing_and_isolate_errors_coexist
    # liberal_parsing accepts the post-quote content, so no event is raised.
    collected = []
    CSV.new(RECOVERABLE_DATA,
            liberal_parsing: true,
            isolate_errors: true).each {|r| collected << r}
    assert(collected.none? {|r| r.is_a?(CSV::MalformedRecord)})
  end

  def test_crlf_row_separator
    data = "1,OK\r\n\"2\",\"\"NOT\" OK\"\r\n3,OK\r\n"
    collected = []
    CSV.new(data, isolate_errors: true).each {|r| collected << r}
    event = collected.grep(CSV::MalformedRecord).first
    assert_not_nil(event)
    assert_equal(2, event.lineno)
    assert_equal("\"2\",\"\"NOT\" OK\"\r\n", event.raw)
    assert_equal(["3", "OK"], collected.last)
    s, e = event.byte_range
    assert_equal(event.raw, data.byteslice(s, e - s))
  end

  def test_multibyte_offsets_are_byte_based
    data = "さん,b\n\"a\"z,y\nlast,x\n"
    event = nil
    CSV.new(data, isolate_errors: true).each do |record|
      event = record if record.is_a?(CSV::MalformedRecord)
    end
    assert_not_nil(event)
    s, e = event.byte_range
    assert_equal(event.raw, data.byteslice(s, e - s))
    # The first record contains a multibyte character, so the bad record's
    # start offset is counted in bytes (not characters).
    assert_equal("さん,b\n".bytesize, s)
  end

  def test_clean_file_yields_no_events
    data = "a,b\n\"multi\nline\",x\nc,d\n"
    collected = []
    CSV.new(data, isolate_errors: true).each {|r| collected << r}
    assert_equal([["a", "b"], ["multi\nline", "x"], ["c", "d"]],
                 collected)
    assert(collected.none? {|r| r.is_a?(CSV::MalformedRecord)})
  end

  def test_error_event_not_converted_by_field_converters
    data = "\"x\"tail,y\n1,2\n"
    collected = []
    CSV.new(data,
            isolate_errors: true,
            converters: :integer).each {|r| collected << r}
    event = collected.grep(CSV::MalformedRecord).first
    assert_not_nil(event)
    assert_equal("\"x\"tail,y\n", event.raw)
    assert_equal([1, 2], collected.last)
  end

  def test_enumerator_interface
    enum = CSV.new(RECOVERABLE_DATA, isolate_errors: true).each
    records = enum.to_a
    assert(records.any? {|r| r.is_a?(CSV::MalformedRecord)})
    assert_equal(["zz", "ww"], records.last)
  end

  def test_foreach_style_with_string_input
    events = CSV.parse(RECOVERABLE_DATA, isolate_errors: true)
    assert_equal(4, events.size)
    assert(events.any? {|r| r.is_a?(CSV::MalformedRecord)})
  end

  def test_malformed_last_record_without_terminator_is_recoverable
    data = "ok\n\"a\"tail"
    collected = []
    CSV.new(data, isolate_errors: true).each {|r| collected << r}
    event = collected.grep(CSV::MalformedRecord).first
    assert_not_nil(event)
    assert_equal("\"a\"tail", event.raw)
    assert_equal([3, 10], event.byte_range)
  end

  def test_recovery_before_unterminated_final_record
    data = "ok\n\"a\"tail\nlast"
    collected = []
    CSV.new(data, isolate_errors: true).each {|r| collected << r}
    assert_equal(3, collected.size)
    assert_instance_of(CSV::MalformedRecord, collected[1])
    assert_equal(["last"], collected.last)
  end

  def test_custom_column_separator_ranges_are_exact
    data = "a;b\n\"a\"tail;x\nz;w\n"
    events = []
    CSV.new(data, col_sep: ";", isolate_errors: true).each {|r| events << r}
    event = events.grep(CSV::MalformedRecord).first
    s, e = event.byte_range
    assert_equal(event.raw, data.byteslice(s, e - s))
    assert_equal(["z", "w"], events.last)
  end

  def test_custom_row_separator_ranges_are_exact
    data = "a|\"a\"tail|last|"
    events = []
    CSV.new(data, row_sep: "|", isolate_errors: true).each {|r| events << r}
    event = events.grep(CSV::MalformedRecord).first
    s, e = event.byte_range
    assert_equal(event.raw, data.byteslice(s, e - s))
    assert_equal(["last"], events.last)
  end

  def test_multi_character_column_separator
    data = "a<=>b\n\"a\"tail<=>x\nz<=>w\n"
    events = []
    CSV.new(data, col_sep: "<=>", isolate_errors: true).each {|r| events << r}
    event = events.grep(CSV::MalformedRecord).first
    s, e = event.byte_range
    assert_equal(event.raw, data.byteslice(s, e - s))
    assert_equal(["z", "w"], events.last)
  end

  def test_blank_lines_around_a_bad_record_are_preserved
    data = "\n\"a\"tail\n\nlast\n"
    collected = []
    CSV.new(data, isolate_errors: true).each {|r| collected << r}
    assert_equal([], collected[0])
    assert_instance_of(CSV::MalformedRecord, collected[1])
    assert_equal([], collected[2])
    assert_equal(["last"], collected[3])
    s, e = collected[1].byte_range
    assert_equal(collected[1].raw, data.byteslice(s, e - s))
  end

  def test_with_headers_event_interleaved_with_rows
    data = "Name,Val\nok,1\n\"q\"tail,2\nlast,3\n"
    collected = []
    CSV.new(data, headers: true, isolate_errors: true).each {|r| collected << r}
    assert_equal(3, collected.size)
    assert_equal("ok", collected[0]["Name"])
    assert_instance_of(CSV::MalformedRecord, collected[1])
    assert_equal(3, collected[1].lineno)
    assert_equal("last", collected[2]["Name"])
  end

  def test_oversized_field_is_isolated_and_parsing_continues
    data = "a\nbbb\nc\n"
    collected = []
    CSV.new(data, max_field_size: 2, isolate_errors: true).each do |r|
      collected << r
    end
    assert_equal(3, collected.size)
    event = collected[1]
    assert_instance_of(CSV::MalformedRecord, event)
    assert_equal("bbb\n", event.raw)
    assert_equal("Field size exceeded: 3 > 2 in line 2.",
                 event.error.message)
    assert_equal(["c"], collected.last)
  end

  def test_oversized_field_strict_mode_still_raises
    assert_raise(CSV::MalformedCSVError) do
      CSV.parse("a\nbbb\nc\n", max_field_size: 2)
    end
  end
end

class TestCSVParseIsolateErrorsChunked < Test::Unit::TestCase
  include CSVHelper

  RECOVERABLE_DATA = "a,b\n" \
                     "\"multi\nline\",x\n" \
                     "\"closed\"tail,y\n" \
                     "zz,ww\n"

  def run_chunked(data, chunk_size)
    with_chunk_size(chunk_size.to_s) do
      begin
        ENV["CSV_PARSER_SCANNER_TEST"] = "yes"
        collected = []
        begin
          CSV.new(data, isolate_errors: true).each {|r| collected << r}
        rescue CSV::UnrecoverableCSVError => error
          collected << error
        end
        collected
      ensure
        ENV.delete("CSV_PARSER_SCANNER_TEST")
      end
    end
  end

  def test_recoverable_byte_ranges_are_exact_for_all_chunk_sizes
    [1, 2, 3, 5, 64].each do |chunk_size|
      collected = run_chunked(RECOVERABLE_DATA, chunk_size)
      events = collected.grep(CSV::MalformedRecord)
      assert_equal(1, events.size, "chunk_size=#{chunk_size}")
      event = events.first
      s, e = event.byte_range
      assert_equal(event.raw,
                   RECOVERABLE_DATA.byteslice(s, e - s),
                   "chunk_size=#{chunk_size}")
      assert_equal(["a", "b"], collected[0], "chunk_size=#{chunk_size}")
      assert_equal(["multi\nline", "x"], collected[1],
                   "chunk_size=#{chunk_size}")
      assert_equal(["zz", "ww"], collected.last, "chunk_size=#{chunk_size}")
    end
  end

  def test_unrecoverable_raw_range_is_exact_for_all_chunk_sizes
    data = "a,b\n\"never closes\nc,d\ne,f\n"
    [1, 2, 3, 5, 64].each do |chunk_size|
      collected = run_chunked(data, chunk_size)
      error = collected.last
      assert_kind_of(CSV::UnrecoverableCSVError, error,
                     "chunk_size=#{chunk_size}")
      s, e = error.byte_range
      assert_equal(error.raw, data.byteslice(s, e - s),
                   "chunk_size=#{chunk_size}")
    end
  end
end
