# frozen_string_literal: true

require "tempfile"
require "tmpdir"

class CSV
  # Error raised when input rows have differing field counts and the
  # caller did not request padding for short rows.
  class RaggedRowError < StandardError
  end

  # Note: Don't use this class directly. This is an internal class.
  #
  # External-memory row/column transposition.
  #
  # The input is streamed exactly once. Its columns are partitioned
  # into stripes of at most +max_buffered_fields+ fields; each stripe
  # is backed by one temporary file containing, for every input row,
  # one record with the fields of that stripe. Stripe files are
  # row-aligned: record +r+ in every stripe belongs to input row +r+.
  #
  # At most +max_open_files+ stripe files are open at once. A closed
  # stripe file is reopened (and rewound) on demand, so stripes never
  # permanently hold a file handle, even when the table is wider than
  # the handle limit.
  #
  # During output the transposed rows are assembled one field at a
  # time, so no output column is ever accumulated in memory.
  class Transposer
    #
    # Prefix used for the temporary files created by a transposition.
    #
    TEMPFILE_PREFIX = "csv-transpose-"
    #
    # Default bound on buffered fields per stripe chunk. It is also
    # the stripe width: the fields of one input row that belong to a
    # stripe, and the field buffer used while emitting output, are
    # both bounded by it.
    #
    DEFAULT_MAX_BUFFERED_FIELDS = 4096
    # Default bound on simultaneously open temporary file handles.
    DEFAULT_MAX_OPEN_FILES = 8

    attr_reader :max_buffered_fields
    attr_reader :max_open_files

    # :call-seq:
    #   CSV::Transposer.new(input_csv, output_csv, **options)
    #
    # +input_csv+ and +output_csv+ are already configured CSV objects;
    # the input must be readable once, the output writable.
    #
    # Options:
    # - +:ragged_rows+: +:raise+ (the default) rejects rows whose
    #   field count differs from the widest row seen so far; any other
    #   value is used as the explicit field value written into the
    #   holes of short rows.
    # - +:max_buffered_fields+: bound on buffered fields (stripe
    #   width); must be at least 1.
    # - +:max_open_files+: bound on simultaneously open temporary
    #   files; must be at least 1.
    # - +:tmpdir+: directory used for temporary files.
    def initialize(input, output,
                   ragged_rows: :raise,
                   max_buffered_fields: DEFAULT_MAX_BUFFERED_FIELDS,
                   max_open_files: DEFAULT_MAX_OPEN_FILES,
                   tmpdir: nil,
                   return_string: false)
      @input = input
      @output = output
      @return_string = return_string
      @ragged_rows = ragged_rows
      @max_buffered_fields = check_positive(max_buffered_fields,
                                            :max_buffered_fields)
      @max_open_files = check_positive(max_open_files, :max_open_files)
      @tmpdir = tmpdir || Dir.tmpdir
    end

    # Performs the transposition.
    #
    # Returns the generated String when +return_string+ was set (the
    # output writes to a String destination); otherwise returns the
    # output CSV object itself.
    def transpose
      Dir.mktmpdir(TEMPFILE_PREFIX, @tmpdir) do |dir|
        pool = StripeFilePool.new(dir, @max_open_files, stripe_options)
        begin
          spill(pool)
          assemble(pool)
        ensure
          pool.close_all
        end
      end
      if @return_string
        @output.string
      else
        @output
      end
    end

    private

    def check_positive(value, name)
      value = Integer(value)
      unless value >= 1
        raise ArgumentError, "#{name} must be at least 1: #{value}"
      end
      value
    end

    # Single pass over the input: append each row's stripe chunks to
    # the matching stripe files, creating stripes on demand and
    # keeping all stripe files row-aligned.
    def spill(pool)
      width = nil
      stripe_count = 0
      row_count = 0

      @input.each do |row|
        fields = row.is_a?(CSV::Row) ? row.fields : Array(row)
        row_width = fields.size

        if width.nil?
          # The first row fixes the initial shape.
          width = row_width
          stripe_count = width.zero? ? 0 : stripe_index_of(width - 1) + 1
          stripe_count.times { |stripe| pool.create(stripe) }
        elsif @ragged_rows == :raise and row_width != width
          raise RaggedRowError,
                "row #{row_count + 1} has #{row_width} fields, " +
                "expected #{width}"
        elsif row_width > width
          # Grow on demand; back-fill new stripes for earlier rows.
          needed = stripe_index_of(row_width - 1) + 1
          while stripe_count < needed
            pool.create(stripe_count)
            pad = Array.new(@max_buffered_fields, @ragged_rows)
            row_count.times { pool.append(stripe_count, pad) }
            stripe_count += 1
          end
        end

        append_row(pool, fields, row_width, stripe_count, width)
        row_count += 1
        width = row_width if row_width > width
      end

      @row_count = row_count
      @stripe_count = stripe_count
      @width = width || 0
    end

    # Writes the row's chunk into every existing stripe. Missing
    # trailing fields (short rows in pad mode) are filled with the
    # explicit pad value; stripes for future wider rows do not exist
    # yet and get their records when such a row grows the table.
    def append_row(pool, fields, row_width, stripe_count, width)
      stripe_count.times do |stripe|
        start_col = stripe * @max_buffered_fields
        chunk = fields[start_col, @max_buffered_fields] || []
        present = start_col + chunk.size
        if present < width
          chunk = chunk + Array.new(width - present, @ragged_rows)
        end
        pool.append(stripe, chunk)
      end
    end

    # Emits the transposed rows. Output column +c+ is the local column
    # of stripe +c / max_buffered_fields+; fields are streamed in
    # stripe order and records are streamed in input-row order.
    def assemble(pool)
      return if @row_count.zero? || @width.nil? || @width.zero?

      @stripe_count.times do |stripe|
        local_width =
          if stripe == @stripe_count - 1
            @width - stripe * @max_buffered_fields
          else
            @max_buffered_fields
          end
        local_width.times do |local_col|
          row_index = 0
          pool.records(stripe) do |record|
            field = local_col < record.size ? record[local_col] : nil
            @output.add_fields([field])
            row_index += 1
          end
          # Rows never reaching this stripe are trailing holes.
          while row_index < @row_count
            @output.add_fields([nil])
            row_index += 1
          end
          @output.finish_row
        end
      end
      @output.flush if @output.respond_to?(:flush)
    end

    def stripe_index_of(column)
      column / @max_buffered_fields
    end

    # Format options shared with the temporary stripe files. The
    # input encoding is used so round-tripping through disk preserves
    # field bytes.
    def stripe_options
      {
        col_sep: @output.col_sep,
        row_sep: "\n",
        quote_char: @output.quote_char,
        encoding: @input.encoding,
      }
    end

    # LRU pool of open stripe file handles.
    #
    # Each stripe has exactly one backing file for its whole lifetime;
    # it is appended to during spilling and re-read several times
    # during assembly. Handles may be closed at any time to respect
    # the open-file bound and are reopened (and positioned) on demand.
    class StripeFilePool
      attr_reader :open_count
      attr_reader :peak_open_count

      def initialize(directory, max_open_files, csv_options)
        @directory = directory
        @max_open_files = max_open_files
        @csv_options = csv_options
        @paths = {}
        @handles = {}
        # Ordered Hash: the first pair is the least recently used.
        @lru = {}
        @open_count = 0
        @peak_open_count = 0
      end

      # Creates the backing file for a new stripe, opened for
      # writing/reading.
      def create(stripe)
        path = File.join(@directory, "stripe-#{stripe}.csv")
        @paths[stripe] = path
        io = open_handle(path, "wb+")
        @handles[stripe] = io
        touch(stripe)
        make_room
      end

      # Appends one record (an Array of fields) to a stripe.
      def append(stripe, record)
        io = acquire_for_writing(stripe)
        io.write(CSV.generate_line(record, **@csv_options))
      end

      # Streams each parsed record of a stripe from its beginning.
      # The handle used for reading is closed afterwards, so a stripe
      # only holds a handle while it is actively being read.
      def records(stripe)
        finish_writes(stripe)
        io = acquire_for_reading(stripe)
        io.seek(0, IO::SEEK_SET)
        begin
          CSV.new(io, **@csv_options).each do |record|
            yield record
          end
        ensure
          close_one(stripe)
        end
      end

      # Closes every open handle. The files themselves are removed by
      # the caller's temporary-directory cleanup.
      def close_all
        @handles.each_value do |io|
          io.close unless io.closed?
        end
        @handles.clear
        @lru.clear
        @open_count = 0
      end

      private

      def acquire_for_writing(stripe)
        io = @handles[stripe]
        if io
          touch(stripe)
          make_room(stripe)
          io
        else
          make_room(stripe)
          io = open_handle(@paths.fetch(stripe), "rb+")
          io.seek(0, IO::SEEK_END)
          @handles[stripe] = io
          touch(stripe)
          io
        end
      end

      def acquire_for_reading(stripe)
        io = @handles[stripe]
        if io
          touch(stripe)
          return io
        end
        make_room(stripe)
        io = open_handle(@paths.fetch(stripe), "rb")
        @handles[stripe] = io
        touch(stripe)
        io
      end

      def finish_writes(stripe)
        @handles[stripe]&.flush
      end

      # Closes least-recently-used handles until opening one more
      # handle would stay within the bound. The +protected+ stripe
      # (the one the caller is about to use) is never evicted here.
      def make_room(protected = nil)
        while @open_count >= @max_open_files
          victim, = (@lru.keys - [protected]).first
          break if victim.nil?
          close_one(victim)
        end
      end

      def touch(stripe)
        @lru.delete(stripe)
        @lru[stripe] = true
      end

      def close_one(stripe)
        io = @handles.delete(stripe)
        return unless io
        io.close unless io.closed?
        @lru.delete(stripe)
        @open_count -= 1
      end

      def open_handle(path, mode)
        io = File.open(path, mode)
        io.binmode
        encoding = @csv_options[:encoding]
        io.set_encoding(encoding) if encoding
        @open_count += 1
        @peak_open_count = @open_count if @open_count > @peak_open_count
        io
      end
    end
  end
end
