# frozen_string_literal: true

require "stringio"

class CSV
  #
  # = CSV Partition Planning
  #
  # CSV.partitions scans a seekable UTF-8 CSV file once and returns a
  # CSV::PartitionPlan describing safe byte ranges that can be parsed
  # independently, for example by different worker threads.
  #
  # A range never starts inside:
  # - a quoted field (an embedded row separator stays with its record),
  # - the middle of the row separator (CRLF is never split), or
  # - a multi-byte character.
  #
  # The planner retains only the information needed to describe the
  # partitions (byte ranges, the starting record index and the header
  # line); it does not store each row or field.

  # Note: Don't use this class directly. This is an internal class.
  #
  # Streams a seekable byte source once while applying the same quoting
  # and separator rules as CSV::Parser, reporting each record terminator
  # as a byte offset through a constant-size sliding window.
  class PartitionScanner
    CHUNK_SIZE = 1 << 16

    def initialize(io,
                   column_separator:,
                   row_separator:,
                   quote_character:,
                   liberal_parsing:,
                   backslash_quote:,
                   strip:,
                   start_offset: 0)
      @io = io
      @start_offset = start_offset
      @column_separator = column_separator.to_s.b
      @row_separator = row_separator.to_s.b
      @quote_character =
        quote_character.nil? ? nil : quote_character.to_s.b
      @liberal_parsing = liberal_parsing
      @backslash_quote = backslash_quote
      @strip = strip

      @state = :field_start
      @record_index = 0
      @line_number = 1
      @base = 0
      @pending = "".b
      @record_started = false
      @ends_with_cr = false
    end

    attr_reader :record_index, :line_number

    # Yields [record_index, end_byte_offset] for every record.  A final
    # record without a trailing row separator is reported at EOF.
    def each_record_end(&block)
      if block
        scan_all(block)
      else
        Enumerator.new do |yielder|
          scan_all(->(index, offset) { yielder << [index, offset] })
        end
      end
    end

    private

    def scan_all(emit)
      @io.seek(@start_offset)
      @base = @start_offset
      while (chunk = @io.read(CHUNK_SIZE))
        chunk = chunk.b
        next if chunk.empty?
        @pending << chunk
        # With very small reads one read may not even contain a full
        # separator; accumulate until a processable window exists.
        next if @pending.bytesize <= trailing_carry
        safe = @pending.bytesize - trailing_carry
        # A quote's escaped status depends on the parity of the whole
        # run of backslashes before it.  Keep any trailing run of
        # backslashes unprocessed so that run can never be split across
        # a chunk boundary.
        if @backslash_quote
          while safe > 0 && @pending.getbyte(safe - 1) == 0x5c
            safe -= 1
          end
        end
        scan(safe, emit)
      end

      # At EOF no pattern can be missing its tail.
      scan(@pending.bytesize, emit)

      if @state == :quoted
        raise MalformedCSVError.new("Unclosed quoted field",
                                    @line_number)
      end
      if @state == :quote_seen && quote_seen_invalid_at_eof?
        raise MalformedCSVError.new(
                "Any value after quoted field isn't allowed",
                @line_number)
      end

      if @record_started || @pending.empty? == false
        @record_index += 1
        emit.call(@record_index, @base + @pending.bytesize)
      end
    end

    # At EOF, bytes following a closing quote (other than permitted
    # strip whitespace) make the record invalid in strict parsing.
    def quote_seen_invalid_at_eof?
      return false if @liberal_parsing
      return @pending.empty? == false unless @strip
      i = 0
      while i < @pending.bytesize
        return true unless strip_byte?(@pending.getbyte(i))
        i += 1
      end
      false
    end

    def scan(safe, emit)
      i = 0
      while i < safe
        next_i = advance(i, safe, emit)
        break if next_i.nil?
        i = next_i
      end
      consumed = @pending.byteslice(0, i)
      @base += consumed.bytesize
      # Remember a lone trailing CR only when its LF is outside the
      # currently processed (safe) window, i.e. it is carried forward.
      @ends_with_cr = !consumed.empty? &&
                      consumed.getbyte(-1) == 0x0d &&
                      i == safe &&
                      safe < @pending.bytesize &&
                      @pending.getbyte(i) == 0x0a
      @pending = @pending.byteslice(i..-1) || "".b
    end

    # States:
    # - +:field_start+       next field begins; a quote opens a real field
    # - +:unquoted+          inside a field that did not start with a quote
    # - +:unquoted_literal+  liberal tail after a quote appeared mid-field
    # - +:quoted+            inside a quoted field
    # - +:quote_seen+        right after a closing quote
    def advance(position, safe, emit)
      case @state
      when :field_start
        if @ends_with_cr && position == 0 &&
           @pending.getbyte(0) == 0x0a &&
           @row_separator == "\r\n".b
          @ends_with_cr = false
          return 1
        end
        position = skip_strip(position, safe)
        return nil if position.nil?
        @record_started = true
        if matches_quote?(position)
          @state = :quoted
          position + @quote_character.bytesize
        else
          @state = :unquoted
          position
        end
      when :unquoted
        advance_unquoted(position, safe, emit, false)
      when :unquoted_literal
        advance_unquoted(position, safe, emit, true)
      when :quoted
        advance_quoted(position, safe)
      when :quote_seen
        advance_quote_seen(position, safe, emit)
      end
    end

    def advance_unquoted(position, safe, emit, literal)
      row_at = @pending.index(@row_separator, position)
      column_at = @pending.index(@column_separator, position)
      quote_at = @quote_character &&
                 @pending.index(@quote_character, position)

      row_ok = complete?(row_at, @row_separator, safe)
      column_ok = complete?(column_at, @column_separator, safe)
      quote_ok = complete?(quote_at, @quote_character, safe)

      # A bare line-ending byte inside an unquoted field is rejected
      # when it is not itself part of the configured row separator
      # (e.g. an isolated CR or LF when the row separator is CRLF).
      bad_line = !@quote_character.nil? &&
                 nearest_bad_line_ending(position, safe)
      if bad_line && (!row_ok || bad_line < row_at) &&
         (!column_ok || bad_line < column_at)
        raise MalformedCSVError.new(
                "Unquoted fields do not allow new line " \
                "<#{@pending.byteslice(bad_line, 2).inspect}>",
                @line_number)
      end

      # In strict mode a quote anywhere inside an unquoted field is
      # illegal; it must be rejected before a separator is consumed.
      if !literal && !@liberal_parsing && !@quote_character.nil? &&
         quote_ok && (!row_ok || quote_at < row_at) &&
         (!column_ok || quote_at < column_at)
        raise MalformedCSVError.new("Illegal quoting", @line_number)
      end

      if row_ok && (!column_ok || row_at < column_at)
        finish_record(row_at, emit)
        return row_at + @row_separator.bytesize
      end
      if column_ok
        @state = :field_start
        @record_started = true
        return column_at + @column_separator.bytesize
      end

      if !literal && @liberal_parsing &&
         complete?(quote_at, @quote_character, safe)
        # Once a quote appears inside an unquoted field the parser keeps
        # the whole field literal up to the next real separator.
        @state = :unquoted_literal
        return safe
      end
      safe
    end

    def advance_quoted(position, safe)
      return safe if @quote_character.nil?

      slash_pattern = "\\".b
      quote_at = @pending.index(@quote_character, position)
      slash_at = @backslash_quote &&
                 @pending.index(slash_pattern, position)

      if @backslash_quote && slash_at &&
         slash_at < (quote_at || safe)
        next_position = slash_at + 1
        if next_position >= safe
          # The escaped byte is in the carry; wait for more data.
          return safe
        end
        if @pending.byteslice(next_position,
                              @quote_character.bytesize) ==
             @quote_character
          # \\" : the quote is consumed literally and never closes.
          next_position + @quote_character.bytesize
        elsif @pending.getbyte(next_position) == 0x5c
          # \\\\ : the following backslash stays available so it can
          # itself escape whatever comes after it (matching the
          # parser, which pairs backslashes overlappingly).
          next_position
        else
          # \\ + other byte: both are consumed literally.
          next_position + 1
        end
      elsif complete?(quote_at, @quote_character, safe)
        # Each quote toggles between inside and outside the field.
        @state = :quote_seen
        quote_at + @quote_character.bytesize
      else
        safe
      end
    end

    def advance_quote_seen(position, safe, emit)
      column_at = @pending.index(@column_separator, position)
      row_at = @pending.index(@row_separator, position)
      quote_at = @quote_character &&
                 @pending.index(@quote_character, position)

      if column_at == position &&
         complete?(column_at, @column_separator, safe)
        @state = :field_start
        @record_started = true
        return column_at + @column_separator.bytesize
      end
      if row_at == position && complete?(row_at, @row_separator, safe)
        finish_record(row_at, emit)
        return row_at + @row_separator.bytesize
      end
      # The LF of a CRLF split across the chunk boundary.
      if @ends_with_cr && position == 0 &&
         @pending.getbyte(0) == 0x0a &&
         @row_separator == "\r\n".b
        @ends_with_cr = false
        @record_index += 1
        @line_number += 1
        @state = :field_start
        @record_started = false
        emit.call(@record_index, @base + 1)
        return 1
      end
      if quote_at == position &&
         complete?(quote_at, @quote_character, safe)
        @state = :quoted
        return quote_at + @quote_character.bytesize
      end

      # A byte at the safe boundary that may begin a multi-byte row
      # separator (its tail is held in the carry) must be deferred.
      if row_at == position && !complete?(row_at, @row_separator, safe)
        return safe
      end

      # In strict mode content after a closing quote is only legal when
      # it is strip whitespace leading directly to a column/row token.
      unless @liberal_parsing
        probe = position
        if @strip
          probe += 1 while probe < safe &&
                           strip_byte?(@pending.getbyte(probe))
          follows_token = (column_at == probe &&
                           complete?(column_at, @column_separator, safe)) ||
                          (row_at == probe &&
                           complete?(row_at, @row_separator, safe))
          at_eof = @pending.bytesize == safe
          if probe < safe && !follows_token &&
             !(at_eof && probe == safe)
            raise MalformedCSVError.new(
                    "Any value after quoted field isn't allowed",
                    @line_number)
          end
        elsif position < safe
          raise MalformedCSVError.new(
                  "Any value after quoted field isn't allowed",
                  @line_number)
        end
      end

      if @liberal_parsing
        # Liberal mode: once a closing quote is followed by content,
        # the remainder of the field is an unquoted value in which
        # quotes are literal.  Move to the literal state; its separator
        # logic handles everything that follows.
        @state = :unquoted_literal
        return position
      end

      # A stray line-ending byte (not the configured row separator)
      # directly after a closing quote is illegal in strict mode.
      bad_line = !@quote_character.nil? &&
                 nearest_bad_line_ending(position, safe)
      if bad_line
        raise MalformedCSVError.new(
                "Illegal end-of-line sequence outside of a " \
                "quoted field " \
                "<#{@pending.byteslice(bad_line, 2).inspect}>",
                @line_number)
      end

      # Strict parsing.  Inspect the bytes until the nearest event that
      # is already fully present; they must all be strip whitespace.
      column_complete = complete?(column_at, @column_separator, safe)
      row_complete = complete?(row_at, @row_separator, safe)
      quote_complete = complete?(quote_at, @quote_character, safe)
      nearest = [
        column_complete ? column_at : nil,
        row_complete ? row_at : nil,
        quote_complete ? quote_at : nil,
      ].compact.min

      if nearest.nil?
        # No structural token is fully present yet.  If this is the end
        # of the input, trailing content must be strip whitespace only.
        if @pending.bytesize == safe
          trailing_ok = @strip ? strip_run?(position, safe)
                               : position >= safe
          unless trailing_ok
            raise MalformedCSVError.new(
                    "Any value after quoted field isn't allowed",
                    @line_number)
          end
        end
        return safe
      end

      # Without strip, any intervening content is illegal.
      unless @strip && strip_run?(position, nearest)
        raise MalformedCSVError.new(
                "Any value after quoted field isn't allowed",
                @line_number)
      end

      if column_complete && (!row_at || column_at <= row_at)
        @state = :field_start
        return column_at + @column_separator.bytesize
      end
      if row_complete
        finish_record(row_at, emit)
        return row_at + @row_separator.bytesize
      end
      # A quote reached only after strip whitespace is illegal; a quote
      # is structural only when it is immediately adjacent.
      raise MalformedCSVError.new(
              "Any value after quoted field isn't allowed",
              @line_number)
    end

    def matches_quote?(position)
      !@quote_character.nil? &&
        @pending.byteslice(position,
                           @quote_character.bytesize) ==
          @quote_character
    end

    def complete?(position, pattern, safe)
      position && !pattern.nil? && position + pattern.bytesize <= safe
    end

    # Position of an isolated CR/LF that is not the configured row
    # separator.  Only relevant when the row separator is CRLF.
    def nearest_bad_line_ending(position, safe)
      return nil unless @row_separator == "\r\n".b
      search = position
      while search < safe
        cr = @pending.index("\r".b, search)
        lf = @pending.index("\n".b, search)
        spot = [cr, lf].compact.min
        return nil if spot.nil? || spot >= safe
        # An LF carried over as the second half of a CRLF whose CR was
        # consumed in the previous chunk is part of the row separator.
        if spot == 0 && @ends_with_cr &&
           @pending.getbyte(0) == 0x0a
          search = 1
          next
        end
        if @pending.byteslice(spot, 2) == "\r\n".b
          search = spot + 2
        elsif @pending.getbyte(spot) == 0x0d &&
              spot == safe - 1 && safe < @pending.bytesize
          # A CR at the safe boundary may be the start of a CRLF whose
          # LF is held in the carry; defer to the next round.
          return nil
        else
          return spot
        end
      end
      nil
    end

    def skip_strip(position, safe)
      return position unless @strip
      position += 1 while position < safe &&
                          strip_byte?(@pending.getbyte(position))
      position >= safe ? nil : position
    end

    def strip_run?(from, to)
      return false unless @strip
      from.upto(to - 1) do |index|
        return false unless strip_byte?(@pending.getbyte(index))
      end
      true
    end

    def strip_byte?(byte)
      if @strip == true
        byte == 0x20 || byte == 0x09 || byte == 0x0c || byte == 0x0b
      else
        @strip.getbyte(0) == byte
      end
    end

    def finish_record(position, emit)
      @record_index += 1
      @line_number += 1
      @state = :field_start
      @record_started = false
      emit.call(@record_index,
                @base + position + @row_separator.bytesize)
    end

    def trailing_carry
      maximum = [
        @row_separator.bytesize,
        @column_separator.bytesize,
        @quote_character&.bytesize || 0,
        @backslash_quote ? 2 : 0,
      ].max
      [maximum - 1, @pending.bytesize].min
    end
  end

  # Builds a PartitionPlan from record terminator offsets by choosing
  # boundaries close to evenly sized byte targets.
  #
  # Only the offsets around the requested cut points are retained, so
  # memory use is proportional to the number of partitions, not rows.
  class Partitioner
    def initialize(io, size, scanner, target_partitions:,
                   target_partition_size: nil, data_start: 0)
      @size = size
      @scanner = scanner
      @target_partitions = target_partitions
      @target_partition_size = target_partition_size
      @data_start = data_start
    end

    # Returns [Array of [start_byte, end_byte, start_record], record_count]
    def plan
      targets = cut_targets

      chosen = []
      target_index = 0
      previous = 0
      previous_record = 0
      record_count = 0
      @scanner.each_record_end do |record, offset|
        record_count += 1
        while target_index < targets.size &&
              offset >= targets[target_index]
          target = targets[target_index]
          boundary =
            if previous.positive? &&
               (target - previous).abs <= (offset - target).abs
              [previous, previous_record]
            else
              [offset, record]
            end
          chosen << boundary
          target_index += 1
        end
        previous = offset
        previous_record = record
      end
      # Targets never reached (record ends all before the target) anchor
      # at the end of the file.
      while chosen.size < targets.size
        chosen << [@size, record_count]
      end

      cuts = [[@data_start, 0]]
      chosen.each do |boundary, record|
        candidate =
          if boundary > cuts.last.first && boundary < @size
            [boundary, record]
          else
            # Several cut points fall on the same (or no) real
            # boundary: collapse them into one zero-width partition at
            # the previous cut position.
            cuts.last
          end
        cuts << candidate
      end
      cuts << [@size, record_count]
      ranges = cuts.each_cons(2).with_index.map do |(start, finish), i|
        [start.first, finish.first, start.last]
      end
      [ranges, record_count]
    end

    private

    def cut_targets
      span = @size - @data_start
      if @target_partition_size
        return [] if @target_partition_size.to_i <= 0 || span <= 0
        (1..(span / @target_partition_size)).map do |i|
          @data_start + i * @target_partition_size
        end
      else
        count = [@target_partitions, 1].max
        (1...count).collect do |i|
          @data_start + (span * i) / count
        end
      end
    end
  end

  # A safe, independently parseable byte range of a CSV file.
  #
  # The range is half-open: bytes +start_byte+ (inclusive) through
  # +end_byte+ (exclusive).  +start_record+ is the zero-based index of
  # the first record contained in the partition (headers excluded when
  # present).
  class Partition
    include Enumerable

    def initialize(plan, index, start_byte, end_byte, start_record)
      @plan = plan
      @index = index
      @start_byte = start_byte
      @end_byte = end_byte
      @start_record = start_record
    end

    attr_reader :index, :start_byte, :end_byte, :start_record

    alias start_offset start_byte
    alias end_offset end_byte

    def byte_size
      @end_byte - @start_byte
    end

    alias size byte_size

    def empty?
      @start_byte == @end_byte
    end

    # Builds a CSV reader for this partition over a fresh IO handle.
    #
    # Pass +io:+ to reuse an already opened IO; otherwise a new file
    # handle is opened when the plan was created from a path.
    def open(io: nil, mode: "rb", **options)
      csv = @plan.build_csv(self, io, mode, options)
      if block_given?
        begin
          yield csv
        ensure
          csv.close
        end
      else
        csv
      end
    end

    # Iterates rows of this partition.
    def each(io: nil, mode: "rb", **options)
      return to_enum(__method__, io: io, mode: mode, **options) unless
        block_given?
      wants_headers = truthy_header_option?(options[:headers])
      open(io: io, mode: mode, **options) do |csv|
        if wants_headers
          header_names =
            if @plan.headers?
              @plan.headers
            elsif options[:headers].is_a?(Array)
              options[:headers]
            end
          if header_names
            csv.each do |row|
              yield CSV::Row.new(header_names, row, false)
            end
          else
            csv.each { |row| yield row }
          end
        else
          csv.each { |row| yield row }
        end
      end
    end

    def inspect
      "#<#{self.class} index=#{@index} " \
        "bytes=#{@start_byte}...#{@end_byte} " \
        "start_record=#{@start_record}>"
    end

    private

    def truthy_header_option?(value)
      value == true || value.is_a?(String) || value.is_a?(Array)
    end
  end

  # The result of CSV.partitions: an ordered list of Partition objects
  # plus the header line detected during planning.
  class PartitionPlan
    include Enumerable

    def initialize(path_or_io, partitions, header_line, headers_present,
                   parse_options, row_separator, size, encoding)
      @path_or_io = path_or_io
      @partitions = partitions
      @header_line = header_line
      @headers_present = headers_present
      @parse_options = parse_options
      @row_separator = row_separator
      @size = size
      @encoding = encoding
    end

    attr_reader :path_or_io, :header_line, :row_separator, :size,
                :encoding

    def each(&block)
      return to_enum(__method__) unless block
      @partitions.each(&block)
    end

    def [](index)
      @partitions[index]
    end

    def length
      @partitions.length
    end

    alias count length

    def first
      @partitions.first
    end

    def last
      @partitions.last
    end

    # True when a header line was detected and excluded from records.
    def headers?
      @headers_present
    end

    # The header fields parsed with the caller's options, or nil when
    # the file has no header.
    def headers
      return nil unless @headers_present
      CSV.parse_line(@header_line, **csv_options)
    end

    # Options used to read each partition (with the resolved row
    # separator and header settings normalized).
    def csv_options
      @csv_options ||= begin
        options = @parse_options.dup
        options[:row_sep] = @row_separator
        options[:headers] = false
        options[:return_headers] = false
        options
      end
    end

    # Internal: builds a CSV reader over one partition.
    def build_csv(partition, supplied_io, mode, overrides)
      source, owned = open_source(supplied_io, mode)
      prefix = partition_prefix(partition)
      bounded = BoundedIO.new(source, owned,
                              partition.start_byte,
                              partition.end_byte,
                              prefix,
                              @encoding)
      options = reader_options(overrides)
      bounded.consume_prefix unless prefix.nil?
      CSV.new(bounded, **options)
    end

    # Reader options.  Partition streams are always parsed as raw rows;
    # header semantics (if requested) are applied by Partition#each so
    # every partition yields consistent, header-aware results.
    def reader_options(overrides)
      options = csv_options.merge(overrides)
      options[:headers] = false
      options[:return_headers] = false
      options
    end

    def inspect
      "#<#{self.class} partitions=#{@partitions.length} " \
        "size=#{@size} header=#{@headers_present}>"
    end

    private

    def partition_prefix(partition)
      return nil unless @headers_present
      return nil if partition.index.zero?
      @header_line
    end

    def open_source(supplied, mode)
      if supplied
        [supplied, false]
      elsif @path_or_io.is_a?(String)
        [File.open(@path_or_io, mode), true]
      elsif @path_or_io.respond_to?(:path) && @path_or_io.path
        [File.open(@path_or_io.path, mode), true]
      elsif @path_or_io.respond_to?(:dup)
        [@path_or_io.dup, false]
      else
        [@path_or_io, false]
      end
    end
  end

  # Note: Don't use this class directly. This is an internal class.
  #
  # Presents a single byte range of a larger IO as a standalone stream,
  # optionally prefixed with a header line.  Every chunk handed to the
  # parser is a valid UTF-8 string, so a partition never starts in the
  # middle of a multi-byte character.
  class BoundedIO
    READ_SIZE = 8192

    def initialize(source, owned, start_byte, end_byte, prefix, encoding)
      @source = source
      @owned = owned
      @start_byte = start_byte
      @end_byte = end_byte
      @position = start_byte
      @encoding = Encoding.find(encoding || "UTF-8")
      @prefix = (prefix || +"").b
      @carry = +"".b
      @source.seek(start_byte)
    end

    attr_reader :encoding

    # The parser reads through IO#gets(separator, limit).
    def gets(separator = $/, limit = nil)
      return nil if eof?
      out = +"".b
      separator = separator.to_s.b unless separator.nil?
      loop do
        chunk = read_chunk(limit ? limit - out.bytesize : READ_SIZE)
        break if chunk.empty?
        out << chunk
        if separator.nil?
          break if limit.nil? || out.bytesize >= limit
        elsif out.include?(separator) ||
              (limit && out.bytesize >= limit)
          break
        end
        break if source_eof?
      end
      return nil if out.empty?
      out.force_encoding(@encoding)
    end

    def read(length)
      return nil if eof?
      chunk = read_chunk(length)
      chunk.empty? ? nil : chunk.force_encoding(@encoding)
    end

    def eof?
      @prefix.empty? && @carry.empty? && @position >= @end_byte
    end

    # Discards the synthetic header prefix entirely.
    def consume_prefix
      @prefix = +"".b
    end

    def external_encoding
      @encoding
    end

    def internal_encoding
      nil
    end

    def close
      source = @source
      @source = nil
      source.close if @owned && source && !source.closed?
    end

    def to_io
      self
    end

    private

    def source_eof?
      @position >= @end_byte
    end

    # Reads up to +want+ bytes from prefix/carry/source while ensuring
    # the returned chunk never ends in the middle of a UTF-8 character.
    def read_chunk(want)
      out = +"".b
      unless @prefix.empty?
        length = [want, @prefix.bytesize].min
        out << @prefix.slice!(0, length)
      end
      while out.bytesize < want &&
            (!@carry.empty? || @position < @end_byte)
        unless @carry.empty?
          out << @carry
          @carry = +"".b
          next
        end
        remaining = @end_byte - @position
        length = [want - out.bytesize, remaining].min
        piece = (@source.read(length) || +"").b
        @position += piece.bytesize
        out << piece
      end
      split_utf8(out)
      out
    end

    # Holds back a trailing incomplete UTF-8 sequence for the next read
    # unless this is the true end of the partition.
    def split_utf8(chunk)
      return if chunk.empty?
      cut = chunk.bytesize
      cut -= 1 while cut > 0 &&
                    !chunk.byteslice(0, cut).dup.force_encoding(
                      Encoding::UTF_8).valid_encoding?
      return if cut == chunk.bytesize
      if @position >= @end_byte && @carry.empty?
        # Invalid trailing bytes: let the parser raise as it would for
        # a serial read of the same (invalid) file.
        return
      end
      held = chunk.byteslice(cut..-1)
      chunk.slice!(cut..-1)
      @carry = held + @carry
    end
  end
end
