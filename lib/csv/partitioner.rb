# frozen_string_literal: true

require "strscan"

class CSV
  # :call-seq:
  #   CSV::Partitioner.new(source, **options) -> partitioner
  #
  # Returns a partition planner for a CSV +source+, which may be a file
  # path or a seekable \IO-like object (it must respond to +seek+, +pos+
  # and +read+; open it in binary mode so byte offsets stay exact).
  #
  # The given +options+ are the usual \CSV parse options (+col_sep+,
  # +row_sep+, +quote_char+, +liberal_parsing+, +headers+, +skip_lines+,
  # ...): the planner follows the caller's separator and quoting rules,
  # and each planned partition is parsed with the same rules.
  #
  # A partitioner scans the file once and computes a partition plan: a
  # list of byte ranges that can each be parsed independently, so that
  # multiple workers (threads or processes) can read one big CSV file in
  # parallel. Every partition starts exactly at a record boundary: a
  # partition never begins in the middle of a quoted field, a CRLF row
  # separator, or a multi-byte character.
  #
  # The scan keeps only the information needed to describe the partitions
  # (byte ranges, first row numbers, and header row information); it does
  # not build an index of every row or field.
  #
  # Example: read one big file from several threads:
  #   partitioner = CSV::Partitioner.new("big.csv", headers: true)
  #   partitions = partitioner.partitions(4)
  #   partitions.map do |partition|
  #     Thread.new do
  #       partition.each("big.csv") do |row|
  #         # ...
  #       end
  #     end
  #   end.each(&:join)
  #
  class Partitioner
    # A single partition of a CSV file, planned by CSV::Partitioner.
    #
    # A partition is a byte range of the source file plus the information
    # needed to parse that range independently: the number of the first
    # record in the partition and header row information.
    class Partition
      # The byte range (<tt>start...stop</tt>, +stop+ exclusive) of this
      # partition in the source file.
      attr_reader :byte_range

      # The number of records that a serial parse with the same options
      # yields before this partition's first record (0-based). The header
      # row is not counted when the +headers+ option is enabled.
      attr_reader :first_row_number

      # The header row as an \Array of field \String objects, or +nil+
      # when the +headers+ option is not enabled. Every partition carries
      # the same header row, so partitions after the first one can still
      # produce rows with headers.
      attr_reader :headers

      def initialize(byte_range:, first_row_number:, headers:,
                     include_headers:, source:, parse_options:, encoding:,
                     own_headers:) # :nodoc:
        @byte_range = byte_range
        @first_row_number = first_row_number
        @headers = headers
        @include_headers = include_headers
        @source = source
        @parse_options = parse_options
        @encoding = encoding
        @own_headers = own_headers
      end

      # Whether this partition's byte range contains the header row.
      # Only the first partition of a plan includes the header row.
      def include_headers?
        @include_headers
      end

      # :call-seq:
      #   partition.each(source = nil) {|row| ... } -> self
      #   partition.each(source = nil) -> enumerator
      #
      # Parses this partition's byte range with the existing \CSV parse
      # interface and yields each row. +source+ may be a file path or a
      # seekable \IO; when omitted, the source given to the partitioner
      # is used (a path is re-opened for each call, an \IO is shared).
      def each(source = nil)
        return enum_for(:each, source) unless block_given?

        open_source(source) do |io|
          io.seek(@byte_range.begin)
          bytes = io.read(@byte_range.size) || ""
          data = bytes.dup.force_encoding(@encoding)
          CSV.new(data, **csv_options).each do |row|
            yield row
          end
        end
        self
      end

      # :call-seq:
      #   partition.read(source = nil) -> array_of_rows
      #
      # Returns all rows of this partition, parsed like #each does.
      def read(source = nil)
        rows = []
        each(source) {|row| rows << row}
        rows
      end

      def inspect # :nodoc:
        "#<#{self.class} byte_range=#{@byte_range} " \
        "first_row_number=#{@first_row_number}>"
      end

      private

      def csv_options
        options = @parse_options
        if @own_headers && !@include_headers
          options = options.merge(headers: @headers, return_headers: false)
        end
        options
      end

      def open_source(source)
        source ||= @source
        if source.is_a?(String) or source.respond_to?(:to_path)
          File.open(source, "rb") {|file| yield file}
        else
          position = source.pos
          begin
            yield source
          ensure
            source.seek(position)
          end
        end
      end
    end

    # The byte size of the chunks read while scanning a file.
    CHUNK_SIZE = 64 * 1024

    def initialize(source, **options)
      @source = source
      @options = options
    end

    # :call-seq:
    #   partitioner.partitions(count) -> array_of_partitions
    #   partitioner.partitions(target_size: byte_size) -> array_of_partitions
    #
    # Scans the source and returns the partition plan as an \Array of
    # CSV::Partitioner::Partition.
    #
    # Pass either +count+ (the desired number of partitions) or
    # <tt>target_size:</tt> (the desired partition size in bytes). The
    # returned plan may contain fewer partitions than requested, because
    # a partition can only start at a record boundary. An empty source
    # yields no partitions.
    def partitions(count = nil, target_size: nil)
      if count.nil? == target_size.nil?
        raise ArgumentError, "pass either count or :target_size"
      end
      if count && (!count.is_a?(Integer) || count < 1)
        raise ArgumentError, "count must be a positive Integer"
      end
      if target_size && (!target_size.is_a?(Integer) || target_size < 1)
        raise ArgumentError, ":target_size must be a positive Integer"
      end
      unless @source.is_a?(String) || @source.respond_to?(:to_path) ||
             (@source.respond_to?(:seek) && @source.respond_to?(:pos) &&
              @source.respond_to?(:read))
        raise ArgumentError, "source must be a path or a seekable IO"
      end
      with_source_io do |io|
        plan(io, count, target_size)
      end
    end

    private

    def plan(io, count, target_size)
      encoding = resolve_encoding(io)
      io.seek(0)
      size = io_size(io)
      return [] if size.zero?

      # Resolve the effective separators with the existing parser, so
      # that :auto row separator detection and encoding handling are
      # shared with a serial parse. This reads a small sample; rewind
      # afterwards.
      probe = CSV.new(io, **@options)
      row_sep = probe.row_sep
      col_sep = probe.col_sep
      quote_char = probe.quote_char
      io.seek(0)

      target_size ||= (size.to_f / count).ceil
      target_size = 1 if target_size < 1

      scanner = BoundaryScanner.new(
        io,
        encoding: encoding,
        row_sep: row_sep,
        col_sep: col_sep,
        quote_char: quote_char,
        liberal_parsing: liberal_parsing?,
        backslash_quote: backslash_quote?,
        strip: @options[:strip],
        skip_lines: @options[:skip_lines],
        skip_blanks: @options[:skip_blanks],
        chunk_size: CHUNK_SIZE
      )

      own_headers = own_headers?
      boundaries = [] # [[byte_offset, first_row_number], ...]
      header_end = nil
      data_records = 0
      next_target = target_size
      scanner.each do |record_end|
        if own_headers && header_end.nil?
          header_end = record_end
          next
        end
        data_records += 1
        if record_end >= next_target && record_end < size
          boundaries << [record_end, data_records]
          next_target = record_end + target_size
        end
      end

      headers = nil
      if own_headers && header_end
        io.seek(0)
        headers = parse_header_row(io.read(header_end), encoding, row_sep)
      end

      parse_options = @options.merge(row_sep: row_sep)
      starts = [[0, 0]] + boundaries
      starts.each_with_index.map do |(start, first_row_number), index|
        stop = index + 1 < starts.size ? starts[index + 1][0] : size
        Partition.new(
          byte_range: start...stop,
          first_row_number: first_row_number,
          headers: headers,
          include_headers: own_headers && index.zero?,
          source: @source,
          parse_options: parse_options,
          encoding: encoding,
          own_headers: own_headers
        )
      end
    end

    def with_source_io
      if @source.is_a?(String) or @source.respond_to?(:to_path)
        File.open(@source, source_file_mode) {|file| yield file}
      else
        position = @source.pos
        begin
          yield @source
        ensure
          @source.seek(position)
        end
      end
    end

    def source_file_mode
      encoding = @options[:encoding]
      encoding = encoding.split(":", 2).first if encoding.is_a?(String)
      encoding ||= Encoding::UTF_8
      "rb:#{encoding}"
    end

    def resolve_encoding(io)
      if io.respond_to?(:internal_encoding) && io.internal_encoding
        return io.internal_encoding
      end
      if io.respond_to?(:external_encoding) && io.external_encoding
        return io.external_encoding
      end
      if (encoding = @options[:internal_encoding])
        return Encoding.find(encoding)
      end
      if (encoding = @options[:encoding])
        encoding = encoding.split(":", 2).first if encoding.is_a?(String)
        return Encoding.find(encoding)
      end
      Encoding.default_internal || Encoding.default_external
    end

    def io_size(io)
      return io.size if io.respond_to?(:size)

      position = io.pos
      io.seek(0, IO::SEEK_END)
      size = io.pos
      io.seek(position)
      size
    end

    def own_headers?
      headers = @options[:headers]
      headers && !headers.is_a?(Array) && !headers.is_a?(String)
    end

    def liberal_parsing?
      !!@options[:liberal_parsing]
    end

    def backslash_quote?
      liberal_parsing = @options[:liberal_parsing]
      liberal_parsing.is_a?(Hash) && !!liberal_parsing[:backslash_quote]
    end

    # Parses the raw bytes of the header record with the caller's
    # separator and quoting rules, returning the raw header fields.
    def parse_header_row(header_bytes, encoding, row_sep)
      header = header_bytes.dup.force_encoding(encoding)
      options = @options.merge(row_sep: row_sep, headers: false)
      # The header row of a serial parse is not converted by field
      # converters, so extract the raw fields here as well.
      options.delete(:converters)
      options.delete(:header_converters)
      options.delete(:unconverted_fields)
      options.delete(:nil_value)
      options.delete(:empty_value)
      options.delete(:skip_lines)
      options.delete(:skip_blanks)
      CSV.parse_line(header, **options) || []
    end

    # Scans a CSV stream chunk by chunk and yields the byte offset just
    # past every record the CSV parser would emit (that is, the offset
    # where the next record starts). Offsets always point to a record
    # boundary: never into a quoted field, a CRLF row separator, or a
    # multi-byte character. Only a sliding window of the input is kept
    # in memory.
    #
    # This mirrors the quoting rules of CSV::Parser: a quote character
    # only opens a quoted field at the start of a field, doubled quote
    # characters are escaped quotes, and a quoted field is closed by an
    # odd run of quote characters.
    class BoundaryScanner # :nodoc:
      def initialize(io, encoding:, row_sep:, col_sep:, quote_char:,
                     liberal_parsing:, backslash_quote:, strip:,
                     skip_lines:, skip_blanks:, chunk_size:)
        @io = io
        @encoding = encoding
        @row_sep = row_sep
        @quote_char = quote_char
        @backslash_quote = backslash_quote
        @skip_blanks = skip_blanks
        @chunk_size = chunk_size

        skip_lines = skip_lines.encode(encoding) if skip_lines.is_a?(String)
        @skip_lines = skip_lines

        @buffer = (+"").force_encoding(encoding)
        @pending = (+"").b
        @scanner = StringScanner.new(@buffer)
        @base = 0
        @eof = false

        encoded = ->(string) { string.encode(encoding) }
        @re_not_line_end = Regexp.new(encoded["[^\r\n]+"])
        @re_row_end = Regexp.new(Regexp.escape(row_sep))
        @row_sep_bytesize = row_sep.bytesize
        @re_col_end = Regexp.new(Regexp.escape(col_sep))
        @col_sep_bytesize = col_sep.bytesize
        if col_sep.size > 1
          escaped_col0 = Regexp.escape(col_sep[0])
          @re_col0_run = Regexp.new(escaped_col0 + "+")
        end

        if quote_char
          escaped_quote = Regexp.escape(quote_char)
          @re_quotes = Regexp.new(escaped_quote + "+")
          @re_quote_one = Regexp.new(escaped_quote)
          @quote_bytesize = quote_char.bytesize

          no_quoted_values = escaped_quote.dup
          if backslash_quote
            escaped_backslash = Regexp.escape(encoded["\\"])
            @re_backslash = Regexp.new(escaped_backslash)
            no_quoted_values << escaped_backslash
          end
          @re_quoted_content =
            Regexp.new(encoded["[^"] + no_quoted_values + encoded["]+"])

          no_unquoted_values = Regexp.escape(row_sep)
          no_unquoted_values << Regexp.escape(col_sep[0])
          no_unquoted_values << escaped_quote unless liberal_parsing
          @re_unquoted =
            Regexp.new(encoded["[^"] + no_unquoted_values + encoded["]+"])

          strip_chars = nil
          if strip.is_a?(String)
            strip_chars = strip.encode(encoding)
          elsif strip
            strip_chars = encoded[" \t\f\v"]
          end
          if strip_chars
            @re_strip =
              Regexp.new(encoded["["] + Regexp.escape(strip_chars) +
                         encoded["]+"])
          end
        else
          @re_line_content =
            Regexp.new(encoded["[^"] + Regexp.escape(row_sep[0]) +
                       encoded["]+"])
        end
      end

      # Yields the byte offset just past each emitted record.
      def each
        return enum_for(:each) unless block_given?

        fill
        if @quote_char
          each_quotable {|position| yield position}
        else
          each_no_quote {|position| yield position}
        end
      end

      private

      # -- input -------------------------------------------------------

      def fill
        return if @eof

        chunk = @io.read(@chunk_size)
        if chunk.nil? || chunk.empty?
          @eof = true
          unless @pending.empty?
            @buffer << @pending.dup.force_encoding(@encoding)
            @pending = (+"").b
          end
          return
        end
        data = @pending << chunk.b
        encoded = data.dup.force_encoding(@encoding)
        if encoded.valid_encoding?
          @pending = (+"").b
          @buffer << encoded
        else
          tail = incomplete_tail_size(data)
          if tail.zero?
            # Genuinely invalid data; a serial parse would fail too.
            @buffer << encoded
            @pending = (+"").b
          else
            @buffer << data.byteslice(0, data.bytesize - tail).
                         force_encoding(@encoding)
            @pending = data.byteslice(data.bytesize - tail, tail)
          end
        end
      end

      # Byte size of the trailing incomplete multi-byte character.
      def incomplete_tail_size(data)
        1.upto(3) do |n|
          prefix = data.byteslice(0, data.bytesize - n) || +""
          prefix = prefix.force_encoding(@encoding)
          return n if prefix.valid_encoding?
        end
        0
      end

      def eos?
        @eof && @scanner.eos?
      end

      def position
        @base + @scanner.pos
      end

      # Drops the consumed part of the buffer to bound memory usage.
      # Only call this when no scan position needs to be restored.
      def compact_buffer
        pos = @scanner.pos
        return if pos < @chunk_size

        @buffer.replace(@buffer.byteslice(pos, @buffer.bytesize - pos) || +"")
        @base += pos
        @scanner.pos = 0
      end

      # Scans a variable-length token that may safely be consumed in
      # pieces (record content). Refills the buffer when the token
      # reaches the end of the buffer.
      def scan_continue(regexp, collect: false, compact: true)
        collected = (+"").force_encoding(@encoding) if collect
        matched = false
        loop do
          if (result = @scanner.scan(regexp))
            matched = true
            collected << result if collect
          end
          if !@eof && @scanner.eos?
            compact_buffer if compact
            fill
            next
          end
          break
        end
        collect ? (matched ? collected : nil) : matched
      end

      # Scans a variable-length token that must be consumed as a whole
      # (a run of quote characters: its parity matters). Refills and
      # retries when the run reaches the end of the buffer.
      def scan_rewindable(regexp)
        loop do
          start = @scanner.pos
          result = @scanner.scan(regexp)
          return result if @eof || !@scanner.eos?

          fill
          @scanner.pos = start
        end
      end

      # Scans a fixed-length token (a separator), refilling the buffer
      # until the token cannot be split by a chunk boundary.
      def scan_fixed(regexp, token_bytesize)
        while !@eof && (@buffer.bytesize - @scanner.pos) < token_bytesize
          fill
        end
        @scanner.scan(regexp)
      end

      def getch
        fill if !@eof && @scanner.eos?
        @scanner.getch
      end

      # -- shared ------------------------------------------------------

      def skip_line?(line)
        case @skip_lines
        when String
          line.include?(@skip_lines)
        when Regexp
          @skip_lines.match?(line)
        else
          @skip_lines.match(line)
        end
      end

      def skip_needless_lines
        return unless @skip_lines

        until eos?
          start = @scanner.pos
          line = scan_continue(@re_not_line_end, collect: true,
                               compact: false)
          line ||= (+"").force_encoding(@encoding)
          scan_fixed(@re_row_end, @row_sep_bytesize)
          if skip_line?(line)
            compact_buffer
          else
            @scanner.pos = start
            break
          end
        end
      end

      # -- quotable (quote_char is set) --------------------------------

      def each_quotable
        record_has_content = false
        state = :record_start
        loop do
          case state
          when :record_start
            break if eos?

            skip_needless_lines
            record_has_content = false
            state = :field
          when :field
            scan_continue(@re_strip) if @re_strip
            if (quotes = scan_rewindable(@re_quotes))
              record_has_content = true
              consume_quoted_content if quotes.length.odd?
              state = :after_quoted
            elsif scan_continue(@re_unquoted)
              record_has_content = true
              state = :unquoted
            else
              state = :field_end
            end
          when :unquoted
            next if scan_continue(@re_unquoted)

            state = :delimited
          when :after_quoted
            scan_continue(@re_strip) if @re_strip
            state = :delimited
          when :field_end, :delimited
            if scan_fixed(@re_col_end, @col_sep_bytesize)
              record_has_content = true
              state = :field
            elsif scan_fixed(@re_row_end, @row_sep_bytesize)
              unless !record_has_content && @skip_blanks
                yield position
              end
              compact_buffer
              state = :record_start
            elsif eos?
              yield position if record_has_content
              break
            else
              # A character the CSV parser would reject (for example a
              # quote character inside an unquoted field); skip it and
              # keep scanning.
              getch
              state = :unquoted
            end
          end
        end
      end

      # Consumes the rest of a quoted field, up to and including the
      # closing (odd) run of quote characters.
      def consume_quoted_content
        loop do
          scan_continue(@re_quoted_content)
          if @backslash_quote && scan_fixed(@re_backslash, 1)
            scan_fixed(@re_quote_one, @quote_bytesize)
            next
          end
          return if eos?

          quotes = scan_rewindable(@re_quotes)
          return if quotes.nil? # unclosed quoted field
          return if quotes.length.odd?
        end
      end

      # -- no quote character ------------------------------------------

      def each_no_quote
        until eos?
          line = (+"").force_encoding(@encoding) if @skip_lines
          has_content = false
          loop do
            if (part = scan_continue(@re_line_content, collect: !line.nil?))
              has_content = true
              line << part if line
            end
            if scan_fixed(@re_row_end, @row_sep_bytesize)
              break
            elsif eos?
              break
            else
              # A prefix of the row separator that does not complete it.
              char = getch
              has_content = true
              line << char if line
            end
          end
          next if line && skip_line?(line)
          next if !has_content && @skip_blanks

          yield position
          compact_buffer
        end
      end
    end

    private_constant :BoundaryScanner
  end
end
