# frozen_string_literal: true

require_relative "../helper"
require "tmpdir"
require "stringio"

class TestCSVInterfaceTranspose < Test::Unit::TestCase
  # An IO that can only be read once, front to back: it rejects rewind
  # and tracks how many times it was read.
  class OnceIO
    def initialize(data)
      @io = StringIO.new(data)
      @read_count = 0
    end

    attr_reader :read_count

    def gets(*args, &block)
      @read_count += 1
      @io.gets(*args, &block)
    end

    def each_line(*args, &block)
      @io.each_line(*args, &block)
    end

    def eof?
      @io.eof?
    end

    def rewind
      raise IOError, "this stream can be read only once"
    end

    def close
      @io.close
    end
  end

  def transpose(input, output = +"", **options)
    CSV.transpose(input, output, **options)
  end

  def matrix(csv_text, **options)
    CSV.parse(csv_text, **options)
  end

  def assert_transpose(source, expected, **options)
    assert_equal(expected, transpose(source, **options))
  end

  # --- basic behavior ------------------------------------------------

  def test_basic_transpose
    assert_transpose("a,b,c\n1,2,3\n",
                     "a,1\nb,2\nc,3\n")
  end

  def test_single_row
    assert_transpose("a,b,c\n",
                     "a\nb\nc\n")
  end

  def test_single_column
    assert_transpose("a\nb\nc\n",
                     "a,b,c\n")
  end

  def test_empty_input
    assert_equal("", transpose(""))
  end

  def test_blank_lines_are_data
    # Every record is data; there is no header guessing. A blank line is
    # just a zero-width row, which is ragged by default...
    assert_raise(CSV::UnevenRowError) do
      transpose("a,b\n\n")
    end
    # ...and with padding it becomes a row padded with empty fields.
    once = transpose("a,b\n\n", ragged_rows: :pad, pad_value: nil)
    assert_equal([["a", nil], ["b", nil]], matrix(once))
    assert_equal("a,\nb,\n", once)
  end

  def test_rectangular_round_trip
    source = "a,b,c\n1,2,3\nx,\"q,u\",z\n,,\n"
    once = transpose(source)
    back = transpose(once)
    assert_equal(matrix(source), matrix(back))
  end

  def test_quotes_commas_newlines_and_empty_fields_preserved
    source = <<~CSV
      "has,comma","has""quote",plain
      "line1
      line2",,end
    CSV
    expected = matrix(source)
    once = transpose(source)
    back = transpose(once)
    # The quoted empty field (nil at a trailing-empty position) comes
    # back as an empty field value; compare as strings.
    expected_normalized = expected.map {|row| row.map {|f| f.nil? ? nil : f}}
    back_matrix = matrix(back)
    assert_equal(expected_normalized.map {|row| row.map {|f| f || ""}},
                 back_matrix.map {|row| row.map {|f| f || ""}})
    assert_equal(expected, matrix(once).transpose)
  end

  def test_crlf_fields
    source = "a,\"x\r\ny\"\r\n1,2\r\n"
    once = transpose(source, in_row_sep: "\r\n", out_row_sep: "\r\n")
    back = transpose(once, in_row_sep: "\r\n", out_row_sep: "\r\n")
    assert_equal(matrix(source, row_sep: "\r\n"),
                 matrix(back, row_sep: "\r\n"))
  end

  def test_transposed_quoting
    # A space needs no quoting in CSV and stays bare...
    assert_equal([["a", "1"], ["b c", "2"]],
                 matrix(transpose("a,b c\n1,2\n")))
    assert_include(transpose("a,b c\n1,2\n"), "b c")
    # ...but a comma or quote does.
    assert_include(transpose("a,\"b,c\"\n1,2\n"), "\"b,c\"")
    assert_include(transpose("a,\"b\"\"c\"\n1,2\n"), "\"b\"\"c\"")
  end

  def test_empty_fields_intermediate
    once = transpose("a,b,c\n,,\n")
    assert_equal([["a", nil], ["b", nil], ["c", nil]], matrix(once))
  end

  # --- ragged rows ---------------------------------------------------

  def test_ragged_rows_raise_by_default
    error = assert_raise(CSV::UnevenRowError) do
      transpose("a,b,c\n1,2\n")
    end
    assert_equal(2, error.line_number)
  end

  def test_ragged_rows_raise_reports_first_bad_line
    error = assert_raise(CSV::UnevenRowError) do
      transpose("a,b\n1,2\nx,y,z\n")
    end
    assert_equal(3, error.line_number)
  end

  def test_ragged_rows_pad_default_nil
    once = transpose("a,b,c\n1,2\n", ragged_rows: :pad)
    # nil pads serialize as empty unquoted fields.
    assert_equal("a,1\nb,2\nc,\n", once)
  end

  def test_ragged_rows_pad_explicit_value
    once = transpose("a,b,c\n1,2\n",
                     ragged_rows: :pad, pad_value: "MISSING")
    assert_equal("a,1\nb,2\nc,MISSING\n", once)
  end

  def test_ragged_rows_pad_wide_then_narrow
    once = transpose("a,b,c\n1,2\nx,y,z\n",
                     ragged_rows: :pad, pad_value: ".")
    assert_equal("a,1,x\nb,2,y\nc,.,z\n", once)
  end

  def test_ragged_rows_double_transpose_equals_padded_matrix
    source = "a,b,c\n1,2\n"
    once = transpose(source, ragged_rows: :pad, pad_value: "P")
    back = transpose(once, ragged_rows: :pad, pad_value: "P")
    assert_equal("a,b,c\n1,2,P\n", back)
  end

  def test_invalid_ragged_rows_value
    assert_raise(ArgumentError) do
      transpose("a\n", ragged_rows: :bogus)
    end
  end

  # --- read-once IO --------------------------------------------------

  def test_read_once_io
    io = OnceIO.new("a,b,c\n1,2,3\n")
    assert_equal("a,1\nb,2\nc,3\n", transpose(io))
    assert_operator(io.read_count, :>, 0)
  end

  def test_string_is_not_rewound
    # A String is wrapped in a StringIO internally; just ensure it works
    # without the caller supplying a rewindable object.
    assert_equal("a,1\nb,2\n", transpose(+"a,b\n1,2\n"))
  end

  # --- resource budget: wide tables with tiny limits ----------------

  def test_wide_table_with_single_open_file
    Dir.mktmpdir do |tmpdir|
      rows = 20
      columns = 25
      source = rows.times.map do |r|
        columns.times.map {|c| "r#{r}c#{c}" }.join(",")
      end.join("\n") + "\n"

      out = StringIO.new
      CSV.transpose(OnceIO.new(source), out,
                    max_buffered_fields: 1,
                    max_open_files: 1,
                    tmpdir: tmpdir)
      transposed = out.string

      expected = columns.times.map do |c|
        rows.times.map {|r| "r#{r}c#{c}" }.join(",")
      end.join("\n") + "\n"
      assert_equal(expected, transposed)
      remaining = Dir.glob(File.join(tmpdir, "*"))
      assert_equal([], remaining)
    end
  end

  def test_wide_table_round_trip_under_tiny_budget
    rows = 30
    columns = 17
    source_rows = rows.times.map do |r|
      columns.times.map {|c| "v#{r}_#{c}" }
    end
    source = source_rows.map {|row| CSV.generate_line(row) }.join

    once = CSV.transpose(OnceIO.new(source), StringIO.new,
                         max_buffered_fields: 3,
                         max_open_files: 1)
    assert_nil(once) # IO destination returns nil

    out = StringIO.new
    CSV.transpose(OnceIO.new(source), out,
                  max_buffered_fields: 7,
                  max_open_files: 2)
    transposed = CSV.parse(out.string)
    assert_equal(columns, transposed.length)
    assert_equal(rows, transposed.first.length)
    assert_equal(source_rows.transpose, transposed)

    back_io = StringIO.new
    CSV.transpose(OnceIO.new(out.string), back_io,
                  max_buffered_fields: 5,
                  max_open_files: 1)
    assert_equal(source_rows, CSV.parse(back_io.string))
  end

  def test_open_files_never_exceed_limit
    Dir.mktmpdir do |tmpdir|
      columns = 12
      source = 5.times.map do
        columns.times.map {|c| "f#{c}" }.join(",")
      end.join("\n") + "\n"

      max_seen = 0
      pool_class = Class.new(CSV::Transposer::SpillPool) do
        define_method(:writer_for) do |column_index|
          handle = super(column_index)
          count = @writers.length
          max_seen = count if count > max_seen
          handle
        end
      end

      output = StringIO.new
      input_csv = CSV.new(OnceIO.new(source))
      output_csv = CSV.new(output)
      writer = output_csv.send(:writer)
      writers_open_when_reading = nil
      pool_class_local = pool_class
      transposer = Class.new(CSV::Transposer) do
        define_method(:emit) do
          writers_open_when_reading =
            @spills.instance_variable_get(:@writers).length
          super()
        end

        define_method(:new_spill_pool) do |max_open_files, tmpdir, encoding|
          pool_class_local.new(max_open_files, tmpdir, encoding)
        end
      end.allocate
      transposer.send(:initialize,
                      input_csv, writer,
                      max_buffered_fields: 1,
                      max_open_files: 2,
                      tmpdir: tmpdir)
      transposer.transpose

      assert_operator(max_seen, :<=, 2)
      assert_equal(2, max_seen) # the limit was actually exercised
      assert_equal(0, writers_open_when_reading) # writers closed before reads
      pool = transposer.instance_variable_get(:@spills)
      assert_equal({}, pool.instance_variable_get(:@writers))
      assert_equal({}, pool.instance_variable_get(:@tempfiles))
      assert_equal([], Dir.glob(File.join(tmpdir, "*")))
    end
  end

  def test_buffered_fields_bounded
    budget = 13
    observed_max = 0
    transposer_class = Class.new(CSV::Transposer) do
      define_method(:add_field) do |column_index, field|
        # Momentary peak is the pre-call count plus this new field; the
        # threshold flush happens inside super, so measure before it.
        peak = @buffered_field_count + 1
        observed_max = peak if peak > observed_max
        super(column_index, field)
      end
    end

    source = 10.times.map { 10.times.map {|c| "c#{c}" }.join(",") }.join("\n") + "\n"
    output = StringIO.new
    input_csv = CSV.new(OnceIO.new(source))
    writer = CSV.new(output).send(:writer)
    transposer = transposer_class.allocate
    transposer.send(:initialize,
                    input_csv, writer,
                    max_buffered_fields: budget, max_open_files: 1)
    transposer.transpose

    # The buffer never holds the whole matrix (100 fields) or even one
    # full column accumulated many times; it stays around the configured
    # budget and is reclaimed as soon as the budget is met.
    assert_operator(observed_max, :<=, budget + 10)
    assert_operator(observed_max, :<, 100)
    assert_operator(observed_max, :>, 1) # flushing genuinely happened
  end

  def test_buffer_flushes_before_accumulating_two_rows_at_budget_one
    # With budget 1 the very first field triggers a flush: at most one
    # field is ever buffered, which directly proves no column is built up
    # in memory.
    observed_max = 0
    transposer_class = Class.new(CSV::Transposer) do
      define_method(:add_field) do |column_index, field|
        peak = @buffered_field_count + 1
        observed_max = peak if peak > observed_max
        super(column_index, field)
      end
    end
    source = 6.times.map { 4.times.map {|c| "c#{c}" }.join(",") }.join("\n") + "\n"
    output = StringIO.new
    input_csv = CSV.new(OnceIO.new(source))
    writer = CSV.new(output).send(:writer)
    transposer = transposer_class.allocate
    transposer.send(:initialize,
                    input_csv, writer,
                    max_buffered_fields: 1, max_open_files: 1)
    transposer.transpose
    assert_equal(1, observed_max)
  end

  def test_no_spill_files_when_input_fits_in_budget
    Dir.mktmpdir do |tmpdir|
      CSV.transpose("a,b\n1,2\n", +"", tmpdir: tmpdir)
      assert_equal([], Dir.glob(File.join(tmpdir, "*")))
    end
  end

  def test_cleanup_on_ragged_error
    Dir.mktmpdir do |tmpdir|
      assert_raise(CSV::UnevenRowError) do
        CSV.transpose("a,b,c\n1,2\n3,4,5\n", +"",
                      max_buffered_fields: 1,
                      max_open_files: 1,
                      tmpdir: tmpdir)
      end
      assert_equal([], Dir.glob(File.join(tmpdir, "*")))
    end
  end

  def test_cleanup_on_malformed_csv
    Dir.mktmpdir do |tmpdir|
      assert_raise(CSV::MalformedCSVError) do
        CSV.transpose("a,\"unterminated\nx,y\n", +"",
                      max_buffered_fields: 1,
                      max_open_files: 1,
                      tmpdir: tmpdir)
      end
      assert_equal([], Dir.glob(File.join(tmpdir, "*")))
    end
  end

  # --- options -------------------------------------------------------

  def test_headers_option_rejected
    assert_raise(ArgumentError) do
      transpose("a,b\n", headers: true)
    end
  end

  def test_input_headers_option_rejected
    assert_raise(ArgumentError) do
      transpose("a,b\n", in_headers: true)
    end
  end

  def test_write_headers_option_rejected
    assert_raise(ArgumentError) do
      transpose("a,b\n", out_write_headers: true)
    end
  end

  def test_custom_col_sep_input_and_output
    source = "a;b;c\n1;2;3\n"
    assert_equal("a|1\nb|2\nc|3\n",
                 transpose(source,
                           in_col_sep: ";",
                           out_col_sep: "|"))
  end

  def test_custom_row_sep
    assert_equal("a,1\r\nb,2\r\n",
                 transpose("a,b\r\n1,2\r\n",
                           in_row_sep: "\r\n",
                           out_row_sep: "\r\n"))
  end

  def test_force_quotes
    assert_equal("\"a\",\"1\"\n\"b\",\"2\"\n",
                 transpose("a,b\n1,2\n", force_quotes: true))
  end

  def test_quote_empty_false
    assert_equal("a,1\nb,2\n",
                 transpose("a,b\n1,2\n", quote_empty: false))
  end

  def test_write_converters
    # After transposition the "1" field lands on the second output row,
    # and the write converter still sees and may rewrite each field.
    once = transpose("a,1\nb,2\n",
                     write_converters: lambda {|field|
                       field == "1" ? "ONE" : field
                     })
    assert_equal("a,b\nONE,2\n", once)
  end

  def test_invalid_buffer_option
    assert_raise(ArgumentError) do
      transpose("a\n", max_buffered_fields: 0)
    end
  end

  def test_invalid_open_files_option
    assert_raise(ArgumentError) do
      transpose("a\n", max_open_files: 0)
    end
  end

  # --- destinations --------------------------------------------------

  def test_output_to_io
    Dir.mktmpdir do |tmpdir|
      path = File.join(tmpdir, "out.csv")
      File.open(path, "w") do |io|
        result = CSV.transpose("a,b\n1,2\n", io)
        assert_nil(result)
      end
      assert_equal("a,1\nb,2\n", File.read(path))
    end
  end

  def test_output_string_returned_and_mutated
    destination = +""
    result = CSV.transpose("a,b\n1,2\n", destination)
    assert_same(destination, result)
    assert_equal("a,1\nb,2\n", destination)
  end

  def test_output_stringio_not_closed_by_transpose
    io = StringIO.new
    CSV.transpose("a,b\n1,2\n", io)
    assert_not_predicate(io, :closed?)
    assert_equal("a,1\nb,2\n", io.string)
  end

  def test_input_from_io
    Dir.mktmpdir do |tmpdir|
      path = File.join(tmpdir, "in.csv")
      File.write(path, "a,b\n1,2\n")
      File.open(path) do |io|
        assert_equal("a,1\nb,2\n", transpose(io))
      end
    end
  end

  # --- encodings -----------------------------------------------------

  def test_multibyte_utf8_round_trip
    matrix = [["日本,語", "a"], ["改行\nテスト", "b"], ["空", ""]]
    source = matrix.map {|row| CSV.generate_line(row) }.join
    once = transpose(source)
    back = transpose(once)
    assert_equal(matrix, CSV.parse(back))
  end

  def test_transcode_to_output_encoding
    source = "山田,佐藤\n1,2\n"
    sjis = transpose(source, +"", out_encoding: "Shift_JIS")
    expected = "山田,1\n佐藤,2\n".encode("Shift_JIS")
    assert_equal(Encoding::Shift_JIS, sjis.encoding)
    assert_equal(expected, sjis)
  end

  def test_read_shift_jis_input
    sjis_source = "山田,佐藤\n1,2\n".encode("Shift_JIS")
    input = StringIO.new(sjis_source)
    input.set_encoding("Shift_JIS")
    output = StringIO.new
    CSV.transpose(input, output, in_encoding: "Shift_JIS")
    assert_equal([["山田", "1"], ["佐藤", "2"]], CSV.parse(output.string))
  end
end
