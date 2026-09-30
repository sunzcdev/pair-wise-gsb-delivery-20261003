# frozen_string_literal: true

require_relative "../helper"
require "stringio"

class TestCSVInterfaceSortedForeach < Test::Unit::TestCase
  extend DifferentOFS

  # Tiny chunk and fan-out settings force several spill runs and more
  # than one merge pass without needing a large input file.
  SORT_OPTIONS = {buffer_size: 2, max_open_files: 2}.freeze

  setup do
    @temp_dir = Dir.mktmpdir("csv-sorted-foreach-test")
  end

  teardown do
    FileUtils.remove_entry(@temp_dir, true) if @temp_dir
  end

  def sorted(input, key_column: 0, headers: false, **overrides)
    options = SORT_OPTIONS.merge(temp_dir: @temp_dir, headers: headers, **overrides)
    CSV.sorted_foreach(StringIO.new(input),
                       key: ->(row) { row[key_column] },
                       **options).to_a
  end

  def reference_arrays(input, key_column: 0, **options)
    rows = CSV.parse(input, **options)
    rows.each_with_index
        .sort_by { |row, index| [row[key_column].to_s, index] }
        .map(&:first)
  end

  def reference_with_header(input, key_name)
    parsed = CSV.parse(input, headers: true)
    header = CSV::Row.new(parsed.headers, parsed.headers, true)
    ordered = parsed.each_with_index
                    .sort_by { |row, index| [row[key_name], index] }
                    .map(&:first)
    [header] + ordered
  end

  def leftover_temp_files
    Dir.glob(File.join(@temp_dir, "csv-sort-*"))
  end

  def test_sorts_arrays_stably_through_multiple_merge_passes
    rows = 30.times.map { |i| [i % 5, "value#{i}"] }
    input = rows.map { |key, value| "#{key},#{value}" }.join("\n")

    result = sorted(input)

    expected = rows.each_with_index
                 .sort_by { |(key, _value), index| [key.to_s, index] }
                 .map { |(key, value), _index| [key.to_s, value] }
    assert_equal(expected, result)
  end

  def test_equal_keys_keep_input_order
    rows = 12.times.map { |i| ["same", "value#{i}"] }
    input = rows.map(&:last).map { |value| "same,#{value}" }.join("\n")

    result = sorted(input)

    assert_equal(rows, result)
    assert_equal(rows.map(&:last), result.map(&:last))
  end

  def test_matches_in_memory_reference
    lines = 40.times.map { |i| "#{rand(i + 1) % 7},v#{i}" }
    input = lines.join("\n")

    assert_equal(reference_arrays(input), sorted(input))
  end

  def test_block_key
    input = (1..10).map { |i| "#{i % 3},v#{i}" }.join("\n")
    consumed = []
    CSV.sorted_foreach(StringIO.new(input),
                       key: ->(row) { row[0] },
                       **SORT_OPTIONS) do |row|
      consumed << row
    end

    assert_equal(reference_arrays(input), consumed)
  end

  def test_header_row_is_first_and_excluded_from_sort
    input = "name,n\nz,1\na,2\nm,3\n"

    result = sorted(input, key_column: "name", headers: true)

    assert_equal(reference_with_header(input, "name"), result)
    assert_predicate(result.first, :header_row?)
    assert_equal(%w[a m z], result[1..].map { |row| row["name"] })
  end

  def test_empty_input_without_headers
    result = sorted("")

    assert_equal([], result)
    assert_equal([], leftover_temp_files)
  end

  def test_input_with_only_a_header
    input = "a,b,c\n"
    result = sorted(input, key_column: "a", headers: true)

    assert_equal(1, result.size)
    assert_predicate(result.first, :header_row?)
  end

  def test_small_input_needs_no_spill
    input = "2,b\n1,a\n"
    result = CSV.sorted_foreach(StringIO.new(input),
                                key: ->(row) { row[0] },
                                buffer_size: 100,
                                max_open_files: 2,
                                temp_dir: @temp_dir).to_a

    assert_equal([["1", "a"], ["2", "b"]], result)
    assert_equal([], leftover_temp_files)
  end

  def test_preserves_embedded_newlines_nils_and_duplicate_headers
    input = "h,h\n" +
            "b,\"line1\nline2\"\n" +
            "a,\n" +
            "a,\"x\r\ny\"\n" +
            "b,last"

    result = sorted(input, key_column: 0, headers: true)
    expected = reference_with_header(input, "h")

    assert_equal(expected.map(&:fields), result.map(&:fields))
    assert_equal(expected.map(&:header_row?), result.map(&:header_row?))
    # Duplicate header names and nil/empty fields survive verbatim.
    assert_equal(["h", "h"], result.first.fields)
    assert_nil(result[1][1])
    assert_equal("x\r\ny", result[2][1])
    assert_equal("line1\nline2", result[3][1])
  end

  def test_preserves_row_separator_and_quoted_semantics_on_round_trip
    input = "h\r\n" +
            "b,\"quoted, field\"\r\n" +
            "a,\"two\r\nlines\"\r\n"

    rows = CSV.sorted_foreach(StringIO.new(input),
                              key: ->(row) { row[0] },
                              headers: true,
                              **SORT_OPTIONS).to_a

    regenerated = CSV.generate(headers: rows.first.headers,
                               write_headers: true,
                               row_sep: "\r\n") do |csv|
      rows[1..].each { |row| csv << row }
    end

    expected = "h\r\n" +
               "a,\"two\r\nlines\"\r\n" +
               "b,\"quoted, field\"\r\n"
    assert_equal(expected, regenerated)
  end

  def test_works_with_file_path
    path = File.join(@temp_dir, "input.csv")
    input = (1..15).map { |i| "#{i % 4},v#{i}" }.join("\n")
    File.write(path, input)

    result = CSV.sorted_foreach(path,
                                key: ->(row) { row[0] },
                                temp_dir: @temp_dir,
                                **SORT_OPTIONS).to_a

    assert_equal(reference_arrays(input), result)
  end

  def test_respects_parsing_options
    input = "2,10\n1,3\n"
    result = CSV.sorted_foreach(StringIO.new(input),
                                key: ->(row) { row[1] },
                                converters: :integer,
                                temp_dir: @temp_dir,
                                **SORT_OPTIONS).to_a

    # converters: :integer converts every numeric field, including the
    # key column; the numeric ordering of column 2 drives the result.
    assert_equal([[1, 3], [2, 10]], result)
  end

  def test_explicit_headers_array
    input = "b,1\na,2\n"
    result = CSV.sorted_foreach(StringIO.new(input),
                                key: ->(row) { row["name"] },
                                headers: %w[name n],
                                temp_dir: @temp_dir,
                                **SORT_OPTIONS).to_a

    assert_equal([["a", "2"], ["b", "1"]], result.map(&:fields))
    assert_not_predicate(result.first, :header_row?)
  end

  def test_key_error_during_spill_releases_resources
    input = (1..10).map { |i| "#{i},v#{i}" }.join("\n")

    assert_raise(RuntimeError) do
      CSV.sorted_foreach(StringIO.new(input),
                         key: ->(_row) { raise "boom in key" },
                         temp_dir: @temp_dir,
                         **SORT_OPTIONS).to_a
    end

    assert_equal([], leftover_temp_files)
  end

  def test_key_error_during_merge_releases_resources
    input = (1..20).map { |i| "k#{i},v#{i}" }.join("\n")
    calls = 0

    assert_raise(RuntimeError) do
      key = lambda do |row|
        calls += 1
        raise "boom in merge" if calls > 20
        row[0]
      end
      CSV.sorted_foreach(StringIO.new(input),
                         key: key,
                         temp_dir: @temp_dir,
                         buffer_size: 2,
                         max_open_files: 2).to_a
    end

    assert_equal([], leftover_temp_files)
  end

  def test_consumer_break_releases_resources
    input = (1..20).map { |i| "#{i % 5},v#{i}" }.join("\n")

    CSV.sorted_foreach(StringIO.new(input),
                       key: ->(row) { row[0] },
                       temp_dir: @temp_dir,
                       **SORT_OPTIONS) do |_row|
      break
    end

    assert_equal([], leftover_temp_files)
  end

  def test_consumer_raising_releases_resources
    input = (1..20).map { |i| "#{i % 5},v#{i}" }.join("\n")

    assert_raise(RuntimeError) do
      CSV.sorted_foreach(StringIO.new(input),
                         key: ->(row) { row[0] },
                         temp_dir: @temp_dir,
                         **SORT_OPTIONS) do |_row|
        raise "consumer boom"
      end.to_a
    end

    assert_equal([], leftover_temp_files)
  end

  def test_read_failure_releases_resources
    path = File.join(@temp_dir, "broken.csv")
    File.write(path, (1..20).map { |i| "#{i},v#{i}" }.join("\n"))
    io = FailingIO.new(path)

    assert_raise(Errno::EIO) do
      CSV.sorted_foreach(io,
                         key: ->(row) { row[0] },
                         temp_dir: @temp_dir,
                         **SORT_OPTIONS).to_a
    end

    assert_equal([], leftover_temp_files)
  end

  class FailingIO
    def initialize(path)
      @io = File.open(path, "r")
    end

    def method_missing(name, *args, &block)
      @io.public_send(name, *args, &block)
    end

    def respond_to_missing?(name, include_private = false)
      @io.respond_to?(name, include_private) || super
    end

    def gets(*)
      raise Errno::EIO, "simulated read failure"
    end
  end

  def test_missing_key_is_an_error
    assert_raise(ArgumentError) do
      CSV.sorted_foreach(StringIO.new("1,a\n"))
    end
  end

  # A block is the consumer and may be used together with key:.
  def test_key_with_consumer_block
    input = "2,b\n1,a\n"
    consumed = []
    CSV.sorted_foreach(StringIO.new(input),
                       key: ->(row) { row[0] }) do |row|
      consumed << row
    end

    assert_equal([["1", "a"], ["2", "b"]], consumed)
  end

  def test_invalid_buffer_size
    assert_raise(ArgumentError) do
      CSV.sorted_foreach(StringIO.new("1,a\n"),
                         buffer_size: 0,
                         key: ->(row) { row[0] })
    end
  end

  def test_invalid_max_open_files
    assert_raise(ArgumentError) do
      CSV.sorted_foreach(StringIO.new("1,a\n"),
                         max_open_files: 1,
                         key: ->(row) { row[0] })
    end
  end

  def test_returns_enumerator_without_block
    input = "2,b\n1,a\n"
    enum = CSV.sorted_foreach(StringIO.new(input),
                              key: ->(row) { row[0] },
                              temp_dir: @temp_dir,
                              **SORT_OPTIONS)

    assert_kind_of(Enumerator, enum)
    assert_equal([["1", "a"], ["2", "b"]], enum.to_a)
    assert_equal([], leftover_temp_files)
  end
end
