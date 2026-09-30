# frozen_string_literal: true

require_relative "helper"

class TestCSVExternalSort < Test::Unit::TestCase
  class FailingStringIO < StringIO
    def initialize(*args)
      super
      @read_count = 0
    end

    def count_read
      @read_count += 1
      raise IOError, "read failed" if @read_count > 1
    end
    private :count_read

    def gets(*args)
      count_read
      super
    end

    def read(*args)
      count_read
      super
    end

    def each_line(*args, &block)
      count_read
      super
    end
  end

  def generate_csv(rows, headers)
    CSV.generate do |csv|
      csv << headers if headers
      rows.each {|row| csv << row}
    end
  end

  def stable_reference(rows, &key_of)
    rows.each_with_index.sort do |(row_a, index_a), (row_b, index_b)|
      cmp = yield(row_a) <=> yield(row_b)
      cmp.zero? ? index_a <=> index_b : cmp
    end.map(&:first)
  end

  def leftover_dirs
    Dir.glob(File.join(Dir.tmpdir, "csv-external-sort-*"))
  end

  def test_sorts_by_named_key_through_multiple_merge_rounds
    rows = (0...25).map {|i| [i.to_s, ((i * 7) % 25).to_s, "payload-#{i}"]}
    input = generate_csv(rows, %w[id key payload])
    sorter = CSV::ExternalSort.new(StringIO.new(input),
                                 key: "key",
                                 headers: true,
                                 chunk_size: 2,
                                 merge_fanin: 2)
    result = sorter.map {|row| row.fields}
    expected = stable_reference(rows) {|row| row[1]}
    assert_equal(expected, result)
    assert_equal(%w[id key payload], sorter.headers)
    assert_empty(leftover_dirs)
  end

  def test_stable_for_equal_keys_across_merge_rounds
    rows = (0...30).map {|i| [i.to_s, (i % 3).to_s, "payload-#{i}"]}
    input = generate_csv(rows, %w[id key payload])
    sorter = CSV::ExternalSort.new(StringIO.new(input),
                                   key: ->(row) { Integer(row["key"]) },
                                   headers: true,
                                   chunk_size: 3,
                                   merge_fanin: 2)
    result = sorter.map {|row| row["payload"]}
    expected = stable_reference(rows) {|row| Integer(row[1])}.map {|row| row[2]}
    assert_equal(expected, result)
    (0...3).each do |key|
      payloads = (0...30).select {|i| i % 3 == key}.map {|i| "payload-#{i}"}
      assert_equal(payloads,
                   result.each_slice(10).to_a[key],
                   "input order must be kept for key #{key}")
    end
  end

  def test_custom_comparator
    rows = (0...10).map {|i| [i.to_s, (i % 4).to_s]}
    input = generate_csv(rows, %w[id key])
    sorter = CSV::ExternalSort.new(StringIO.new(input),
                                   key: "key",
                                   compare: ->(a, b) { b <=> a },
                                   headers: true,
                                   chunk_size: 2,
                                   merge_fanin: 3)
    result = sorter.map {|row| row.fields}
    expected = rows.each_with_index.sort do |(row_a, index_a), (row_b, index_b)|
      cmp = row_b[1] <=> row_a[1]
      cmp.zero? ? index_a <=> index_b : cmp
    end.map(&:first)
    assert_equal(expected, result)
  end

  def test_sorts_arrays_without_headers
    rows = (0...12).map {|i| [i.to_s, ((i * 5) % 12).to_s]}
    input = generate_csv(rows, nil)
    sorter = CSV::ExternalSort.new(StringIO.new(input),
                                   key: 1,
                                   chunk_size: 2,
                                   merge_fanin: 2)
    assert_equal(stable_reference(rows) {|row| row[1]}, sorter.to_a)
    assert_nil(sorter.headers)
  end

  def test_sorts_from_file_path
    rows = (0...9).map {|i| [i.to_s, (9 - i).to_s]}
    Tempfile.create(["ledger", ".csv"]) do |file|
      file.write(generate_csv(rows, %w[id key]))
      file.flush
      sorter = CSV::ExternalSort.new(file.path,
                                     key: "key",
                                     headers: true,
                                     chunk_size: 2,
                                     merge_fanin: 2)
      assert_equal(stable_reference(rows) {|row| row[1]},
                   sorter.map {|row| row.fields})
    end
    assert_empty(leftover_dirs)
  end

  def test_preserves_headers_duplicate_columns_nil_and_newlines
    records = [
      ["2", "x", nil],
      ["1", "y", "line1\nline2"],
      ["3", "z", "quote\"me"],
    ]
    input = generate_csv(records, ["a", "a", "b"])
    sorter = CSV::ExternalSort.new(StringIO.new(input),
                                   key: "a",
                                   headers: true,
                                   chunk_size: 2,
                                   merge_fanin: 2)
    result = sorter.to_a
    assert_equal(["a", "a", "b"], sorter.headers)
    assert_equal(["a", "a", "b"], result.first.headers)
    assert_equal([["1", "y", "line1\nline2"],
                  ["2", "x", nil],
                  ["3", "z", "quote\"me"]],
                 result.map(&:fields))

    output = CSV.generate do |csv|
      csv << sorter.headers
      result.each {|row| csv << row}
    end
    reparsed = CSV.parse(output, headers: true)
    assert_equal(result.map(&:fields), reparsed.map(&:fields))
    assert_equal(["a", "a", "b"], reparsed.headers)
    assert_empty(leftover_dirs)
  end

  def test_empty_input
    sorter = CSV::ExternalSort.new(StringIO.new(""),
                                   key: "key",
                                   headers: true,
                                   chunk_size: 2,
                                   merge_fanin: 2)
    assert_equal([], sorter.to_a)
    assert_empty(leftover_dirs)
  end

  def test_headers_only_input
    sorter = CSV::ExternalSort.new(StringIO.new("a,b\n"),
                                   key: "a",
                                   headers: true,
                                   chunk_size: 2,
                                   merge_fanin: 2)
    assert_equal([], sorter.to_a)
    assert_equal(%w[a b], sorter.headers)
    assert_empty(leftover_dirs)
  end

  def test_comparator_error_releases_temporary_files
    rows = (0...10).map {|i| [i.to_s, i.to_s]}
    input = generate_csv(rows, %w[id key])
    sorter = CSV::ExternalSort.new(StringIO.new(input),
                                   key: "key",
                                   compare: ->(_a, _b) { raise "boom" },
                                   headers: true,
                                   chunk_size: 2,
                                   merge_fanin: 2)
    assert_raise(RuntimeError) { sorter.each {|_row| } }
    assert_empty(leftover_dirs)
  end

  def test_key_extractor_error_releases_temporary_files
    rows = (0...10).map {|i| [i.to_s, i.to_s]}
    input = generate_csv(rows, %w[id key])
    sorter = CSV::ExternalSort.new(StringIO.new(input),
                                   key: ->(_row) { raise "key boom" },
                                   headers: true,
                                   chunk_size: 2,
                                   merge_fanin: 2)
    assert_raise(RuntimeError) { sorter.each {|_row| } }
    assert_empty(leftover_dirs)
  end

  def test_read_failure_releases_temporary_files
    rows = (0...10).map {|i| [i.to_s, i.to_s]}
    input = FailingStringIO.new(generate_csv(rows, %w[id key]))
    sorter = CSV::ExternalSort.new(input,
                                   key: "key",
                                   headers: true,
                                   chunk_size: 2,
                                   merge_fanin: 2)
    assert_raise(IOError) { sorter.each {|_row| } }
    assert_empty(leftover_dirs)
  end

  def test_early_break_releases_temporary_files
    rows = (0...10).map {|i| [i.to_s, ((i * 3) % 10).to_s]}
    input = generate_csv(rows, %w[id key])
    sorter = CSV::ExternalSort.new(StringIO.new(input),
                                   key: "key",
                                   headers: true,
                                   chunk_size: 2,
                                   merge_fanin: 2)
    yielded = 0
    sorter.each do |_row|
      yielded += 1
      break
    end
    assert_equal(1, yielded)
    assert_empty(leftover_dirs)
  end

  def test_invalid_chunk_size
    assert_raise(ArgumentError) do
      CSV::ExternalSort.new(StringIO.new(""), chunk_size: 0)
    end
  end

  def test_invalid_merge_fanin
    assert_raise(ArgumentError) do
      CSV::ExternalSort.new(StringIO.new(""), merge_fanin: 1)
    end
  end
end
