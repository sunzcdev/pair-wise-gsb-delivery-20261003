# frozen_string_literal: true

require "tempfile"

class CSV
  # Raised by CSV.transpose when input rows have different numbers of
  # fields and the caller has not opted into padding.
  class UnevenRowError < MalformedCSVError
  end

  # Note: Don't use this class directly. This is an internal class used
  # by CSV.transpose.
  #
  # Transposes an arbitrarily large CSV matrix with bounded resources:
  #
  # - The input stream is read exactly once, front to back.
  # - At most +max_buffered_fields+ parsed fields are held in memory.
  # - At most +max_open_files+ spill files are open at the same time,
  #   independently of how many columns the input has.
  # - No column is ever accumulated wholly in memory and no file
  #   descriptor is kept open permanently for a column.
  #
  # Each input column gets its own spill file on disk. Records within a
  # column are appended in input row order as length-prefixed binary
  # records, so the (possibly much wider) output rows are emitted by
  # reading one column file at a time, in lockstep.
  class Transposer
    # Header of each spill record: input row index, byte length of the
    # following field, and a nil flag (1 when the field is nil), all
    # packed big-endian. Keeping nil distinct from an empty String
    # preserves the CSV distinction between an unquoted empty field and
    # a quoted empty field.
    RECORD_HEADER = "Q>2C"
    RECORD_HEADER_SIZE = 17
    NIL_FLAG = 1
    STRING_FLAG = 0

    DEFAULT_MAX_BUFFERED_FIELDS = 8192
    DEFAULT_MAX_OPEN_FILES = 8

    def initialize(input, writer,
                   max_buffered_fields: DEFAULT_MAX_BUFFERED_FIELDS,
                   max_open_files: DEFAULT_MAX_OPEN_FILES,
                   ragged_rows: :raise,
                   pad_value: nil,
                   tmpdir: nil,
                   output_encoding: nil)
      @input = input
      @writer = writer
      @max_buffered_fields = max_buffered_fields
      @ragged_rows = ragged_rows
      @pad_value = pad_value
      @output_encoding = output_encoding

      unless [:raise, :pad].include?(@ragged_rows)
        raise ArgumentError,
              ":ragged_rows must be :raise or :pad: " +
              @ragged_rows.inspect
      end
      unless max_buffered_fields.is_a?(Integer) and
             max_buffered_fields >= 1
        raise ArgumentError,
              ":max_buffered_fields must be an Integer >= 1: " +
              max_buffered_fields.inspect
      end
      unless max_open_files.is_a?(Integer) and max_open_files >= 1
        raise ArgumentError,
              ":max_open_files must be an Integer >= 1: " +
              max_open_files.inspect
      end

      # column index -> array of [row_index, field] pairs
      @buffers = {}
      @buffered_field_count = 0
      @row_count = 0
      @column_count = 0
      # number of fields in each input row, for precise error reporting
      @row_widths = []

      @spills = new_spill_pool(max_open_files, tmpdir, input.encoding)
    end

    # Reads the whole input once, spills it to column files, and writes
    # the transposed matrix to the output.
    def transpose
      spill
      # All input has been spilled and validated; close every writer
      # handle before opening readers so the write and read phases never
      # hold spill handles at the same time. From here on at most one
      # column file (the one being emitted) is open.
      @spills.close_writers
      emit
    ensure
      # All spill files are closed and removed whether we succeeded or
      # raised, so no temporary resource survives the call.
      @spills.close_writers
      @spills.cleanup_all
    end

    private

    # Indirection so tests can substitute an instrumented pool.
    def new_spill_pool(max_open_files, tmpdir, encoding)
      SpillPool.new(max_open_files, tmpdir, encoding)
    end

    #
    # Phase 1: stream the input exactly once. Fields are buffered per
    # column and flushed to that column's spill file whenever the field
    # budget is reached.
    #
    def spill
      @input.each do |row|
        width = row.length
        @row_widths << width
        @column_count = width if width > @column_count
        row.each_with_index do |field, column_index|
          add_field(column_index, field)
        end
        @row_count += 1
      end

      flush_all

      if @ragged_rows == :raise
        first_width = @row_widths.first
        offset = @row_widths.index {|width| width != first_width}
        if offset
          raise CSV::UnevenRowError.new(
                  "rows have different numbers of fields",
                  1 + offset)
        end
      end
    end

    def add_field(column_index, field)
      (@buffers[column_index] ||= []) << [@row_count, field]
      @buffered_field_count += 1
      return unless @buffered_field_count >= @max_buffered_fields

      # Flush the fullest buffer so the most headroom is reclaimed.
      fullest_column = nil
      fullest_size = 0
      @buffers.each do |index, buffer|
        if buffer.length > fullest_size
          fullest_column = index
          fullest_size = buffer.length
        end
      end
      flush_column(fullest_column)
    end

    def flush_column(column_index)
      buffer = @buffers.delete(column_index)
      return if buffer.nil? or buffer.empty?

      spill = @spills.writer_for(column_index)
      buffer.each do |row_index, field|
        bytes, nil_flag = field_binary_bytes(field)
        spill.write([row_index, bytes.bytesize, nil_flag].pack(RECORD_HEADER))
        spill.write(bytes)
      end
      # Flush to the OS at once so the bytes are durable and visible to
      # the read phase. The handle itself stays open in the pool's LRU
      # cache (still counted against the open-file limit); when more
      # distinct columns than the limit are written, the pool closes and
      # reopens the least recently used handle, reopening in append mode.
      spill.flush
      @buffered_field_count -= buffer.length
    end

    def flush_all
      @buffers.keys.each do |column_index|
        flush_column(column_index)
      end
      @buffers.clear
      @buffered_field_count = 0
    end

    def field_binary_bytes(field)
      if field.nil?
        ["".b, NIL_FLAG]
      else
        [String(field).b, STRING_FLAG]
      end
    end

    #
    # Phase 2: open each column file one at a time and write its fields
    # as one output row, padding gaps when the caller opted in. Only one
    # spill file is open during this whole phase.
    #
    def emit
      return if @column_count.zero?

      @column_count.times do |column_index|
        @writer.begin_streaming_row

        emitted = 0
        reader = @spills.reader_for(column_index)
        if reader
          reader.each do |row_index, field|
            if @ragged_rows == :pad
              while emitted < row_index
                @writer.emit_streaming_field(encode_field(@pad_value))
                emitted += 1
              end
            elsif emitted != row_index
              # Rectangularity was validated in phase 1; a gap means the
              # spill data is corrupt.
              raise CSV::MalformedCSVError.new(
                      "corrupted transpose spill data", 0)
            end
            @writer.emit_streaming_field(encode_field(field))
            emitted += 1
          end
          reader.close
        end

        if @ragged_rows == :pad
          while emitted < @row_count
            @writer.emit_streaming_field(encode_field(@pad_value))
            emitted += 1
          end
        end

        @writer.end_streaming_row
        # The column file is finished; release its disk space at once.
        @spills.unlink(column_index)
      end
    end

    # Transcodes a field from the input encoding to the requested output
    # encoding. nil is passed through untouched (written as an empty
    # field). Fields that are already in (or ASCII-compatible with) the
    # target encoding are returned unchanged.
    def encode_field(field)
      return field if field.nil? or @output_encoding.nil?
      return field if field.encoding == @output_encoding
      field.encode(@output_encoding)
    end

    #
    # Owns the per-column spill files and bounds the number of them that
    # are open simultaneously. Writer handles are kept in a least
    # recently used cache of open IOs; callers open at most one reader at
    # a time.
    #
    class SpillPool
      def initialize(max_open_files, tmpdir, encoding)
        @max_open_files = max_open_files
        @tmpdir = tmpdir
        @encoding = encoding
        # column index -> closed Tempfile (kept alive and used for path)
        @tempfiles = {}
        # column index -> open writable IO, LRU order (least used first)
        @writers = {}
      end

      # Returns an IO positioned for appending to the given column. May
      # close the least recently used open writer to stay within the
      # open-file budget.
      def writer_for(column_index)
        handle = @writers.delete(column_index)
        unless handle
          evict if @writers.length >= @max_open_files
          handle = File.open(tempfile(column_index).path, "ab")
        end
        @writers[column_index] = handle
        handle
      end

      # Returns a Reader positioned at the start of the column file, or
      # nil if the column never received a record.
      def reader_for(column_index)
        tempfile = @tempfiles[column_index]
        return nil if tempfile.nil?

        Reader.new(File.open(tempfile.path, "rb"), @encoding)
      end

      # Closes the column's writer handle (if any) and removes its file.
      def unlink(column_index)
        @writers.delete(column_index)&.close
        @tempfiles.delete(column_index)&.close!
      rescue IOError, SystemCallError
        # The file may already be closed or gone; that is the goal.
      end

      # Closes all currently open writer handles.
      def close_writers
        @writers.each_value(&:close)
        @writers.clear
      end

      def cleanup_all
        close_writers
        @tempfiles.each_value(&:close!)
        @tempfiles.clear
      rescue IOError, SystemCallError
        # Best effort: every remaining file must disappear.
      end

      private

      def tempfile(column_index)
        @tempfiles[column_index] ||= begin
          file = Tempfile.new(
            ["csv-transpose-#{column_index}-", ".bin"],
            @tmpdir)
          # Close the Tempfile's own handle immediately: all later access
          # goes through explicitly opened IOs, which we count and close.
          file.close
          file
        end
      end

      def evict
        _column_index, handle = @writers.shift
        handle&.close
      end

      # Sequentially reads length-prefixed records from one spill file.
      class Reader
        def initialize(io, encoding)
          @io = io
          @encoding = encoding
        end

        def each
          header = @io.read(Transposer::RECORD_HEADER_SIZE)
          until header.nil?
            if header.bytesize != Transposer::RECORD_HEADER_SIZE
              raise CSV::MalformedCSVError.new(
                      "corrupted transpose spill data", 0)
            end
            row_index, byte_size, nil_flag =
              header.unpack(Transposer::RECORD_HEADER)
            bytes = @io.read(byte_size)
            if bytes.nil? or bytes.bytesize != byte_size
              raise CSV::MalformedCSVError.new(
                      "corrupted transpose spill data", 0)
            end
            field =
              if nil_flag == Transposer::NIL_FLAG
                nil
              else
                bytes.force_encoding(@encoding)
              end
            yield(row_index, field)
            header = @io.read(Transposer::RECORD_HEADER_SIZE)
          end
        end

        def close
          @io.close
        end
      end
    end
  end
end
