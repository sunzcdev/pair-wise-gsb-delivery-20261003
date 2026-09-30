# frozen_string_literal: true

require "tempfile"

class CSV
  # Note: Don't use this class directly. This is an internal class.
  #
  # CSV::ExternalSort implements a streaming, stable external sort for
  # CSV data that may be larger than the available memory.
  #
  # The input is consumed in bounded-size chunks: each chunk is sorted in
  # memory (stably) and spilled to a temporary file on disk.  The spilled
  # runs are then merged in repeated k-way merge passes, where at most
  # +max_open_files+ runs are opened at once.  The final pass streams the
  # sorted rows to the consumer.
  #
  # Stability across merge passes is preserved by tagging every record
  # with its global input sequence and breaking key ties on it.
  #
  # Each run stores the verbatim bytes of the input records, framed with
  # a length prefix, so headers, duplicated column names, nil values and
  # fields containing embedded newlines retain their original semantics.
  class ExternalSort
    # A run reader plus the record currently at its head.
    RunEntry = Struct.new(:reader, :raw_line, :row, :key, :sequence)

    # Framing for one persisted record: two 64-bit unsigned big-endian
    # integers (record byte length, global sequence) followed by the
    # verbatim bytes of the record.
    HEADER_FORMAT = "Q>"
    HEADER_SIZE = 8

    def initialize(path_or_io, mode, key,
                   buffer_size: 10_000,
                   max_open_files: 10,
                   temp_dir: nil,
                   **options)
      @path_or_io = path_or_io
      @mode = mode
      @key = key
      @options = options
      @buffer_size = buffer_size
      @max_open_files = max_open_files
      @temp_dir = temp_dir
      @sequence = 0
    end

    # Runs the whole sort, yielding rows in sorted order.  All temporary
    # files and file handles owned by this sort are released, even when
    # the key extractor raises, reading or writing fails, or the consumer
    # stops early.
    def each(&block)
      return to_enum(__method__) unless block_given?

      @run_paths = []
      @delete_paths = []
      @opened_files = []
      @input_csv = nil
      @input_file = nil
      @record_encoding = nil
      begin
        header_row, record_separator = spill_runs
        @record_separator = record_separator
        run_paths = merge_runs
        stream(run_paths, record_separator, header_row, &block)
      ensure
        cleanup
      end
    end

    private

    # Reads the input in bounded chunks, spills each sorted chunk to a
    # temporary file, and returns the header row (if any) and the input's
    # resolved record separator.
    def spill_runs
      header_row = nil
      record_separator = nil
      buffer = []

      input_csv = open_input
      # Resolve the record separator up front (instead of mid-iteration):
      # with headers it must be read before parser state advances.
      record_separator = input_csv.row_sep
      input_csv.each do |row|
        if row.is_a?(Row) and row.header_row?
          header_row = row
          next
        end

        raw_line = input_csv.line
        @record_encoding ||= raw_line.encoding
        buffer << [@key.call(row), @sequence, raw_line]
        @sequence += 1
        if buffer.size >= @buffer_size
          spill(buffer)
          buffer = []
        end
      end
      spill(buffer) unless buffer.empty?
      close_input

      [header_row, record_separator]
    end

    # Stably sorts one chunk and writes its raw records, in order, to a
    # new temporary file.  A unique, monotonic sequence number breaks
    # ties so equal keys keep their input order even after repeated merge
    # passes.
    #
    # Each framed record is an 8-byte big-endian unsigned length followed
    # by that many bytes of the verbatim input record, so records with
    # embedded newlines round-trip unchanged.
    def spill(buffer)
      ordered = buffer.each_with_index.to_a
      ordered.sort_by! { |entry, index| [entry[0], index] }

      path = new_run_file(["csv-sort-", ".run"]) do |tempfile|
        ordered.each do |(_key, sequence, raw_line), _index|
          write_record(tempfile, sequence, raw_line)
        end
      end
      path
    end

    def new_run_file(prefix_suffix)
      tempfile = Tempfile.new(prefix_suffix, @temp_dir)
      @opened_files << tempfile
      @run_paths << tempfile.path
      @delete_paths << tempfile.path
      tempfile.binmode
      yield tempfile
      tempfile.flush
      tempfile.path
    end

    def write_record(io, sequence, raw_line)
      bytes = raw_line.b
      io.write([bytes.bytesize].pack(HEADER_FORMAT))
      io.write([sequence].pack(HEADER_FORMAT))
      io.write(bytes)
    end

    # Repeatedly merges the existing runs, at most +max_open_files+ runs
    # at a time, until a single run remains.  Returns the paths of the
    # runs to consume.
    def merge_runs
      run_paths = @run_paths.dup
      while run_paths.size > 1
        next_paths = []
        run_paths.each_slice(@max_open_files) do |group|
          if group.size == 1
            next_paths << group.first
          else
            next_paths << merge_group(group, @record_separator)
          end
        end
        run_paths = next_paths
      end
      run_paths
    end

    # Performs one k-way merge pass over +group+ and returns the path of
    # the resulting run.  The global input sequence wins on equal keys,
    # which preserves stability through every merge pass.
    def merge_group(group, record_separator)
      group_files = []
      new_run_file(["csv-sort-merge-", ".run"]) do |output|
        heap = []
        group.each do |path|
          reader, file = open_run(path, record_separator)
          group_files << file
          entry = reader.next_entry
          heap_push(heap, entry) if entry
        end

        while (entry = heap_pop(heap))
          write_record(output, entry.sequence, entry.raw_line)
          next_entry = entry.reader.next_entry
          heap_push(heap, next_entry) if next_entry
        end
      end
    ensure
      release_group(group, group_files)
    end

    # Yields the header row followed by each data row of the final run in
    # sorted order.  With no runs (empty input) only the header is
    # yielded.
    def stream(run_paths, record_separator, header_row)
      yield header_row if header_row
      return if run_paths.empty?

      reader, file = open_run(run_paths.first, record_separator)
      begin
        while (entry = reader.next_entry)
          yield entry.row
        end
      ensure
        close_file(file)
        delete_run_file(run_paths.first)
      end
    end

    # Closes the files of one consumed merge group and removes the source
    # runs, keeping the number of simultaneously open handles at the
    # configured limit even across many passes.
    def release_group(paths, files)
      files.each { |file| close_file(file) }
      paths.each do |path|
        @run_paths.delete(path)
        delete_run_file(path)
      end
    end

    def delete_run_file(path)
      return unless path
      File.delete(path) if File.exist?(path)
      @delete_paths.delete(path)
    rescue SystemCallError
      nil
    end

    def close_file(file)
      file.close unless file.closed?
      @opened_files.delete(file)
    rescue IOError
      nil
    end

    # Releases the input as soon as the spill phase is done.  Only the
    # file opened by this sort for a path argument is closed here; an IO
    # supplied by the caller is left untouched, as in CSV.foreach.
    def close_input
      return unless @input_file
      close_file(@input_file)
      @input_file = nil
    end

    # Opens the input the same way CSV.foreach does, so paths and IO
    # streams are both supported.
    def open_input
      options = @options.dup
      if options[:headers] == true
        # Surface the header row once so it can be yielded first and
        # excluded from the sort.
        options[:return_headers] = true
      end
      if @path_or_io.is_a?(String)
        # Match CSV.open/CSV.foreach handling exactly: newline
        # translation is disabled and BOM detection may be enabled, so
        # verbatim records (including CRLF) are preserved.
        @input_csv = CSV.open(@path_or_io, @mode, **options)
        @input_file = @input_csv.to_io
        @opened_files << @input_file
      else
        @input_csv = CSV.new(@path_or_io, **options)
      end
      @input_csv
    end

    # Options for re-parsing a spilled raw record.  Runs contain raw data
    # only, so headers are provided explicitly when the input used
    # headers; this keeps field conversion identical to the original
    # parse and avoids treating the first data row as a header.
    def run_options(record_separator = nil)
      options = @options.dup
      headers_option = options[:headers]
      if headers_option == true
        options[:headers] = @input_csv.headers
      end
      options[:return_headers] = false
      # The spilled bytes are already in the parser's working encoding,
      # so they must not be transcoded again.
      options.delete(:encoding)
      options.delete(:internal_encoding)
      options.delete(:external_encoding)
      if record_separator
        # A spilled record is parsed on its own; with +:auto+ a single
        # record could be misdetected when it embeds another separator.
        options[:row_sep] = record_separator
      end
      options
    end

    def open_run(path, record_separator = nil)
      file = File.open(path, "rb")
      @opened_files << file
      [RunReader.new(file,
                     run_options(record_separator),
                     @record_encoding,
                     @key), file]
    end

    def heap_push(heap, entry)
      heap << entry
      sift_up(heap, heap.size - 1)
    end

    def heap_pop(heap)
      return nil if heap.empty?
      head = heap.first
      tail = heap.pop
      unless heap.empty?
        heap[0] = tail
        sift_down(heap, 0)
      end
      head
    end

    def sift_up(heap, index)
      while index > 0
        parent = (index - 1) / 2
        break if entry_compare(heap[index], heap[parent]) >= 0
        heap[index], heap[parent] = heap[parent], heap[index]
        index = parent
      end
    end

    def sift_down(heap, index)
      limit = heap.size
      loop do
        left = index * 2 + 1
        break if left >= limit
        right = left + 1
        child =
          if right < limit && entry_compare(heap[right], heap[left]) < 0
            right
          else
            left
          end
        break if entry_compare(heap[index], heap[child]) <= 0
        heap[index], heap[child] = heap[child], heap[index]
        index = child
      end
    end

    # Orders by sort key, then by global input sequence.  This keeps
    # equal-keyed rows in input order through every merge pass.
    def entry_compare(left, right)
      comparison = (left.key <=> right.key)
      return comparison unless comparison == 0
      left.sequence <=> right.sequence
    end

    # Closes every handle we opened and removes every temporary file we
    # created.  Safe to call repeatedly.
    def cleanup
      @opened_files.each do |file|
        file.close if file.respond_to?(:close) && !file.closed?
      rescue IOError
        nil
      end
      @delete_paths.each do |path|
        File.delete(path) if path && File.exist?(path)
      rescue SystemCallError
        nil
      end
      @opened_files.clear
      @run_paths.clear
      @delete_paths.clear
    end

    # Reads framed raw records from one spilled run and re-parses each
    # record with the original parsing options.
    class RunReader
      def initialize(io, reparse_options, record_encoding, key)
        @io = io
        @reparse_options = reparse_options
        @record_encoding = record_encoding
        @key = key
      end

      def next_entry
        header = read_bytes(HEADER_SIZE)
        return nil if header.nil?
        length = header.unpack1(HEADER_FORMAT)
        sequence_header = read_bytes(HEADER_SIZE)
        raise EOFError, "unexpected end of CSV sort run" if sequence_header.nil?
        sequence = sequence_header.unpack1(HEADER_FORMAT)
        raw_line = read_bytes(length)
        raise EOFError, "unexpected end of CSV sort run" if raw_line.nil?
        raw_line.force_encoding(@record_encoding) if @record_encoding
        csv = CSV.new(raw_line, **@reparse_options)
        row = csv.shift
        RunEntry.new(self, raw_line, row, @key.call(row), sequence)
      end

      private

      def read_bytes(length)
        bytes = @io.read(length)
        return nil if bytes.nil?
        raise EOFError, "unexpected end of CSV sort run" if bytes.bytesize < length
        bytes
      end
    end
  end
end
