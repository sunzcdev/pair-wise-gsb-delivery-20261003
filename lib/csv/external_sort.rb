# frozen_string_literal: true

require "fileutils"
require "tmpdir"

class CSV
  # == CSV::ExternalSort
  #
  # External (on-disk) stable sort for CSV data that may be larger than
  # available memory.
  #
  # Records are read from the input as a stream, sorted in bounded in-memory
  # chunks that are spilled to temporary files, and then merged back with a
  # k-way merge that never opens more than +merge_fanin+ temporary files at
  # the same time.  Records whose sort keys compare equal are yielded in
  # their original input order, no matter how many merge rounds are needed.
  #
  # Only Ruby standard libraries (+tmpdir+ and +fileutils+) are used for the
  # temporary storage.  All temporary files and handles of a sort run are
  # released when the run finishes, when the key extractor or comparator
  # raises, when reading or writing fails, or when the consumer stops
  # iteration early (use the block form of #each for deterministic cleanup).
  #
  # Example:
  #
  #   sorter = CSV::ExternalSort.new("ledger.csv",
  #                                  key: "account",
  #                                  headers: true,
  #                                  chunk_size: 10_000,
  #                                  merge_fanin: 8)
  #   sorter.each do |row|
  #     puts row.to_csv
  #   end
  #
  # Options:
  #
  # [+key+]
  #   How to extract the sort key from a record.  A column name or index
  #   (used as <tt>row[key]</tt>), a callable invoked with each record, or
  #   +nil+ to compare records themselves.  Defaults to +nil+.
  # [+compare+]
  #   An optional callable receiving two sort keys and returning a negative
  #   number, zero, or a positive number, like <tt><=></tt>.  Defaults to
  #   comparing keys with <tt><=></tt>.
  # [+chunk_size+]
  #   Maximum number of records buffered in memory before spilling a sorted
  #   chunk to disk.  Must be >= 1.  Defaults to 10_000.
  # [+merge_fanin+]
  #   Maximum number of chunk files opened simultaneously while merging.
  #   Must be >= 2.  Defaults to 8.
  #
  # Any other options (such as +:headers+, +:col_sep+, +:converters+) are
  # forwarded to CSV when parsing the input.  +:return_headers+ is managed
  # internally and therefore ignored.
  class ExternalSort
    include Enumerable

    # Options that affect the on-disk chunk representation when writing.
    CHUNK_WRITE_OPTIONS = %i[
      col_sep
      force_quotes
      quote_char
      quote_empty
      row_sep
    ].freeze

    # Options that affect how chunks are parsed back.
    CHUNK_READ_OPTIONS = %i[
      col_sep
      converters
      quote_char
    ].freeze

    def initialize(input, key: nil, compare: nil, chunk_size: 10_000,
                   merge_fanin: 8, **options)
      @input = input
      @key = key
      @compare = compare
      @chunk_size = Integer(chunk_size)
      if @chunk_size < 1
        raise ArgumentError, "chunk_size must be a positive integer"
      end
      @merge_fanin = Integer(merge_fanin)
      if @merge_fanin < 2
        raise ArgumentError, "merge_fanin must be an integer >= 2"
      end
      @read_options = options.reject {|name, _| name == :return_headers}
      @chunk_write_options = options.slice(*CHUNK_WRITE_OPTIONS)
      @chunk_read_options = options.slice(*CHUNK_READ_OPTIONS)
      @headers = nil
    end

    # The headers of the input as an Array (including duplicated column
    # names), or +nil+ when the input has no headers.  Populated once #each
    # has started.
    attr_reader :headers

    # Yields each record in sorted order.  Records are CSV::Row objects when
    # the +:headers+ option is in effect, Arrays otherwise.  Without a block,
    # returns an Enumerator; note that only the block form releases temporary
    # files deterministically when iteration stops early.
    def each
      return to_enum(:each) unless block_given?

      dir = Dir.mktmpdir("csv-external-sort-")
      begin
        @headers = nil
        chunks = split_into_chunks(dir)
        chunks = merge_rounds(dir, chunks)
        stream_chunks(chunks) {|row| yield row}
      ensure
        FileUtils.remove_entry(dir) if File.exist?(dir)
      end
      self
    end

    private

    def split_into_chunks(dir)
      chunks = []
      buffer = []
      sequence = 0
      open_input do |csv|
        csv.each do |row|
          @headers ||= row.headers if row.is_a?(CSV::Row)
          buffer << [extract_key(row), sequence, row]
          sequence += 1
          if buffer.size >= @chunk_size
            chunks << write_chunk(dir, "chunk-%06d.csv" % chunks.size, buffer)
            buffer = []
          end
        end
        headers = csv.headers
        @headers ||= headers if headers.is_a?(Array)
      end
      unless buffer.empty?
        chunks << write_chunk(dir, "chunk-%06d.csv" % chunks.size, buffer)
      end
      chunks
    end

    def merge_rounds(dir, chunks)
      round = 0
      while chunks.size > @merge_fanin
        merged = []
        chunks.each_slice(@merge_fanin) do |group|
          if group.size == 1
            merged << group.first
          else
            name = "merge-%02d-%06d.csv" % [round, merged.size]
            path = File.join(dir, name)
            merge_chunk_files(group, path)
            merged << path
          end
        end
        chunks = merged
        round += 1
      end
      chunks
    end

    def merge_chunk_files(paths, out_path)
      readers = paths.map {|path| new_chunk_reader(path)}
      begin
        CSV.open(out_path, "w", **@chunk_write_options) do |csv|
          each_merged_entry(readers) do |_key, sequence, row|
            csv << [sequence, *row_fields(row)]
          end
        end
      ensure
        readers.each(&:close)
      end
      out_path
    end

    def stream_chunks(chunks)
      readers = chunks.map {|path| new_chunk_reader(path)}
      begin
        each_merged_entry(readers) do |_key, _sequence, row|
          yield row
        end
      ensure
        readers.each(&:close)
      end
    end

    def each_merged_entry(readers)
      loop do
        best = nil
        best_reader = nil
        readers.each do |reader|
          entry = reader.entry
          next if entry.nil?
          if best.nil? or compare_entries(entry, best) < 0
            best = entry
            best_reader = reader
          end
        end
        break if best.nil?
        yield best
        best_reader.advance
      end
    end

    # Entries are [key, sequence, row]; the input sequence breaks ties so
    # that the sort is stable across chunking and every merge round.
    def compare_entries(a, b)
      cmp = if @compare
        @compare.call(a[0], b[0])
      else
        a[0] <=> b[0]
      end
      if cmp.nil?
        raise ArgumentError,
              "comparison of #{a[0].inspect} with #{b[0].inspect} failed"
      end
      cmp = a[1] <=> b[1] if cmp.zero?
      cmp
    end

    def extract_key(row)
      case @key
      when nil
        row
      when Proc, Method
        @key.call(row)
      else
        if @key.respond_to?(:call)
          @key.call(row)
        else
          row[@key]
        end
      end
    end

    def row_fields(row)
      row.is_a?(CSV::Row) ? row.fields : row.to_a
    end

    def write_chunk(dir, name, buffer)
      buffer.sort! {|a, b| compare_entries(a, b)}
      path = File.join(dir, name)
      CSV.open(path, "w", **@chunk_write_options) do |csv|
        buffer.each do |_key, sequence, row|
          csv << [sequence, *row_fields(row)]
        end
      end
      path
    end

    def new_chunk_reader(path)
      ChunkReader.new(path, @headers, @chunk_read_options) do |row|
        extract_key(row)
      end
    end

    def open_input
      if @input.respond_to?(:to_str) or @input.respond_to?(:to_path)
        CSV.open(@input.to_s, "r", **@read_options) do |csv|
          yield csv
        end
      else
        yield CSV.new(@input, **@read_options)
      end
    end

    # Reads one temporary chunk file.  The first column of every stored
    # record is the input sequence number used for stable tie-breaking; the
    # remaining columns are the original fields.
    class ChunkReader
      attr_reader :entry

      def initialize(path, headers, csv_options, &key_extractor)
        @io = File.open(path, "r")
        begin
          @csv = CSV.new(@io, **csv_options)
          @headers = headers
          @key_extractor = key_extractor
          advance
        rescue Exception
          @io.close unless @io.closed?
          raise
        end
      end

      def advance
        record = @csv.gets
        if record.nil?
          @entry = nil
        else
          sequence = Integer(record.shift)
          fields = record
          row = @headers ? CSV::Row.new(@headers, fields) : fields
          @entry = [@key_extractor.call(row), sequence, row]
        end
      end

      def close
        @io.close unless @io.closed?
        nil
      end
    end
  end
end
