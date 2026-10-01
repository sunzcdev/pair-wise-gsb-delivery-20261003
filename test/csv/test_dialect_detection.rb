require_relative "helper"

class TestCSVDialect < Test::Unit::TestCase
  include CSVHelper

  #
  # A stream that can neither seek nor rewind, and that returns at most
  # +max_read+ bytes per read(), even when more are requested. It also
  # caps gets() the same way, like an OS pipe delivering small chunks.
  #
  class ForwardIO
    def initialize(data, max_read: 7)
      @buffer = data
      @max_read = max_read
      @closed = false
    end

    def read(length = nil)
      length = @max_read if length.nil? || length > @max_read
      chunk = @buffer.byteslice(0, length)
      @buffer = @buffer.byteslice(length..-1) || +""
      return nil if chunk.nil? || chunk.empty?
      chunk
    end

    def gets(separator = $/, limit = nil)
      out = +""
      limit = @max_read if limit.nil? || limit > @max_read
      until out.bytesize >= limit
        char = read(1)
        break if char.nil?
        out << char
        break if separator && out.end_with?(separator)
      end
      out.empty? ? nil : out
    end

    def eof?
      @buffer.empty?
    end

    def external_encoding
      Encoding::UTF_8
    end

    def seek(*)
      raise "seek must not be called on a forward-only stream"
    end

    def rewind
      raise "rewind must not be called on a forward-only stream"
    end

    def pos
      raise "pos must not be called on a forward-only stream"
    end

    def close
      @closed = true
    end

    def closed?
      @closed
    end

    attr_reader :buffer
  end

  # Even more restrictive: only read() is available.
  class ReadOnlyForwardIO < ForwardIO
    undef_method :gets
  end

  # read() takes no length argument (returns a stream sized chunk).
  class NoArgReadIO
    def initialize(data, max_read: 2)
      @buffer = data
      @max_read = max_read
    end

    def read
      chunk = @buffer.byteslice(0, @max_read)
      @buffer = @buffer.byteslice(@max_read..-1) || +""
      chunk.empty? ? nil : chunk
    end

    def eof?
      @buffer.empty?
    end

    def external_encoding
      Encoding::UTF_8
    end
  end

  def detect(data, **options)
    CSV.detect_dialect(data, **options)
  end

  def dialect_of(data, **options)
    detection = detect(data, **options)
    assert_false(detection.ambiguous?,
                 "expected a unique dialect but #{detection.ambiguous_dimensions.inspect} were ambiguous")
    detection.best
  end

  def assert_dialect(expected, data, **options)
    actual = dialect_of(data, **options)
    assert_equal(expected,
                 [actual.col_sep, actual.row_sep, actual.quote_char])
  end

  def test_detects_comma
    assert_dialect([",", "\n", '"'],
                   "a,b,c\n1,2,3\n4,5,6\n")
  end

  def test_detects_semicolon
    assert_dialect([";", "\n", '"'],
                   "a;b;c\n1;2;3\n4;5;6\n")
  end

  def test_detects_tab
    assert_dialect(["\t", "\n", '"'],
                   "a\tb\tc\n1\t2\t3\n4\t5\t6\n")
  end

  def test_detects_crlf
    assert_dialect([",", "\r\n", '"'],
                   "a,b\r\nc,d\r\ne,f\r\n")
  end

  def test_detects_cr
    assert_dialect([",", "\r", '"'],
                   "a,b\rc,d\re,f\r")
  end

  def test_no_line_break_leaves_row_sep_undetermined
    detection = detect("a,b,c")
    assert_false(detection.ambiguous?)
    assert_nil(detection.best.row_sep)
    options = detection.best.to_csv_options
    assert_equal(:auto, options[:row_sep])
  end

  def test_separator_inside_quotes_is_ignored
    data = "name;city\n\"smith, john\";\"paris, france\"\ndoe;berlin\n"
    assert_dialect([";", "\n", '"'], data)
  end

  def test_tab_inside_quotes_is_ignored
    data = "name\tcity\n\"a\tb\"\tx\nc\td\n"
    assert_dialect(["\t", "\n", '"'], data)
  end

  def test_row_separator_inside_quoted_field
    data = "a,\"line one\nline two\",c\nx,y,z\n"
    assert_dialect([",", "\n", '"'], data)
    detection = detect(data)
    quoted_candidate = detection.candidates.find do |candidate|
      candidate[:dialect].quote_char == '"'
    end
    assert(quoted_candidate[:reasons].any? {|reason|
             reason.include?("quoted field")},
           "expected quoted field evidence: #{quoted_candidate[:reasons].inspect}")
  end

  def test_first_line_alone_would_guess_comma
    # The first physical line has more commas (inside quoted fields)
    # than semicolons, so a first-line frequency or split heuristic picks
    # ",". Looking at the whole quote aware sample reveals ";" is the
    # real column separator.
    data = "\"last, first\";\"city, country\"\n" \
           "\"smith, john\";\"paris, france\"\n" \
           "\"doe, jane\";\"berlin, germany\"\n"
    assert_operator(data.lines.first.count(","),
                    :>, data.lines.first.count(";"))
    assert_dialect([";", "\n", '"'], data)
  end

  def test_sample_ending_inside_unquoted_field_is_not_malformed
    data = "a,b,c\nd,e,f" # whole stream, no trailing row sep
    detection = detect(data)
    assert_false(detection.truncated)
    assert_equal([], detection.rejected.map {|r| r[:reason]}.grep(/Unclosed/))
    assert_dialect([",", "\n", '"'], data)
  end

  def test_sample_truncated_inside_quoted_field_is_not_malformed
    full = "a,b,\"still going\",x\ny,z,w,v\n"
    cut = full.byteslice(0, 9) # ends as 'a,b,"stil' (dangling quote)
    assert_equal("a,b,\"stil", cut)
    detection = CSV.detect_dialect(ForwardIO.new(full, max_read: 4),
                                   sample_bytes: cut.bytesize)
    assert(detection.truncated)
    quoted = detection.candidates.find do |candidate|
      candidate[:dialect].quote_char == '"'
    end
    assert_not_nil(quoted)
    assert(quoted[:reasons].any? {|reason|
             reason.include?("inside a quoted field")},
           quoted[:reasons].inspect)
    assert_false(detection.rejected.any? {|rejected|
                   rejected[:reason] == "Unclosed quoted field"})
  end

  def test_truncated_multiline_quote_resolves_with_more_data
    full = "name;note\n\"a; b\";x\n\"c; d\";y\n"
    # Cut inside the first quoted field of the second record.
    cut = full.byteslice(0, full.index('"', full.index("\n") + 1) + 3)
    detection = CSV.detect_dialect(ForwardIO.new(full, max_read: 4),
                                   sample_bytes: cut.bytesize)
    assert(detection.truncated)
    assert_false(detection.ambiguous?,
                 detection.ambiguous_dimensions.inspect)
    assert_equal(";", detection.best.col_sep)
  end

  def test_unclosed_quote_in_complete_sample_is_rejected
    detection = detect("a,\"b,c")
    assert_false(detection.truncated)
    assert(detection.rejected.any? do |rejected|
      rejected[:settings][:quote_char] == '"' &&
        rejected[:reason] == "Unclosed quoted field"
    end)
    # The unquoted interpretation still parses, so detection succeeds.
    assert_false(detection.ambiguous?)
    assert_nil(detection.best.quote_char)
  end

  def test_quotes_in_unquoted_field_are_rejected
    data = "a,\"b,c\nd,e,f\ng,h,i\n"
    detection = detect(data)
    assert(detection.rejected.any? do |rejected|
      rejected[:settings][:quote_char] == '"' &&
        rejected[:reason].include?("quoted field")
    end)
  end

  def test_equally_valid_separators_are_ambiguous
    # Every line has exactly one of the candidate separators, so both
    # dialects produce one 2-column and one 1-column row: a genuine tie.
    detection = detect("a;b\nc,d\n")
    assert(detection.ambiguous?)
    assert_include(detection.ambiguous_dimensions, "col_sep")
    assert_nil(detection.best)
    col_seps = detection.candidates
                 .map {|candidate| candidate[:dialect].col_sep}
                 .uniq
    assert_include(col_seps, ",")
    assert_include(col_seps, ";")
  end

  def test_ambiguity_does_not_pick_first_candidate
    # Same data, reversed candidate order: ambiguity must be reported
    # either way instead of selecting whichever came first.
    data = "a;b\nc,d\n"
    one = detect(data, col_seps: [",", ";"])
    two = detect(data, col_seps: [";", ","])
    assert(one.ambiguous?)
    assert(two.ambiguous?)
    assert_nil(one.best)
    assert_nil(two.best)
  end

  def test_no_structure_is_ambiguous_across_col_seps
    detection = detect("hello world")
    assert(detection.ambiguous?)
    assert_include(detection.ambiguous_dimensions, "col_sep")
    assert_nil(detection.best)
  end

  def test_empty_sample_is_ambiguous
    detection = detect("")
    assert(detection.ambiguous?)
    assert_nil(detection.best)
    assert_not_nil(detection.candidates)
  end

  def test_every_candidate_carries_reasons
    detection = detect("a,b\nc,d\n")
    assert_not_empty(detection.candidates)
    detection.candidates.each do |candidate|
      assert_kind_of(CSV::Dialect, candidate[:dialect])
      assert_kind_of(Array, candidate[:score])
      assert_kind_of(Array, candidate[:reasons])
      assert_not_empty(candidate[:reasons])
      candidate[:reasons].each {|reason| assert_kind_of(String, reason)}
    end
  end

  def test_rejected_candidates_carry_reasons
    detection = detect("a,\"b,c\nd,e,f\n")
    assert_not_empty(detection.rejected)
    detection.rejected.each do |rejected|
      assert_kind_of(Hash, rejected[:settings])
      assert_kind_of(String, rejected[:reason])
    end
  end

  def test_sample_bytes_is_respected
    data = "x" * 100
    detection = detect(data, sample_bytes: 32)
    assert_equal(32, detection.sample.bytesize)
    assert(detection.truncated)
  end

  def test_sample_bytes_zero
    detection = detect("a,b,c\n", sample_bytes: 0)
    assert_equal(0, detection.sample.bytesize)
    assert(detection.ambiguous?)
  end

  def test_caller_supports_custom_candidates
    assert_dialect(["|", "\n", nil],
                   "a|b|c\n1|2|3\n",
                   col_seps: ["|", ","],
                   quote_chars: [nil])
  end

  def test_explicit_row_sep_candidates
    detection = detect("a,b\r\nc,d\r\n", row_seps: ["\r\n", "\n"])
    assert_false(detection.ambiguous?)
    assert_equal("\r\n", detection.best.row_sep)
  end

  def test_invalid_candidate_arguments
    assert_raise(ArgumentError) { detect("x", col_seps: []) }
    assert_raise(ArgumentError) { detect("x", col_seps: [""]) }
    assert_raise(ArgumentError) { detect("x", col_seps: [1]) }
    assert_raise(ArgumentError) { detect("x", quote_chars: []) }
    assert_raise(ArgumentError) { detect("x", row_seps: []) }
    assert_raise(ArgumentError) { detect("x", row_seps: [1]) }
    assert_raise(ArgumentError) { detect("x", sample_bytes: -1) }
  end

  # ----- non-rewindable IO ------------------------------------------------

  def test_detect_reads_forward_io_once
    data = "a,b,c\n1,2,3\n4,5,6\n"
    io = ForwardIO.new(data, max_read: 3)
    detection = CSV.detect_dialect(io)
    assert_false(detection.ambiguous?)
    assert_equal([",", "\n", '"'],
                 [detection.best.col_sep,
                  detection.best.row_sep,
                  detection.best.quote_char])
  end

  def test_parse_detected_from_forward_io_starts_at_first_record
    data = "name;city\n\"smith, john\";\"paris, france\"\ndoe;berlin\n"
    io = ForwardIO.new(data, max_read: 3)
    rows = CSV.parse_detected(io, sample_bytes: 40)
    assert_equal([%w[name city],
                  ["smith, john", "paris, france"],
                  %w[doe berlin]], rows)
  end

  def test_parse_detected_from_forward_io_small_sample
    data = "name;city\n\"smith, john\";\"paris, france\"\ndoe;berlin\n"
    # Sample ends inside the first quoted field.
    io = ForwardIO.new(data, max_read: 4)
    rows = CSV.parse_detected(io, sample_bytes: 14)
    assert_equal([%w[name city],
                  ["smith, john", "paris, france"],
                  %w[doe berlin]], rows)
  end

  def test_parse_detected_forward_io_with_headers_and_block
    data = "name;city\n\"smith, john\";\"paris, france\"\ndoe;berlin\n"
    io = ForwardIO.new(data, max_read: 5)
    collected = []
    CSV.parse_detected(io, sample_bytes: 30, headers: true) do |csv|
      csv.each {|row| collected << row.to_h}
    end
    assert_equal([
                   {"name" => "smith, john", "city" => "paris, france"},
                   {"name" => "doe", "city" => "berlin"},
                 ], collected)
  end

  def test_parse_detected_works_with_read_only_stream
    data = "a,b,c\n1,2,3\n4,5,6\n"
    io = ReadOnlyForwardIO.new(data, max_read: 3)
    rows = CSV.parse_detected(io)
    assert_equal([%w[a b c], %w[1 2 3], %w[4 5 6]], rows)
  end

  def test_parse_detected_does_not_seek_forward_io
    data = "a,b,c\n1,2,3\n4,5,6\n"
    io = ForwardIO.new(data, max_read: 2)
    # Would raise if the implementation tried to rewind/seek.
    rows = CSV.parse_detected(io, sample_bytes: 10)
    assert_equal([%w[a b c], %w[1 2 3], %w[4 5 6]], rows)
  end

  def test_parse_detected_first_line_misleading_from_forward_io
    data = "\"last, first\";\"city, country\"\n" \
           "\"smith, john\";\"paris, france\"\n" \
           "\"doe, jane\";\"berlin, germany\"\n"
    io = ForwardIO.new(data, max_read: 4)
    rows = CSV.parse_detected(io, sample_bytes: data.bytesize)
    assert_equal([["last, first", "city, country"],
                  ["smith, john", "paris, france"],
                  ["doe, jane", "berlin, germany"]], rows)
  end

  def test_parse_detected_raises_on_ambiguity
    io = ForwardIO.new("a;b\nc,d\n", max_read: 2)
    error = assert_raise(CSV::AmbiguousDialectError) do
      CSV.parse_detected(io, sample_bytes: 10)
    end
    assert_kind_of(CSV::DialectDetection, error.detection)
    assert_include(error.detection.ambiguous_dimensions, "col_sep")
  end

  def test_parse_detected_from_string
    rows = CSV.parse_detected("a;b;c\n1;2;3\n")
    assert_equal([%w[a b c], %w[1 2 3]], rows)
  end

  def test_multibyte_content_detection_and_replay
    data = "名前;都市\n\"田中, 太郎\";東京\n佐藤;大阪\n"
    detection = detect(data)
    assert_equal(";", detection.best.col_sep)
    rows = CSV.parse_detected(ForwardIO.new(data, max_read: 3))
    assert_equal([%w[名前 都市],
                  ["田中, 太郎", "東京"],
                  %w[佐藤 大阪]], rows)
  end

  def test_multibyte_boundary_split_is_replayed_intact
    data = "あ,い,う\nえ,お,か\n"
    # Every possible byte offset, reading one byte at a time: the byte cut
    # in the middle of a character must be re-attached during replay.
    resolvable = 0
    0.upto(data.bytesize) do |sample_bytes|
      begin
        rows = CSV.parse_detected(ForwardIO.new(data, max_read: 1),
                                  sample_bytes: sample_bytes)
        assert_equal([%w[あ い う], %w[え お か]], rows)
        resolvable += 1
      rescue CSV::AmbiguousDialectError
        # Too little structure yet; expected for very short samples.
      end
    end
    assert_operator(resolvable, :>, 0)
  end

  def test_stream_without_read_length_argument
    rows = CSV.parse_detected(NoArgReadIO.new("a,b\nc,d\n"),
                              sample_bytes: 4)
    assert_equal([%w[a b], %w[c d]], rows)
  end

  def test_multi_character_column_separator_cut_by_sample
    data = "a<|>b<|>c\nd<|>e<|>f\n"
    [6, 7, 8].each do |sample_bytes|
      rows = CSV.parse_detected(ForwardIO.new(data, max_read: 3),
                                sample_bytes: sample_bytes,
                                col_seps: ["<|>", ","])
      assert_equal([%w[a b c], %w[d e f]], rows)
    end
  end

  def test_crlf_split_between_cr_and_lf_at_sample_end
    data = "a,b\r\nc,d\r\ne,f\r\n"
    # 5 bytes is "a,b\r\n"; also exercise a cut exactly between CR and LF.
    [4, 5, 6].each do |sample_bytes|
      rows = CSV.parse_detected(ForwardIO.new(data, max_read: 2),
                                sample_bytes: sample_bytes)
      assert_equal([%w[a b], %w[c d], %w[e f]], rows)
    end
  end

  def test_block_form_closes_forward_io
    data = "a,b\nc,d\n"
    io = ForwardIO.new(data, max_read: 2)
    CSV.parse_detected(io, sample_bytes: 3) { |csv| csv.to_a }
    assert(io.closed?)
  end
end
