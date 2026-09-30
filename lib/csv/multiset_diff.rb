# frozen_string_literal: true

require "tempfile"

class CSV
  # The error raised when CSV::MultisetDiff detects that one of its inputs
  # is not sorted by the selected business key.
  #
  # The offending record can be located via #side and #position.
  class UnsortedInputError < RuntimeError
    # Which input is broken: +:left+ or +:right+.
    attr_reader :side
    # The 1-based record number of the out-of-order record in its input.
    attr_reader :position
    # The business key of the out-of-order record.
    attr_reader :key
    # The business key of the previous record.
    attr_reader :previous_key

    def initialize(side, position, key, previous_key)
      @side = side
      @position = position
      @key = key
      @previous_key = previous_key
      super("#{side} input is not sorted by business key: " \
            "record #{position} has key #{key.inspect}, " \
            "which must not sort before the previous key " \
            "#{previous_key.inspect}")
    end
  end

  # CSV::MultisetDiff reconciles two CSV-like record streams that are both
  # sorted by a caller-selected business key.  The same key may appear many
  # times in each input.
  #
  # Records are consumed lazily and compared as a streaming merge, so the
  # inputs are never converted to a Hash as a whole.  Within a group of
  # records that share the same business key, records whose content (as
  # selected by the +:content+ proc) is equal are paired off one-for-one by
  # count, like a multiset intersection.  Whatever remains is reported as
  # +:left_only+ or +:right_only+; two records with different content are
  # never reported as a single "modification".
  #
  # Memory usage is bounded by +:max_group_size+: when a single key group
  # grows beyond that many records, the group is spilled to standard
  # library Tempfile storage and reconciled with an external sort instead
  # of being held in memory.
  #
  # Example:
  #
  #   diff = CSV::MultisetDiff.new(left_csv, right_csv,
  #                                key: ->(row) { row["id"] },
  #                                content: ->(row) { row.fields })
  #   diff.each do |event, key, record|
  #     case event
  #     when :left_only  then puts "missing on right: #{key}"
  #     when :right_only then puts "missing on left: #{key}"
  #     end
  #   end
  class MultisetDiff
    include Enumerable

    # Default limit of records of a single key group held in memory.
    DEFAULT_MAX_GROUP_SIZE = 1024

    # Creates a streaming multiset diff over +left+ and +right+.
    #
    # Both +left+ and +right+ must be enumerables of records sorted by the
    # business key selected by the +key:+ proc.  An UnsortedInputError is
    # raised, reporting the record position, if either input turns out not
    # to be sorted.
    #
    # The +content:+ proc selects how row content is compared; it defaults
    # to the record itself.  It must return values comparable with <=>
    # (Array and String content, for example, both work).
    #
    # +max_group_size+ limits how many records of a single key group are
    # kept in memory before the group is spilled to temporary files.
    def initialize(left, right, key:, content: nil,
                   max_group_size: DEFAULT_MAX_GROUP_SIZE)
      @left = left
      @right = right
      @key_selector = key
      @content_selector = content || ->(record) { record }
      unless max_group_size.is_a?(Integer) && max_group_size > 0
        raise ArgumentError,
              "max_group_size must be a positive Integer: " \
              "#{max_group_size.inspect}"
      end
      @max_group_size = max_group_size
    end

    # Yields <tt>[event, key, record]</tt> for every record that could not
    # be paired with an equal-content record of the same business key on
    # the other side.  +event+ is +:left_only+ or +:right_only+.
    #
    # If the consumer stops iterating early (for example with +break+),
    # any temporary files used for spilled key groups are removed.
    def each
      return to_enum(__method__) unless block_given?

      left = CheckedRecords.new(@left, :left, @key_selector).each
      right = CheckedRecords.new(@right, :right, @key_selector).each
      left_current = next_entry(left)
      right_current = next_entry(right)
      while left_current && right_current
        comparison = compare_values(left_current[0], right_current[0], :left)
        if comparison < 0
          yield :left_only, left_current[0], left_current[1]
          left_current = next_entry(left)
        elsif comparison > 0
          yield :right_only, right_current[0], right_current[1]
          right_current = next_entry(right)
        else
          left_current, right_current =
            reconcile_group(left, left_current, right, right_current) do |*event|
              yield(*event)
            end
        end
      end
      while left_current
        yield :left_only, left_current[0], left_current[1]
        left_current = next_entry(left)
      end
      while right_current
        yield :right_only, right_current[0], right_current[1]
        right_current = next_entry(right)
      end
      self
    end

    private

    def next_entry(enumerator)
      enumerator.next
    rescue StopIteration
      nil
    end

    def compare_values(left_value, right_value, side)
      comparison = left_value <=> right_value
      if comparison.nil?
        raise ArgumentError,
              "#{side} input produced values that are not comparable " \
              "with <=>: #{left_value.inspect} and #{right_value.inspect}"
      end
      comparison
    end

    # Consumes the rest of the key group shared by +left_current+ and
    # +right_current+, yielding unmatched records, and returns the next
    # currents of both streams.
    def reconcile_group(left, left_current, right, right_current)
      key = left_current[0]
      matcher = GroupMatcher.new(@content_selector, @max_group_size)
      begin
        while left_current &&
              compare_values(left_current[0], key, :left) == 0
          matcher.add_left(left_current[1])
          left_current = next_entry(left)
        end
        while right_current &&
              compare_values(right_current[0], key, :left) == 0
          unless matcher.absorb_right(right_current[1])
            yield :right_only, key, right_current[1]
          end
          right_current = next_entry(right)
        end
        matcher.flush do |event, record|
          yield event, key, record
        end
      ensure
        matcher.close
      end
      [left_current, right_current]
    end

    # Wraps an input enumerable, yielding <tt>[key, record]</tt> pairs and
    # verifying that keys arrive in non-decreasing order.
    class CheckedRecords
      include Enumerable

      def initialize(records, side, key_selector)
        @records = records
        @side = side
        @key_selector = key_selector
      end

      def each
        return to_enum(__method__) unless block_given?

        previous_key = nil
        position = 0
        @records.each do |record|
          position += 1
          key = @key_selector.call(record)
          if position > 1
            comparison = key <=> previous_key
            if comparison.nil?
              raise ArgumentError,
                    "#{@side} input record #{position} has a business key " \
                    "that is not comparable with <=>: #{key.inspect} and " \
                    "#{previous_key.inspect}"
            end
            if comparison < 0
              raise UnsortedInputError.new(@side, position, key, previous_key)
            end
          end
          previous_key = key
          yield key, record
        end
      end
    end
    private_constant :CheckedRecords

    # Reconciles the records of a single key group.  Records are counted
    # in memory until the group grows beyond the configured limit; then
    # both sides are spilled to temporary files and reconciled with an
    # external sort.
    class GroupMatcher
      def initialize(content_selector, max_group_size)
        @content_selector = content_selector
        @max_group_size = max_group_size
        # content => [unmatched count, representative record]
        @counts = {}
        @size = 0
        @spilled = false
        @left_bag = nil
        @right_bag = nil
      end

      def add_left(record)
        if @spilled
          @left_bag.add(record)
        else
          content = @content_selector.call(record)
          entry = @counts[content]
          if entry
            entry[0] += 1
          else
            @counts[content] = [1, record]
          end
          @size += 1
          spill if @size > @max_group_size
        end
      end

      # Absorbs a record from the right side.  Returns false when the
      # record is known to be unmatched (in-memory mode only); when
      # spilled, matching is deferred to #flush and true is returned.
      def absorb_right(record)
        if @spilled
          @right_bag.add(record)
          true
        else
          content = @content_selector.call(record)
          entry = @counts[content]
          if entry
            entry[0] -= 1
            @counts.delete(content) if entry[0] == 0
            true
          else
            false
          end
        end
      end

      # Yields <tt>[event, record]</tt> for every record left unmatched.
      def flush
        if @spilled
          flush_spilled { |event, record| yield event, record }
        else
          @counts.each_value do |count, record|
            count.times { yield :left_only, record }
          end
        end
      end

      def close
        @left_bag&.close
        @right_bag&.close
      end

      private

      def spill
        @left_bag = ExternalSortedBag.new(@content_selector, @max_group_size)
        @right_bag = ExternalSortedBag.new(@content_selector, @max_group_size)
        @counts.each_value do |count, record|
          count.times { @left_bag.add(record) }
        end
        @counts.clear
        @spilled = true
      end

      def flush_spilled
        left = @left_bag.each
        right = @right_bag.each
        left_record = bag_next(left)
        right_record = bag_next(right)
        while left_record && right_record
          comparison = compare_content(left_record, right_record)
          if comparison < 0
            yield :left_only, left_record
            left_record = bag_next(left)
          elsif comparison > 0
            yield :right_only, right_record
            right_record = bag_next(right)
          else
            left_record = bag_next(left)
            right_record = bag_next(right)
          end
        end
        while left_record
          yield :left_only, left_record
          left_record = bag_next(left)
        end
        while right_record
          yield :right_only, right_record
          right_record = bag_next(right)
        end
      end

      def bag_next(enumerator)
        enumerator.next
      rescue StopIteration
        nil
      end

      def compare_content(left_record, right_record)
        comparison = @content_selector.call(left_record) <=>
                     @content_selector.call(right_record)
        if comparison.nil?
          raise ArgumentError,
                "record contents are not comparable with <=>: " \
                "#{left_record.inspect} and #{right_record.inspect}"
        end
        comparison
      end
    end
    private_constant :GroupMatcher

    # A bag of records that is sorted externally: records are buffered up
    # to the run size, sorted runs are written to Tempfile storage, and
    # #each merges the runs back in content order.  Only one run is ever
    # held in memory at a time.
    class ExternalSortedBag
      END_OF_RUN = Object.new

      def initialize(content_selector, run_size)
        @content_selector = content_selector
        @run_size = run_size
        @runs = []
        @buffer = []
      end

      def add(record)
        @buffer << record
        flush_run if @buffer.size >= @run_size
      end

      def each
        return to_enum(__method__) unless block_given?

        flush_run
        heads = []
        @runs.each do |run|
          run.rewind
          record = read_record(run)
          heads << [run, record] unless record.equal?(END_OF_RUN)
        end
        until heads.empty?
          minimum = 0
          1.upto(heads.size - 1) do |index|
            comparison = content_of(heads[index][1]) <=>
                         content_of(heads[minimum][1])
            if comparison.nil?
              raise ArgumentError,
                    "record contents are not comparable with <=>: " \
                    "#{heads[index][1].inspect} and " \
                    "#{heads[minimum][1].inspect}"
            end
            minimum = index if comparison < 0
          end
          run, record = heads[minimum]
          yield record
          following = read_record(run)
          if following.equal?(END_OF_RUN)
            heads.delete_at(minimum)
          else
            heads[minimum] = [run, following]
          end
        end
      end

      def close
        @runs.each do |run|
          run.close!
        rescue Errno::ENOENT
          # Already gone; nothing to clean up.
        end
        @runs.clear
        @buffer.clear
      end

      private

      def content_of(record)
        @content_selector.call(record)
      end

      def flush_run
        return if @buffer.empty?

        sorted = @buffer.sort_by { |record| content_of(record) }
        run = Tempfile.new(["csv-multiset-diff", ".run"])
        run.binmode
        sorted.each do |record|
          payload = Marshal.dump(record)
          run.write([payload.bytesize].pack("N"))
          run.write(payload)
        end
        run.flush
        @runs << run
        @buffer.clear
      end

      def read_record(io)
        header = io.read(4)
        return END_OF_RUN if header.nil? || header.bytesize < 4

        Marshal.load(io.read(header.unpack1("N")))
      end
    end
    private_constant :ExternalSortedBag
  end
end
