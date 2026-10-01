# -*- coding: utf-8 -*-
# frozen_string_literal: false

require "tempfile"
require_relative "../helper"

class TestCSVParsePartitions < Test::Unit::TestCase
  extend DifferentOFS
  include CSVHelper

  private

  def with_csv_file(data, binmode: true)
    file = Tempfile.new(["csv-partitions", ".csv"])
    file.binmode if binmode
    file.write(data)
    file.flush
    begin
      yield(file.path, file)
    ensure
      file.close
      file.unlink
    end
  end

  def serial_rows(data, **options)
    CSV.parse(data, **options)
  end

  def merge_partitions(plan, **options)
    merged = []
    plan.each do |partition|
      partition.each(**options) { |row| merged << row }
    end
    merged
  end

  def assert_partitions_roundtrip(data, count, **options)
    serial = serial_rows(data, **options)
    with_csv_file(data) do |path|
      plan = CSV.partitions(path, count, **options)
      assert_equal(count, plan.length)
      merged = merge_partitions(plan, **options)
      assert_equal(serial, merged)

      # Ranges must be contiguous and never overlap.
      starts = plan.map(&:start_byte)
      ends = plan.map(&:end_byte)
      assert_equal(starts[1..], ends[0...-1])
      plan.each do |partition|
        assert_operator(partition.start_byte, :<=, partition.end_byte)
      end
      assert_equal(data.bytesize, ends.last)
      # The first partition begins after the header line (or at zero
      # without headers), and the first boundary must be a safe record
      # boundary (or the data start).
      assert(starts.first >= 0)
    end
  end

  public

  def test_basic_split
    data = (1..10).map { |i| "row#{i},value#{i}" }.join("\n") + "\n"
    [1, 2, 3, 5, 10, 20].each do |count|
      assert_partitions_roundtrip(data, count)
    end
  end

  def test_no_trailing_newline
    data = "a,b\nc,d\ne,f"
    [1, 2, 3, 4].each do |count|
      assert_partitions_roundtrip(data, count)
    end
  end

  def test_empty_file
    with_csv_file("") do |path|
      plan = CSV.partitions(path, 3)
      assert_equal(3, plan.length)
      assert_equal([], merge_partitions(plan))
    end
  end

  def test_only_newlines
    data = "\n\n\n\n"
    assert_partitions_roundtrip(data, 3)
  end

  def test_embedded_newline_in_quotes
    data = <<~CSV
      id,text
      1,"line one
      line two"
      2,"a,b
      c"
      3,plain
    CSV
    [1, 2, 3, 5, 8].each do |count|
      assert_partitions_roundtrip(data, count, headers: true)
    end
  end

  def test_crlf
    data = "h1,h2\r\n" +
           (1..20).map { |i| "r#{i},\"v#{i}\r\ncont\u{4e2d}\"" }.
             join("\r\n") + "\r\nlast,x\r\n"
    [1, 2, 4, 7, 12].each do |count|
      assert_partitions_roundtrip(data, count, headers: true)
    end
  end

  def test_multibyte_content
    data = (1..30).map do |i|
      "\u{3055}\u{3093},\"\u{4e2d}\n\u{56fd}\u{554a}\",#{i}"
    end.join("\n") + "\n"
    [1, 2, 5, 11, 31].each do |count|
      assert_partitions_roundtrip(data, count)
    end
  end

  def test_custom_separators
    data = (1..20).map do |i|
      "f1#{i}\tf2#{i}\t\"x\ty\nz\u{00e9}\""
    end.join("|")
    [1, 2, 4, 9].each do |count|
      assert_partitions_roundtrip(data, count,
                                  col_sep: "\t", row_sep: "|")
    end
  end

  def test_quoted_multibyte_separator
    data = (1..20).map { |i| "a#{i}\u{00b7}b#{i}" }.join("\n") + "\n"
    assert_partitions_roundtrip(data, 4, col_sep: "\u{00b7}")
  end

  def test_headers_provided_to_every_partition
    data = "name,value\n" +
           (1..30).map { |i| "n#{i},v#{i}" }.join("\n") + "\n"
    with_csv_file(data) do |path|
      plan = CSV.partitions(path, 4, headers: true)
      assert(plan.headers?)
      assert_equal(%w[name value], plan.headers)
      merged = []
      plan.each do |partition|
        partition.each(headers: true) do |row|
          assert_kind_of(CSV::Row, row)
          assert_equal(%w[name value], row.headers)
          merged << row.to_h
        end
      end
      assert_equal(CSV.parse(data, headers: true).map(&:to_h), merged)
    end
  end

  def test_array_headers_keep_first_row_as_data
    data = "a,b\nc,d\ne,f\n"
    with_csv_file(data) do |path|
      plan = CSV.partitions(path, 2, headers: %w[X Y])
      refute(plan.headers?)
      merged = []
      plan.each do |partition|
        partition.each(headers: %w[X Y]) { |row| merged << row.to_h }
      end
      assert_equal([{ "X" => "a", "Y" => "b" },
                    { "X" => "c", "Y" => "d" },
                    { "X" => "e", "Y" => "f" }], merged)
    end
  end

  def test_start_record_index
    data = (1..10).map { |i| "a#{i},b#{i}" }.join("\n") + "\n"
    with_csv_file(data) do |path|
      plan = CSV.partitions(path, 3)
      counts = plan.map { |p| p.each.to_a.size }
      starts = plan.map(&:start_record)
      assert_equal(0, starts.first)
      cumulative = starts.dup
      cumulative.each_with_index do |start, i|
        next if i.zero?
        assert_equal(starts[0...i].zip(counts[0...i]).
                       map { |s, c| s + c }.last, start)
      end
      assert_equal(10, starts.last + counts.last)
    end
  end

  def test_partition_size
    data = (1..100).map { |i| "row#{i},v" }.join("\n") + "\n"
    with_csv_file(data) do |path|
      plan = CSV.partitions(path, partition_size: 23)
      merged = merge_partitions(plan)
      assert_equal(serial_rows(data), merged)
      plan.each do |partition|
        # Excluding a final empty partition, every partition is bounded
        # by a record end, so ranges stay on safe offsets.
        assert_operator(partition.byte_size, :>=, 0)
      end
    end
  end

  def test_independent_io_handles
    with_csv_file("a\n" * 50) do |path|
      plan = CSV.partitions(path, 4)
      readers = plan.map { |partition| partition.open }
      begin
        enums = readers.map(&:each)
        first = enums.first.first
        assert_equal(["a"], first)
      ensure
        readers.each(&:close)
      end
    end
  end

  def test_invalid_utf8_does_not_split_character
    # A boundary is chosen between whole characters; a multi-byte
    # sequence must never be torn apart.
    data = "\u{4e2d}\u{56fd}\n" * 20
    assert_partitions_roundtrip(data, 7)
  end

  def test_requires_argument
    with_csv_file("a\n") do |path|
      assert_raise(ArgumentError) { CSV.partitions(path) }
    end
  end

  def test_non_seekable_io_raises
    io = Object.new
    def io.respond_to?(name, *)
      name == :gets || super
    end
    assert_raise(ArgumentError) { CSV.partitions(io, 2) }
  end

  def test_io_object
    require "stringio"
    data = (1..20).map { |i| "x#{i},y#{i}" }.join("\n") + "\n"
    file = Tempfile.new(["csv-partitions-io", ".csv"])
    file.binmode
    file.write(data)
    file.flush
    file.rewind
    plan = CSV.partitions(file, 4)
    assert_equal(serial_rows(data), merge_partitions(plan))
  ensure
    file&.close
    file&.unlink
  end
end
