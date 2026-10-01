# encoding: UTF-8
# frozen_string_literal: true

require_relative "helper"

class TestCSVPartitioner < Test::Unit::TestCase
  include CSVHelper

  def with_csv_file(content)
    file = Tempfile.new(["csv_partitioner", ".csv"])
    begin
      file.binmode
      file.write(content)
      file.close
      yield file.path
    ensure
      file.unlink
    end
  end

  def with_partition_chunk_size(chunk_size)
    CSV::Partitioner.send(:remove_const, :CHUNK_SIZE)
    CSV::Partitioner.const_set(:CHUNK_SIZE, chunk_size)
    yield
  ensure
    CSV::Partitioner.send(:remove_const, :CHUNK_SIZE)
    CSV::Partitioner.const_set(:CHUNK_SIZE, 64 * 1024)
  end

  def serial_rows(path, options)
    CSV.foreach(path, "rb:UTF-8", **options).to_a
  end

  def assert_partition_plan(path, partitions, options)
    serial = serial_rows(path, options)

    # Byte ranges tile the whole file without gaps or overlaps.
    assert_equal(0, partitions.first.byte_range.begin)
    partitions.each_cons(2) do |previous, current|
      assert_equal(previous.byte_range.end, current.byte_range.begin)
    end
    assert_equal(File.size(path), partitions.last.byte_range.end)

    # Every partition starts at a record boundary: reading each
    # partition independently and concatenating must equal a serial
    # read, field by field.
    merged = partitions.flat_map {|partition| partition.read(path)}
    assert_equal(serial, merged)

    # The first row number of each partition indexes into the serial
    # row sequence.
    partitions.each do |partition|
      rows = partition.read(path)
      assert_equal(serial[partition.first_row_number, rows.size], rows)
    end
  end

  def nasty_content
    rows = [
      "id,name,note",
      "1,alice,\"line1\r\nline2\"",
      "2,日本語,\"絵文字🎉\"",
      "3,\"quote \"\"inside\"\"\",x",
      "",
      "4,\"has\nlone lf\",y",
      "5,\"has\rlone cr\",z",
      "6,last,\"no trailing newline\"",
    ]
    rows.join("\r\n")
  end

  def test_partitions_by_count
    with_csv_file(nasty_content) do |path|
      partitions = CSV::Partitioner.new(path).partitions(4)
      assert_partition_plan(path, partitions, {})
    end
  end

  def test_partitions_by_target_size
    with_csv_file(nasty_content) do |path|
      partitions = CSV::Partitioner.new(path).partitions(target_size: 16)
      assert_partition_plan(path, partitions, {})
    end
  end

  def test_tiny_target_size
    with_csv_file(nasty_content) do |path|
      partitions = CSV::Partitioner.new(path).partitions(target_size: 1)
      assert_partition_plan(path, partitions, {})
      # Every record gets its own partition.
      assert_equal((0...serial_rows(path, {}).size).to_a,
                   partitions.map(&:first_row_number))
    end
  end

  def test_tiny_scan_chunks
    with_csv_file(nasty_content) do |path|
      with_partition_chunk_size(3) do
        partitions = CSV::Partitioner.new(path).partitions(target_size: 5)
        assert_partition_plan(path, partitions, {})
      end
    end
  end

  def test_never_splits_quoted_field
    quoted = "\"#{"long\r\nquoted\r\nfield" * 20}\""
    content = +"a,b\n#{quoted},x\ny,z"
    with_csv_file(content) do |path|
      partitions = CSV::Partitioner.new(path).partitions(target_size: 10)
      # The huge quoted field cannot be split across partitions.
      assert_operator(partitions.size, :<, 10)
      assert_partition_plan(path, partitions, {})
    end
  end

  def test_never_splits_crlf
    with_csv_file(nasty_content) do |path|
      partitions = CSV::Partitioner.new(path).partitions(target_size: 2)
      data = File.binread(path)
      partitions.each do |partition|
        start = partition.byte_range.begin
        next if start.zero?

        refute_equal("\r", data.byteslice(start - 1, 1),
                     "partition starts between CR and LF at #{start}")
      end
      assert_partition_plan(path, partitions, {})
    end
  end

  def test_never_splits_multibyte_character
    with_csv_file(nasty_content) do |path|
      partitions = CSV::Partitioner.new(path).partitions(target_size: 2)
      partitions.each do |partition|
        bytes = File.open(path, "rb") do |file|
          file.seek(partition.byte_range.begin)
          file.read(partition.byte_range.size)
        end
        assert_predicate(bytes.dup.force_encoding(Encoding::UTF_8),
                         :valid_encoding?)
      end
      assert_partition_plan(path, partitions, {})
    end
  end

  def test_no_trailing_newline
    content = +"a,b,c\n1,2,3\n4,5,6"
    with_csv_file(content) do |path|
      partitions = CSV::Partitioner.new(path).partitions(2)
      assert_partition_plan(path, partitions, {})
      assert_equal(File.size(path), partitions.last.byte_range.end)
    end
  end

  def test_headers
    with_csv_file(nasty_content) do |path|
      partitions = CSV::Partitioner.new(path, headers: true).
                      partitions(target_size: 16)
      assert_partition_plan(path, partitions, {headers: true})

      assert_equal(["id", "name", "note"], partitions[0].headers)
      assert(partitions[0].include_headers?)
      partitions[1..].each do |partition|
        assert_not_predicate(partition, :include_headers?)
        assert_equal(["id", "name", "note"], partition.headers)
      end
      # Rows of later partitions still carry headers.
      assert_equal(serial_rows(path, {headers: true})[partitions[1].first_row_number],
                   partitions[1].read(path).first)
    end
  end

  def test_headers_array_option
    with_csv_file(nasty_content) do |path|
      headers = ["a", "b", "c"]
      partitions = CSV::Partitioner.new(path, headers: headers).
                      partitions(3)
      assert_nil(partitions[0].headers)
      assert_partition_plan(path, partitions, {headers: headers})
    end
  end

  def test_empty_file
    with_csv_file(+"") do |path|
      assert_equal([], CSV::Partitioner.new(path).partitions(3))
    end
  end

  def test_single_record_without_newline
    with_csv_file(+"a,b,c") do |path|
      partitions = CSV::Partitioner.new(path).partitions(4)
      assert_equal(1, partitions.size)
      assert_partition_plan(path, partitions, {})
    end
  end

  def test_more_partitions_than_records
    with_csv_file(+"a\nb\nc\n") do |path|
      partitions = CSV::Partitioner.new(path).partitions(10)
      assert_operator(partitions.size, :<=, 3)
      assert_partition_plan(path, partitions, {})
    end
  end

  def test_quote_char_nil
    content = +"a,\"b\",c\n1,2,3\n"
    with_csv_file(content) do |path|
      options = {quote_char: nil}
      partitions = CSV::Partitioner.new(path, **options).
                      partitions(target_size: 3)
      assert_partition_plan(path, partitions, options)
    end
  end

  def test_custom_separators
    content = +"a;'b\nb';c\n1;'x\ny'\n"
    with_csv_file(content) do |path|
      options = {col_sep: ";", quote_char: "'"}
      partitions = CSV::Partitioner.new(path, **options).
                      partitions(target_size: 4)
      assert_partition_plan(path, partitions, options)
    end
  end

  def test_skip_lines
    content = +"# comment\na,b\n# another\n1,2\n# trailing\n"
    with_csv_file(content) do |path|
      options = {skip_lines: /\A#/}
      partitions = CSV::Partitioner.new(path, **options).
                      partitions(target_size: 5)
      assert_partition_plan(path, partitions, options)
    end
  end

  def test_blank_lines
    content = +"a,b\n\n\n1,2\n\n"
    with_csv_file(content) do |path|
      [{}, {skip_blanks: true}].each do |options|
        partitions = CSV::Partitioner.new(path, **options).
                        partitions(target_size: 2)
        assert_partition_plan(path, partitions, options)
      end
    end
  end

  def test_liberal_parsing
    content = +"a,\"b\"junk,c\n1,2,3\n"
    with_csv_file(content) do |path|
      options = {liberal_parsing: true}
      partitions = CSV::Partitioner.new(path, **options).
                      partitions(target_size: 3)
      assert_partition_plan(path, partitions, options)
    end
  end

  def test_io_source
    with_csv_file(nasty_content) do |path|
      File.open(path, "rb:UTF-8") do |file|
        partitions = CSV::Partitioner.new(file).partitions(3)
        assert_partition_plan(path, partitions, {})
        # The IO position is preserved.
        assert_equal(0, file.pos)
        # Partitions can be read through the same IO.
        merged = partitions.flat_map {|partition| partition.read(file)}
        assert_equal(serial_rows(path, {}), merged)
      end
    end
  end

  def test_stringio_source
    io = StringIO.new(nasty_content)
    io.set_encoding(Encoding::UTF_8)
    partitions = CSV::Partitioner.new(io).partitions(3)
    serial = CSV.foreach(io, **{}).to_a
    merged = partitions.flat_map {|partition| partition.read(io)}
    assert_equal(serial, merged)
  end

  def test_threads
    with_csv_file(nasty_content) do |path|
      partitions = CSV::Partitioner.new(path).partitions(6)
      merged = partitions.map do |partition|
        Thread.new {partition.read(path)}
      end.flat_map(&:value)
      assert_equal(serial_rows(path, {}), merged)
    end
  end

  def test_manual_slice_read
    content = +"a,b\n1,2\n3,4\n"
    with_csv_file(content) do |path|
      partition = CSV::Partitioner.new(path).partitions(3)[1]
      rows = File.open(path, "rb:UTF-8") do |file|
        file.seek(partition.byte_range.begin)
        CSV.parse(file.read(partition.byte_range.size), row_sep: "\n")
      end
      assert_equal([["1", "2"]], rows)
    end
  end

  def test_invalid_arguments
    with_csv_file(+"a\n") do |path|
      partitioner = CSV::Partitioner.new(path)
      assert_raise(ArgumentError) {partitioner.partitions}
      assert_raise(ArgumentError) {partitioner.partitions(2, target_size: 1)}
      assert_raise(ArgumentError) {partitioner.partitions(0)}
      assert_raise(ArgumentError) {partitioner.partitions(target_size: 0)}
    end
  end

  def test_varied_boundaries
    # Deterministically generated content, split at tiny target sizes to
    # hit many different boundary positions; merging the partitions must
    # always equal one serial read, field by field.
    [
      [{}, "\r\n"],
      [{}, "\n"],
      [{headers: true}, "\r\n"],
      [{skip_blanks: true}, "\n"],
      [{quote_char: nil}, "\r\n"],
      [{col_sep: ";", quote_char: "'"}, "\n"],
    ].each_with_index do |(options, row_sep), index|
      rng = Random.new(index)
      rows = ["plain,simple,start"]
      quote_char = options.fetch(:quote_char, "\"")
      20.times do
        fields = (1..rng.rand(1..3)).map do
          case rng.rand(7)
          when 0 then "plain"
          when 1 then quote("quoted", options)
          when 2 then quote("with#{row_sep}newline", options)
          when 3 then quote("quote #{quote_char.to_s * 2}inside#{quote_char.to_s * 2}", options)
          when 4 then "日本語#{rng.rand(10)}"
          when 5 then quote("絵文字🎉", options)
          else ""
          end
        end
        rows << fields.join(options.fetch(:col_sep, ","))
      end
      content = rows.join(row_sep)
      content = content.chomp if index.odd?
      with_csv_file(content) do |path|
        [1, 3, 11].each do |target_size|
          partitions = CSV::Partitioner.new(path, **options).
                          partitions(target_size: target_size)
          assert_partition_plan(path, partitions, options)
        end
      end
    end
  end

  def quote(field, options)
    quote_char = options.fetch(:quote_char, "\"")
    return field unless quote_char

    "#{quote_char}#{field}#{quote_char}"
  end
end
