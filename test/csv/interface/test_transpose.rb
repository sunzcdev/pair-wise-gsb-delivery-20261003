# frozen_string_literal: true

require "tmpdir"

require_relative "../helper"

class TestCSVTranspose < Test::Unit::TestCase
  include CSVHelper

  def setup
    @tmpdir = Dir.mktmpdir("csv-transpose-test-")
  end

  def teardown
    FileUtils.remove_entry_secure(@tmpdir) if File.exist?(@tmpdir)
  end

  def transpose(input, output = nil, **options)
    options = {tmpdir: @tmpdir}.merge(options)
    if output
      CSV.transpose(input, output, **options)
    else
      CSV.transpose(input, **options)
    end
  end

  def transpose_matrix(rows, **options)
    input = rows.map { |row| CSV.generate_line(row) }.join
    CSV.parse(transpose(input, **options))
  end

  # --- Basic transposition ------------------------------------------------

  def test_basic
    assert_equal("a,1\nb,2\nc,3\n",
                 transpose("a,b,c\n1,2,3\n"))
  end

  def test_matrix
    rows = [["a", "b", "c"], ["1", "2", "3"], ["x", "y", "z"]]
    assert_equal(rows.transpose, transpose_matrix(rows))
  end

  def test_single_row_becomes_single_fields
    assert_equal("only\n", transpose("only\n"))
  end

  def test_single_column
    assert_equal("a,b,c\n", transpose("a\nb\nc\n"))
  end

  def test_empty_input
    assert_equal("", transpose(""))
  end

  def test_blank_row
    # Blank rows parse as zero-field records: a zero-column matrix
    # transposes to a zero-row result.
    assert_equal("", transpose("\n"))
    assert_equal("", transpose("\n\n"))
  end

  # --- Quoting, newlines and empty fields ---------------------------------

  def test_quotes_commas_newlines_and_empty_fields_survive
    rows = [
      ["a,b", %q{he said "hi"}, "", "line1\nline2"],
      ["1", "2", "3", "4"],
    ]
    input = rows.map { |row| CSV.generate_line(row) }.join
    output = transpose(input, ragged_rows: "")
    parsed = CSV.parse(output).map { |r| r.map { |f| f.nil? ? "" : f } }
    normalized = rows.map { |r| r.map { |f| f.nil? ? "" : f } }
    assert_equal(normalized.transpose, parsed)
    # Empty fields are emitted as quoted empty fields.
    assert_include(output, %Q{""})
  end

  def test_output_quoting_is_exact
    input = "a,\"b,c\",,\"x\"\"y\"\n1,2,3,4\n"
    output = transpose(input, ragged_rows: "")
    assert_equal(<<~CSV, output)
      a,1
      "b,c",2
      ,3
      "x""y",4
    CSV
  end

  def test_crlf_input
    rows = [["a", "b"], ["1", "2"]]
    input = "a,b\r\n1,2\r\n"
    assert_equal(rows.transpose, CSV.parse(transpose(input)))
  end

  # --- Ragged rows --------------------------------------------------------

  def test_ragged_rows_raise_by_default
    error = assert_raise(CSV::RaggedRowError) do
      transpose("a,b,c\n1,2\n")
    end
    assert_match(/row 2/, error.message)
  end

  def test_ragged_rows_raise_on_wider_row
    assert_raise(CSV::RaggedRowError) do
      transpose("a,b\n1,2,3\n")
    end
  end

  def test_ragged_rows_pad_short_with_empty_string
    output = transpose("a,b,c\n1,2\n", ragged_rows: "")
    # The explicit empty pad value is written as a quoted empty field.
    assert_equal("a,1\nb,2\nc,\"\"\n", output)
  end

  def test_ragged_rows_pad_with_nil
    output = transpose("a,b\n1,2\nx,y,z\n", ragged_rows: nil)
    assert_equal([["a", "1", "x"],
                  ["b", "2", "y"],
                  [nil, nil, "z"]],
                 CSV.parse(output))
  end

  def test_ragged_rows_pad_with_explicit_value
    input = "a,b\n1,2\nx,y,z\np,q,r,s\n"
    expected = [
      ["a", "1", "x", "p"],
      ["b", "2", "y", "q"],
      ["PAD", "PAD", "z", "r"],
      ["PAD", "PAD", "PAD", "s"],
    ]
    output = transpose(input, ragged_rows: "PAD")
    got = CSV.parse(output).map { |r| r.map { |f| f.nil? ? "PAD" : f } }
    assert_equal(expected, got)
  end

  # --- Headers are never inferred -----------------------------------------

  def test_first_record_is_data_not_header
    output = transpose("name,age\nalice,30\n")
    assert_equal("name,alice\nage,30\n", output)
  end

  def test_header_option_is_rejected
    assert_raise(ArgumentError) do
      transpose("a,b\n1,2\n", in_headers: true)
    end
    assert_raise(ArgumentError) do
      transpose("a,b\n1,2\n", headers: true)
    end
  end

  def test_instance_transpose_rejects_header_source
    csv = CSV.new("a,b\n1,2\n", headers: true)
    assert_raise(ArgumentError) { csv.transpose }
  end

  # --- Tiny budgets and many files ----------------------------------------

  def test_wider_than_open_file_limit
    rows = Array.new(4) { |r| Array.new(9) { |c| "r#{r}c#{c}" } }
    [[1, 1], [1, 2], [2, 1], [3, 2], [9, 1]].each do |buffer, files|
      assert_equal(rows.transpose,
                   transpose_matrix(rows,
                                    max_buffered_fields: buffer,
                                    max_open_files: files),
                   "buffer=#{buffer} files=#{files}")
    end
  end

  def test_round_trip_with_tiny_budget
    rows = Array.new(5) { |r| Array.new(7) { |c| "r#{r}c#{c}" } }
    rows[0][0] = "a,b"
    rows[1][2] = %q{q"q}
    rows[2][3] = "line\nbreak"
    rows[3][4] = ""
    input = rows.map { |row| CSV.generate_line(row) }.join
    [[1, 1], [1, 3], [2, 2], [4, 1]].each do |buffer, files|
      once = transpose(input,
                       max_buffered_fields: buffer,
                       max_open_files: files)
      assert_equal(rows.transpose, CSV.parse(once))
      back = transpose(once,
                       max_buffered_fields: buffer,
                       max_open_files: files)
      assert_equal(rows, CSV.parse(back),
                   "round-trip buffer=#{buffer} files=#{files}")
    end
  end

  def test_ragged_growth_with_tiny_budget
    rows = [["a", "b"], ["1", "2"], ["x", "y", "z"], ["p", "q", "r", "s"]]
    input = rows.map { |row| CSV.generate_line(row) }.join
    [[1, 1], [1, 2], [2, 1]].each do |buffer, files|
      output = transpose(input,
                         ragged_rows: "P",
                         max_buffered_fields: buffer,
                         max_open_files: files)
      expected = [
        ["a", "1", "x", "p"],
        ["b", "2", "y", "q"],
        ["P", "P", "z", "r"],
        ["P", "P", "P", "s"],
      ]
      got = CSV.parse(output).map { |r| r.map { |f| f.nil? ? "P" : f } }
      assert_equal(expected, got,
                   "buffer=#{buffer} files=#{files}")
    end
  end

  # --- Forward-only input -------------------------------------------------

  def test_input_is_read_only_forward_never_rewound
    input = String.new("a,b,c\n1,2,3\n")
    io = OneReadIO.new(input)
    output = StringIO.new
    result = CSV.transpose(io, output, tmpdir: @tmpdir,
                           max_buffered_fields: 1,
                           max_open_files: 1)
    assert_kind_of(CSV, result)
    assert_equal("a,1\nb,2\nc,3\n", output.string)
    assert_operator(io.read_count, :>=, 1)
    assert_equal(0, io.rewind_count)
  end

  # --- IO output ----------------------------------------------------------

  def test_file_in_file_out
    in_path = File.join(@tmpdir, "in.csv")
    out_path = File.join(@tmpdir, "out.csv")
    File.write(in_path, "a,b,c\n1,2,3\n")
    File.open(in_path, "rb") do |fin|
      File.open(out_path, "wb") do |fout|
        assert_kind_of(CSV, CSV.transpose(fin, fout, tmpdir: @tmpdir))
      end
    end
    assert_equal("a,1\nb,2\nc,3\n", File.read(out_path))
  end

  def test_instance_method_to_io
    output = StringIO.new
    CSV.new("a,b\n1,2\n").transpose(output, tmpdir: @tmpdir)
    assert_equal("a,1\nb,2\n", output.string)
  end

  def test_instance_method_returns_string
    assert_equal("a,1\nb,2\n",
                 CSV.new("a,b\n1,2\n").transpose(tmpdir: @tmpdir))
  end

  # --- Options ------------------------------------------------------------

  def test_tab_separator
    output = transpose("a\tb\tc\n1\t2\t3\n", col_sep: "\t")
    assert_equal("a\t1\nb\t2\nc\t3\n", output)
  end

  def test_distinct_input_and_output_separators
    output = transpose("a;b;c\n1;2;3\n",
                       in_col_sep: ";", out_col_sep: "|")
    assert_equal("a|1\nb|2\nc|3\n", output)
  end

  def test_custom_output_row_separator
    output = transpose("a,b\n1,2\n", out_row_sep: "\r\n")
    assert_equal("a,1\r\nb,2\r\n", output)
  end

  def test_multibyte_content_with_tiny_budget
    rows = [["héllo", "wörld"], ["日本語", "x"]]
    assert_equal(rows.transpose,
                 transpose_matrix(rows, max_buffered_fields: 1))
  end

  def test_special_fields_round_trip_through_temp_files
    # Fields needing quoting with commas, quotes and newlines, with
    # a tiny budget forcing every field through a stripe file.
    rows = [
      ["a,b", %q{he said "hi"}, "line\nbreak", ""],
      ["1", "2", "3", "4"],
    ]
    input = rows.map { |row| CSV.generate_line(row) }.join
    output = transpose(input,
                       ragged_rows: "",
                       max_buffered_fields: 1,
                       max_open_files: 1)
    got = CSV.parse(output).map { |r| r.map { |f| f.nil? ? "" : f } }
    expected = rows.map { |r| r.map { |f| f.nil? ? "" : f } }.transpose
    assert_equal(expected, got)
  end

  def test_custom_quote_character
    rows = [["a|b", "plain"], ["c", "d"]]
    input = rows.map do |row|
      CSV.generate_line(row, quote_char: "|", col_sep: ";")
    end.join
    output = transpose(input,
                       quote_char: "|",
                       col_sep: ";",
                       max_buffered_fields: 1)
    assert_equal(rows.transpose,
                 CSV.parse(output, quote_char: "|", col_sep: ";"))
    assert_include(output, "|a||b|;c")
  end

  def test_invalid_buffered_fields
    assert_raise(ArgumentError) do
      transpose("a\n", max_buffered_fields: 0)
    end
  end

  def test_invalid_open_files
    assert_raise(ArgumentError) do
      transpose("a\n", max_open_files: 0)
    end
  end

  # --- Temporary resource reclamation -------------------------------------

  def test_temporary_files_are_removed
    rows = Array.new(6) { |r| Array.new(8) { |c| "r#{r}c#{c}" } }
    input = rows.map { |row| CSV.generate_line(row) }.join
    before = Dir.glob(File.join(@tmpdir, "**", "*")).to_a
    5.times do
      transpose(input, max_buffered_fields: 1, max_open_files: 1)
    end
    GC.start
    after = Dir.glob(File.join(@tmpdir, "**", "*")).to_a
    assert_equal([], after - before)
  end

  def test_temporary_files_are_removed_on_ragged_error
    before = Dir.glob(File.join(@tmpdir, "**", "*")).to_a
    assert_raise(CSV::RaggedRowError) do
      transpose("a,b,c\n1,2\nx,y,z\n",
                max_buffered_fields: 1, max_open_files: 1)
    end
    GC.start
    after = Dir.glob(File.join(@tmpdir, "**", "*")).to_a
    assert_equal([], after - before)
  end

  def test_open_file_bound_is_respected
    # Drives the pool through many stripes with a single handle slot;
    # correctness here also proves closed stripes are reopened.
    rows = Array.new(3) { |r| Array.new(12) { |c| "r#{r}c#{c}" } }
    assert_equal(rows.transpose,
                 transpose_matrix(rows,
                                  max_buffered_fields: 1,
                                  max_open_files: 1))
  end

  def test_stripe_pool_never_exceeds_handle_bound
    pool = CSV::Transposer::StripeFilePool.new(
      @tmpdir, 2,
      col_sep: ",", row_sep: "\n", quote_char: '"',
      encoding: Encoding::UTF_8)
    10.times { |stripe| pool.create(stripe) }
    assert_operator(pool.peak_open_count, :<=, 2)

    10.times { |stripe| pool.append(stripe, ["a#{stripe}", "b"]) }
    assert_operator(pool.open_count, :<=, 2)

    values = []
    10.times do |stripe|
      pool.records(stripe) { |record| values << record.first }
      assert_operator(pool.open_count, :<=, 2)
    end
    assert_equal(10.times.map { |i| "a#{i}" }, values)
    pool.close_all
    assert_equal(0, pool.open_count)
  ensure
    pool&.close_all
  end

  # An IO that can be read once and rewound never.
  class OneReadIO
    attr_reader :read_count, :rewind_count

    def initialize(string)
      @io = StringIO.new(string)
      @read_count = 0
      @rewind_count = 0
    end

    def read(*args)
      @read_count += 1
      @io.read(*args)
    end

    def gets(*args)
      @read_count += 1
      @io.gets(*args)
    end

    def each(&block)
      @io.each(&block)
    end

    def rewind
      @rewind_count += 1
      raise "rewind must not be called"
    end

    def close
      @io.close
    end
  end
end
