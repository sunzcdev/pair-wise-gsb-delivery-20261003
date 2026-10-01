# frozen_string_literal: true

require_relative "helper"

class TestCSVDialectSniffer < Test::Unit::TestCase
  # An IO-like source that behaves like a network stream or pipe:
  # it cannot seek or rewind, and it never returns more than
  # +max_read+ bytes per call, however many were requested.
  class NonSeekableStream
    attr_reader :bytes_served

    def initialize(data, max_read: 5)
      @data = data
      @pos = 0
      @max_read = max_read
      @bytes_served = 0
    end

    def readpartial(length)
      raise EOFError, "end of stream" if eof?
      serve([length, @max_read].min)
    end

    def read(length)
      return nil if eof?
      serve([length, @max_read].min)
    end

    def gets(separator, limit = nil)
      return nil if eof?
      rest = @data.byteslice(@pos..-1)
      if separator.nil?
        take = rest.bytesize
      else
        index = rest.index(separator)
        take = index ? index + separator.bytesize : rest.bytesize
      end
      take = limit if limit && limit < take
      take = @max_read if @max_read < take
      serve(take)
    end

    def eof?
      @pos >= @data.bytesize
    end
    alias_method :eof, :eof?

    def seek(*)
      raise "this stream cannot seek"
    end

    def rewind
      raise "this stream cannot rewind"
    end

    def pos=(value)
      raise "this stream cannot seek"
    end

    private

    def serve(length)
      chunk = @data.byteslice(@pos, length)
      @pos += chunk.bytesize
      @bytes_served += chunk.bytesize
      chunk
    end
  end

  def sniff(input, **options)
    CSV.sniff_dialect(input, **options)
  end

  def test_detects_semicolon_dialect_from_non_rewindable_stream
    data = "name;age;city\nAlice;30;Paris\nBob;25;Rome\n"
    stream = NonSeekableStream.new(data, max_read: 5)
    result = sniff(stream)
    assert do
      result.detected?
    end
    assert_equal(";", result.best.col_sep)
    assert_equal("\n", result.best.row_sep)
    assert_equal([3, 3, 3], result.best.column_counts)
  end

  def test_parses_from_the_first_record_after_detection
    data = "name;age;city\nAlice;30;Paris\nBob;25;Rome\n"
    stream = NonSeekableStream.new(data, max_read: 4)
    result = sniff(stream, sample_bytes: 17)
    rows = result.csv.read
    assert_equal([
                   ["name", "age", "city"],
                   ["Alice", "30", "Paris"],
                   ["Bob", "25", "Rome"],
                 ],
                 rows)
  end

  def test_sample_bytes_bound_is_respected
    data = "a;b\n1;2\n3;4\n5;6\n7;8\n9;10\n"
    stream = NonSeekableStream.new(data, max_read: 3)
    result = sniff(stream, sample_bytes: 7)
    assert_operator(stream.bytes_served, :<=, 7)
    assert_equal("a;b\n1;2", result.sample)
  end

  def test_first_line_alone_would_mislead
    # The first line is a comment containing a comma; every
    # following record is semicolon separated.  A sniffer that
    # only looks at the first line would pick ",".
    data = <<~CSV
      # generated, 2026-09-30
      name;age;city
      Alice;30;Paris
      Bob;25;Rome
    CSV
    result = sniff(NonSeekableStream.new(data, max_read: 6))
    assert do
      result.detected?
    end
    assert_equal(";", result.best.col_sep)
    assert_equal([["# generated, 2026-09-30"],
                  ["name", "age", "city"],
                  ["Alice", "30", "Paris"],
                  ["Bob", "25", "Rome"]],
                 result.csv.read)
  end

  def test_sample_ending_mid_field_is_truncated_not_malformed
    data = "a;b\n\"long;field\";x\n1;2\n"
    # The sample ends right in the middle of a quoted field.
    result = sniff(NonSeekableStream.new(data, max_read: 4),
                   sample_bytes: 12)
    assert_equal("a;b\n\"long;fi", result.sample)
    best = result.best
    assert_not_nil(best)
    assert_equal(";", best.col_sep)
    assert do
      best.viable?
    end
    assert do
      best.truncated?
    end
    assert do
      best.reasons.any? {|reason| reason.include?("middle of a record")}
    end
    # ... and the chosen dialect still parses the whole input,
    # including the record that was cut by the sample.
    assert_equal([["a", "b"], ["long;field", "x"], ["1", "2"]],
                 result.csv.read)
  end

  def test_quoted_multiline_fields_are_respected
    data = "id;note\n" \
           "1;\"line one\nline two; still quoted\"\n" \
           "2;plain\n"
    result = sniff(NonSeekableStream.new(data, max_read: 3))
    assert do
      result.detected?
    end
    assert_equal(";", result.best.col_sep)
    # The quoted line break must not be counted as a row separator:
    # there are exactly three records, each with two columns.
    assert_equal([2, 2, 2], result.best.column_counts)
    assert_equal([["id", "note"],
                  ["1", "line one\nline two; still quoted"],
                  ["2", "plain"]],
                 result.csv.read)
  end

  def test_ambiguity_is_reported_not_silently_resolved
    # Both "," and ";" produce two consistent columns here.
    data = "a,b;c\n1,2;3\n"
    result = sniff(data, col_seps: [",", ";"], quote_chars: ['"'])
    assert do
      result.ambiguous?
    end
    assert do
      not result.detected?
    end
    assert_nil(result.best)
    assert_nil(result.dialect)
    assert_equal([",", ";"].sort,
                 result.best_candidates.map(&:col_sep).sort)
    error = assert_raise(CSV::DialectSniffer::AmbiguousDialectError) do
      result.csv
    end
    assert_match(/equally well/, error.message)
    # The caller can still resolve the ambiguity explicitly.
    assert_equal([["a,b", "c"], ["1,2", "3"]],
                 result.csv(col_sep: ";").read)
  end

  def test_single_column_file_is_ambiguous_between_separators
    result = sniff("alpha\nbeta\ngamma\n",
                   col_seps: [",", ";"],
                   quote_chars: ['"'])
    assert do
      result.ambiguous?
    end
    assert_equal([",", ";"].sort,
                 result.best_candidates.map(&:col_sep).sort)
  end

  def test_crlf_row_separator_wins_over_lf
    data = "a;b\r\n1;2\r\n3;4\r\n"
    result = sniff(NonSeekableStream.new(data, max_read: 5))
    assert do
      result.detected?
    end
    assert_equal("\r\n", result.best.row_sep)
    lf = result.candidates.find {|c| c.row_sep == "\n"}
    assert do
      lf.reasons.any? {|reason| reason.include?("line-break")}
    end
    assert_equal([["a", "b"], ["1", "2"], ["3", "4"]],
                 result.csv.read)
  end

  def test_quote_char_undetermined_when_absent_from_sample
    result = sniff("a;b\n1;2\n")
    best = result.best
    assert_not_nil(best)
    assert_nil(best.quote_char)
    assert do
      not best.quote_char_determined?
    end
    assert do
      best.reasons.any? {|reason| reason.include?("quote_char is undetermined")}
    end
    # Parsing still works; the default quote character applies.
    assert_equal([["a", "b"], ["1", "2"]], result.csv.read)
  end

  def test_malformed_candidate_records_its_reason
    # The apostrophe in O'Neil rules out "'" as a quote character.
    data = "name;city\nO'Neil;Paris\n"
    result = sniff(data)
    assert do
      result.detected?
    end
    apostrophe = result.candidates.find {|c| c.quote_char == "'"}
    assert_not_nil(apostrophe)
    assert do
      not apostrophe.viable?
    end
    assert_match(/unquoted field/, apostrophe.malformed_reason)
    assert do
      apostrophe.reasons.any? {|reason| reason.start_with?("rejected:")}
    end
  end

  def test_no_viable_candidate
    # A character right after a closing quote contradicts the only
    # offered dialect.
    result = sniff(%Q{"a"x\n},
                   col_seps: [","], quote_chars: ['"'], row_seps: ["\n"])
    assert do
      not result.detected?
    end
    assert do
      not result.ambiguous?
    end
    assert_raise(CSV::DialectSniffer::NoViableDialectError) do
      result.csv
    end
  end

  def test_unclosed_quote_at_eof_is_malformed_not_truncated
    result = sniff("a;\"unclosed\n",
                   col_seps: [";"], quote_chars: ['"'], row_seps: ["\n"])
    candidate = result.candidates.first
    assert do
      not candidate.viable?
    end
    assert_match(/unclosed quoted field/, candidate.malformed_reason)
  end

  def test_string_input
    result = sniff("a\tb\n1\t2\n")
    assert do
      result.detected?
    end
    assert_equal("\t", result.best.col_sep)
    assert_equal([["a", "b"], ["1", "2"]], result.csv.read)
  end

  def test_csv_cannot_be_built_twice_from_one_result
    result = sniff(NonSeekableStream.new("a;b\n1;2\n"))
    result.csv
    assert_raise(CSV::DialectSniffer::Error) do
      result.csv
    end
  end

  def test_candidates_are_sorted_best_first_and_carry_reasons
    result = sniff("a;b;c\n1;2;3\n")
    best = result.candidates.first
    assert_equal(";", best.col_sep)
    assert do
      best.reasons.any? {|reason| reason.include?("3 column(s)")}
    end
    scores = result.candidates.select(&:viable?).map(&:score)
    assert_equal(scores.sort.reverse, scores)
  end
end
