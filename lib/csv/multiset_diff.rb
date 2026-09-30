# frozen_string_literal: true

require "tempfile"

class CSV
  # Streaming multiset comparison of two key-sorted CSV row streams.
  #
  # Both inputs must be sorted by the business key extracted with the
  # +key+ callable.  Rows that share the same key form a group.  Within
  # a group, rows whose content (extracted with the +content+ callable)
  # is equal are paired one-to-one by count; any remaining rows are
  # reported as +:left_only+ or +:right_only+ differences.  Two rows
  # with different content are never reported as a "modification" --
  # they show up as one leftover on each side.
  #
  # Memory usage is bounded: at most +max_group_size+ rows of a single
  # key group are kept in memory.  Larger groups are spilled to
  # Tempfile storage and paired with a chunked external sort.  The
  # whole input is never loaded into a Hash.
  #
  #   diff = CSV::MultisetDiff.new(
  #     key:     ->(row) { row["id"] },
  #     content: ->(row) { row.fields },
  #     max_group_size: 1_000,
  #   )
  #   diff.each(CSV.foreach("old.csv", headers: true),
  #             CSV.foreach("new.csv", headers: true)) do |difference|
  #     p [difference.side, difference.key, difference.row]
  #   end
  class MultisetDiff
    # A single reported difference: a row that exists only on one side
    # (after equal rows have been paired by count within its key group).
    Difference = Struct.new(:side, :key, :row)

    # Raised when an input is not sorted by the selected key.  Carries
    # the +side+ (:left or :right), the 1-based record +position+ in
    # that input, the +previous_key+ and the offending +key+.
    class UnorderedInputError < StandardError
      attr_reader :side, :position, :previous_key, :key

      def initialize(side, position, previous_key, key)
        @side = side
        @position = position
        @previous_key = previous_key
        @key = key
        super("#{side} input is not sorted: record #{position} has key " \
              "#{key.inspect}, which sorts before the previous key " \
              "#{previous_key.inspect}")
      end
    end

    DEFAULT_MAX_GROUP_SIZE = 10_000

    # +key+ extracts the business key from a row; keys must be
    # comparable with <=> and both inputs must be sorted by it.
    # +content+ extracts the value used to decide whether two rows in
    # the same key group are identical; it must have value semantics
    # for hash/eql? (defaults to the row itself).  +max_group_size+
    # bounds how many rows of one key group are held in memory before
    # spilling to temporary files.
    def initialize(key:, content: nil, max_group_size: DEFAULT_MAX_GROUP_SIZE)
      @key_of = key
      @content_of = content || ->(row) { row }
      @max_group_size = Integer(max_group_size)
      if @max_group_size < 1
        raise ArgumentError, "max_group_size must be positive"
      end
      @live_tempfiles = []
    end

    # Tempfiles currently held by an in-progress comparison.  Empty
    # once a comparison finishes or is aborted (e.g. when the consumer
    # of #each raises or breaks out early).
    attr_reader :live_tempfiles

    # Streams the differences between +left+ and +right+ (any
    # enumerables of rows).  Yields Difference objects.  Returns an
    # Enumerator when no block is given.
    def each(left, right)
      return enum_for(:each, left, right) unless block_given?
      @live_tempfiles = []
      left_group = nil
      right_group = nil
      begin
        left_reader = GroupReader.new(left, :left, @key_of,
                                      @max_group_size, self)
        right_reader = GroupReader.new(right, :right, @key_of,
                                       @max_group_size, self)
        left_group = left_reader.next_group
        right_group = right_reader.next_group
        while left_group && right_group
          order = compare_keys(left_group.key, right_group.key)
          if order < 0
            emit_group(left_group, :left_only) { |d| yield d }
            left_group.close
            left_group = left_reader.next_group
          elsif order > 0
            emit_group(right_group, :right_only) { |d| yield d }
            right_group.close
            right_group = right_reader.next_group
          else
            pair_groups(left_group, right_group) { |d| yield d }
            left_group.close
            right_group.close
            left_group = left_reader.next_group
            right_group = right_reader.next_group
          end
        end
        while left_group
          emit_group(left_group, :left_only) { |d| yield d }
          left_group.close
          left_group = left_reader.next_group
        end
        while right_group
          emit_group(right_group, :right_only) { |d| yield d }
          right_group.close
          right_group = right_reader.next_group
        end
      ensure
        # Runs also when the consumer aborts iteration early (raise,
        # throw, break): release any spilled storage.
        left_group&.close
        right_group&.close
        until @live_tempfiles.empty?
          begin
            @live_tempfiles.pop.close!
          rescue StandardError
            # Already gone; nothing to clean up.
          end
        end
      end
    end

    # @private
    def content_of(row)
      @content_of.call(row)
    end

    # @private
    def register_tempfile(file)
      @live_tempfiles << file
      file
    end

    # @private
    def deregister_tempfile(file)
      @live_tempfiles.delete(file)
    end

    # @private
    def self.write_record(io, object)
      data = Marshal.dump(object)
      io.write([data.bytesize].pack("N"))
      io.write(data)
      io
    end

    # @private
    def self.read_record(io)
      header = io.read(4)
      return nil if header.nil?
      if header.bytesize < 4
        raise IOError, "truncated multiset diff spill file"
      end
      size = header.unpack1("N")
      data = io.read(size)
      if data.nil? || data.bytesize < size
        raise IOError, "truncated multiset diff spill file"
      end
      Marshal.load(data)
    end

    private

    def compare_keys(left_key, right_key)
      order = left_key <=> right_key
      if order.nil?
        raise ArgumentError,
              "keys are not comparable: #{left_key.inspect} vs " \
              "#{right_key.inspect}"
      end
      order
    end

    def emit_group(group, side)
      group.each do |row|
        yield Difference.new(side, group.key, row)
      end
    end

    def pair_groups(left_group, right_group, &block)
      if left_group.spilled? || right_group.spilled?
        pair_spilled(left_group, right_group, &block)
      else
        pair_in_memory(left_group.key, left_group, right_group, &block)
      end
    end

    def pair_in_memory(key, left_group, right_group)
      buckets = {}
      left_group.each do |row|
        (buckets[content_of(row)] ||= [[], []])[0] << row
      end
      right_group.each do |row|
        (buckets[content_of(row)] ||= [[], []])[1] << row
      end
      buckets.each_value do |left_rows, right_rows|
        common = [left_rows.size, right_rows.size].min
        left_rows.drop(common).each do |row|
          yield Difference.new(:left_only, key, row)
        end
        right_rows.drop(common).each do |row|
          yield Difference.new(:right_only, key, row)
        end
      end
    end

    def pair_spilled(left_group, right_group)
      key = left_group.key
      left_run = SortedRun.new(self, left_group, @max_group_size)
      right_run = SortedRun.new(self, right_group, @max_group_size)
      begin
        merge_sorted_runs(key, left_run, right_run) do |difference|
          yield difference
        end
      ensure
        left_run.close
        right_run.close
      end
    end

    # Both runs yield [content_dump, row] pairs ordered by content_dump.
    # Equal contents are consumed in lockstep (paired) with O(1) memory;
    # only the unpaired tail of a run is emitted.
    def merge_sorted_runs(key, left_run, right_run)
      left_enum = left_run.each
      right_enum = right_run.each
      left_record = next_record(left_enum)
      right_record = next_record(right_enum)
      while left_record && right_record
        order = left_record[0] <=> right_record[0]
        if order.zero?
          content = left_record[0]
          left_record = next_record(left_enum)
          right_record = next_record(right_enum)
          while left_record && right_record &&
                left_record[0] == content && right_record[0] == content
            left_record = next_record(left_enum)
            right_record = next_record(right_enum)
          end
          while left_record && left_record[0] == content
            yield Difference.new(:left_only, key, left_record[1])
            left_record = next_record(left_enum)
          end
          while right_record && right_record[0] == content
            yield Difference.new(:right_only, key, right_record[1])
            right_record = next_record(right_enum)
          end
        elsif order < 0
          yield Difference.new(:left_only, key, left_record[1])
          left_record = next_record(left_enum)
        else
          yield Difference.new(:right_only, key, right_record[1])
          right_record = next_record(right_enum)
        end
      end
      while left_record
        yield Difference.new(:left_only, key, left_record[1])
        left_record = next_record(left_enum)
      end
      while right_record
        yield Difference.new(:right_only, key, right_record[1])
        right_record = next_record(right_enum)
      end
    end

    def next_record(enum)
      enum.next
    rescue StopIteration
      nil
    end

    # Reads one key group at a time from an input, verifying that the
    # input is sorted by the selected key.
    class GroupReader
      NONE = Object.new
      private_constant :NONE

      def initialize(enum, side, key_of, max_group_size, diff)
        @enum = enum.each
        @side = side
        @key_of = key_of
        @max_group_size = max_group_size
        @diff = diff
        @position = 0
        @last_key = nil
        @have_last_key = false
        @peeked = false
        @buffer = NONE
      end

      # Returns the next Group, or nil at end of input.
      def next_group
        first = take
        return nil if first.equal?(NONE)
        key = @key_of.call(first)
        check_order(key)
        group = Group.new(key, @max_group_size, @diff)
        group.add(first)
        loop do
          row = peek
          break if row.equal?(NONE)
          break unless @key_of.call(row) == key
          group.add(take)
        end
        group
      end

      private

      def check_order(key)
        if @have_last_key
          order = key <=> @last_key
          if order.nil?
            raise ArgumentError,
                  "#{@side} input record #{@position}: key " \
                  "#{key.inspect} is not comparable with previous key " \
                  "#{@last_key.inspect}"
          end
          if order < 0
            raise UnorderedInputError.new(@side, @position, @last_key, key)
          end
        end
        @last_key = key
        @have_last_key = true
      end

      def take
        if @peeked
          @peeked = false
          @buffer
        else
          advance
        end
      end

      def peek
        unless @peeked
          @buffer = advance
          @peeked = true
        end
        @buffer
      end

      def advance
        @position += 1
        @enum.next
      rescue StopIteration
        NONE
      end
    end

    # All rows sharing one key.  Keeps at most +max_in_memory+ rows in
    # an Array; beyond that the rows are appended to a Tempfile and
    # streamed back on #each.
    class Group
      include Enumerable

      attr_reader :key, :size

      def initialize(key, max_in_memory, diff)
        @key = key
        @max_in_memory = max_in_memory
        @diff = diff
        @rows = []
        @file = nil
        @size = 0
        @closed = false
      end

      def add(row)
        raise "group is closed" if @closed
        @size += 1
        if @rows
          @rows << row
          spill if @rows.size > @max_in_memory
        else
          MultisetDiff.write_record(@file, row)
        end
        self
      end

      def spilled?
        !@file.nil?
      end

      def each
        return enum_for(:each) unless block_given?
        if @rows
          @rows.each { |row| yield row }
        else
          @file.flush
          @file.rewind
          while (row = MultisetDiff.read_record(@file))
            yield row
          end
        end
        self
      end

      def close
        return if @closed
        @closed = true
        @rows = nil
        if @file
          file = @file
          @file = nil
          @diff.deregister_tempfile(file)
          file.close!
        end
        self
      end

      private

      def spill
        @file = @diff.register_tempfile(
          Tempfile.new(["csv-multiset-diff-group", ".bin"])
        )
        @file.binmode
        @rows.each { |row| MultisetDiff.write_record(@file, row) }
        @rows = nil
      end
    end

    # A copy of one key group, sorted by dumped content, stored in one
    # or more Tempfile chunks.  Memory use is bounded by the chunk size;
    # chunks are merged with a k-way merge on read.
    class SortedRun
      include Enumerable

      def initialize(diff, group, chunk_size)
        @diff = diff
        @files = []
        chunk = []
        group.each do |row|
          chunk << [Marshal.dump(diff.content_of(row)), row]
          flush(chunk) if chunk.size >= chunk_size
        end
        flush(chunk)
      end

      # Yields [content_dump, row] pairs ordered by content_dump.
      def each
        return enum_for(:each) unless block_given?
        if @files.size == 1
          stream_file(@files[0]) { |record| yield record }
        else
          merge_files { |record| yield record }
        end
        self
      end

      def close
        until @files.empty?
          file = @files.pop
          @diff.deregister_tempfile(file)
          begin
            file.close!
          rescue StandardError
            # Already gone.
          end
        end
        self
      end

      private

      def flush(chunk)
        return if chunk.empty?
        chunk.sort_by!(&:first)
        file = @diff.register_tempfile(
          Tempfile.new(["csv-multiset-diff-run", ".bin"])
        )
        file.binmode
        chunk.each { |record| MultisetDiff.write_record(file, record) }
        file.flush
        @files << file
        chunk.clear
      end

      def stream_file(file)
        file.rewind
        while (record = MultisetDiff.read_record(file))
          yield record
        end
      end

      def merge_files
        files = @files
        files.each(&:rewind)
        current = files.map { |file| MultisetDiff.read_record(file) }
        loop do
          best_index = nil
          current.each_with_index do |record, index|
            next if record.nil?
            if best_index.nil? || record[0] < current[best_index][0]
              best_index = index
            end
          end
          break if best_index.nil?
          yield current[best_index]
          current[best_index] = MultisetDiff.read_record(files[best_index])
        end
      end
    end
  end
end
