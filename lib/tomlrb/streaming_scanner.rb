# frozen_string_literal: true

require 'strscan'

module Tomlrb
  # A tokenizer that pulls data from any chunked IO instead of requiring the
  # whole document as one string.
  #
  # It only keeps the bytes that have not yet been turned into tokens (plus a
  # single byte of lookahead). A chunk boundary may therefore legally fall in
  # the middle of a UTF-8 character, an escape sequence or a (multi-line)
  # string. Conditions that can only be decided once the IO is exhausted -
  # unterminated strings, dangling escapes, incomplete UTF-8 sequences and
  # unclosed constructs - raise {ParseError} from the final {#next_token}
  # call, which returns +nil+ on a clean end of input.
  class StreamingScanner
    COMMENT =
      /#[^\u0000-\u0008\u000A-\u001F\u007F]*/.freeze
    IDENTIFIER =
      /[A-Za-z0-9_-]+/.freeze
    SPACE =
      /[ \t]+/.freeze
    NEWLINE =
      /(?:[ \t]*(?:\r?\n)[ \t]*)+/.freeze
    STRING_BASIC =
      /(")(?:\\?[^\u0000-\u0008\u000A-\u001F\u007F\\]|(?:\\[^\u0000-\u0008\u000A-\u001F\u007F]))*?\1/.freeze
    STRING_MULTI =
      /"{3}([^\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F]*?(?<!\\)"{3,5})/m.freeze
    STRING_LITERAL =
      /(')(?:\\?[^\u0000-\u0008\u000A-\u001F\u007F])*?\1/.freeze
    STRING_LITERAL_MULTI =
      /'{3}([^\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F]*?'{3,5})/m.freeze
    DATETIME =
      /(-?\d{4})-([01]\d)-([0-3]\d)(?:(?:t|\s)([0-2]\d):([0-5]\d):([0-6]\d(?:\.\d+)?))?(z|[-+][01]\d:\d{2})?/i.freeze
    LOCAL_TIME =
      /([0-2]\d):([0-5]\d):([0-6]\d(?:\.\d+)?)/.freeze
    FLOAT =
      /[+-]?(?:(?:\d|[1-9](?:_?\d)*)\.\d(?:_?\d)*|\d+(?=[eE]))(?:[eE][+-]?[0-9]+(_[0-9])*[0-9]*)?(?!\w)/.freeze
    FLOAT_KEYWORD =
      /[+-]?(?:inf|nan)\b/.freeze
    INTEGER =
      /[+-]?([1-9](_?\d)*|0)(?![A-Za-z0-9_-]+)/.freeze
    NON_DEC_INTEGER =
      /0(?:x[0-9A-Fa-f](_?[0-9A-Fa-f])*|o[0-7](_?[0-7])*|b[01](_?[01])*)/.freeze
    BOOLEAN =
      /true|false/.freeze
    SPACED_ARRAY_OF_TABLES_START =
      /^\[[ \t]+\[(#{IDENTIFIER}|#{STRING_BASIC}|#{STRING_LITERAL}|#{INTEGER}|#{NON_DEC_INTEGER}|#{FLOAT_KEYWORD}|#{BOOLEAN})\]\]$/.freeze
    SPACED_ARRAY_OF_TABLES_END =
      /^\[\[(#{IDENTIFIER}|#{STRING_BASIC}|#{STRING_LITERAL}|#{INTEGER}|#{NON_DEC_INTEGER}|#{FLOAT_KEYWORD}|#{BOOLEAN})\][ \t]+\]$/.freeze
    SPACED_ARRAY_OF_TABLES_BOTH =
      /^\[[ \t]+\[(#{IDENTIFIER}|#{STRING_BASIC}|#{STRING_LITERAL}|#{INTEGER}|#{NON_DEC_INTEGER}|#{FLOAT_KEYWORD}|#{BOOLEAN})\][ \t]+\]$/.freeze

    # @param io [#read, #getbyte] object delivering the document in chunks
    # @param chunk_size [Integer] maximum number of bytes requested per read
    def initialize(io, chunk_size: 4096)
      @io = io
      @chunk_size = chunk_size
      @buffer = String.new(encoding: Encoding::BINARY)
      @pos = 0
      @eof = false
      @eos_emitted = false
    end

    # @return [Array, nil] the next racc token, or +nil+ at a clean EOF
    def next_token
      loop do
        result = scan
        case result
        when :retry
          pull_more
        when :restart
          compact!
        else
          compact!
          return result
        end
      end
    end

    def pull_more
      chunk = read_chunk unless @eof
      if chunk.nil? || chunk.empty?
        @eof = true
      else
        @buffer << chunk
      end
    end

    private

    def scan
      refill_buffer if @pos >= @buffer.length

      scanner = StringScanner.new(@buffer)
      scanner.pos = @pos

      # Whitespace and comments are skipped without a token. They still need
      # the lookahead guarantee: a chunk can split a comment or indentation.
      while scanner.skip(SPACE) || scanner.skip(COMMENT)
        return :retry if !@eof && at_buffer_end?(scanner)

        @pos = scanner.pos
      end

      if scanner.scan(NEWLINE)
        return :retry unless final?(scanner)

        return token(scanner, [:NEWLINE, nil])
      end

      spaced = check_spaced_array_of_tables(scanner)
      return spaced if spaced == :retry
      if scanner.scan(DATETIME)
        return :retry unless final?(scanner) && !datetime_continuation?(scanner)

        return token(scanner, process_datetime(utf8(scanner.matched)))
      elsif scanner.scan(LOCAL_TIME)
        return :retry unless final?(scanner) && !numeric_continuation?(scanner, scanner.matched)

        return token(scanner, process_local_time(utf8(scanner.matched)))
      elsif (text = scanner.scan(STRING_MULTI))
        return :retry unless final?(scanner)

        return token(scanner, [:STRING_MULTI, utf8(text[3..-4])])
      elsif (text = scanner.scan(STRING_LITERAL_MULTI))
        return :retry unless final?(scanner)

        return token(scanner, [:STRING_LITERAL_MULTI, utf8(text[3..-4])])
      elsif scanner.peek(1) == '"' || scanner.peek(1) == "'"
        decision = string_start_decision(scanner)
        return decision if decision == :retry || (decision.is_a?(Array))

        # A closeable single-line string (value or quoted key); fall through
        # to the regular string regexes below via a fresh match attempt.
        if (text = scanner.scan(STRING_BASIC))
          return :retry unless final?(scanner)

          return token(scanner, [:STRING_BASIC, utf8(text[1..-2])])
        elsif (text = scanner.scan(STRING_LITERAL))
          return :retry unless final?(scanner)

          return token(scanner, [:STRING_LITERAL, utf8(text[1..-2])])
        elsif @eof
          raise_unterminated_single_line(scanner)
        elsif (rest = scanner.rest) && (rest.include?("\n") || rest.include?("\r"))
          # A single-line string cannot span the line break already buffered:
          # this opening quote is genuinely stray.
          char = scanner.getch
          @pos = scanner.pos
          [char, char]
        else
          # Opening quote without a closing quote in the buffer yet.
          :retry
        end
      elsif (text = scanner.scan(FLOAT))
        return :retry unless final?(scanner) && !numeric_continuation?(scanner, text)

        return token(scanner, [:FLOAT, utf8(text)])
      elsif (text = scanner.scan(FLOAT_KEYWORD))
        return :retry unless final?(scanner)

        return token(scanner, [:FLOAT_KEYWORD, utf8(text)])
      elsif (text = scanner.scan(INTEGER))
        return :retry unless final?(scanner) && !integer_prefix_at_end?(scanner)

        return token(scanner, [:INTEGER, utf8(text)])
      elsif (text = scanner.scan(NON_DEC_INTEGER))
        return :retry unless final?(scanner) && !integer_prefix_at_end?(scanner)

        return token(scanner, [:NON_DEC_INTEGER, utf8(text)])
      elsif (text = scanner.scan(BOOLEAN))
        return :retry unless final?(scanner)

        return token(scanner, [:BOOLEAN, utf8(text)])
      elsif (text = scanner.scan(IDENTIFIER))
        return :retry unless final?(scanner)

        return token(scanner, [:IDENTIFIER, utf8(text)])
      elsif @eof && scanner.eos?
        @pos = scanner.pos
        return nil if @eos_emitted

        @eos_emitted = true
        [:EOS, nil]
      else
        # A leading digit matching no complete number/date/time token may be
        # a construct split by a chunk boundary (e.g. `07:3` waiting for
        # `:00`); hold it until more bytes or EOF decide.
        if !@eof && digit_byte?(@buffer.getbyte(scanner.pos))
          return :retry
        end

        return unexpected_byte(scanner)
      end
    end

    # Records the scanner position and hands the completed token back. The
    # caller compacts the buffer once this returns, so the scanner is never
    # used again after bytes underneath it have been discarded.
    def token(scanner, token)
      @pos = scanner.pos
      token
    end

    def refill_buffer
      return if @eof

      chunk = read_chunk
      if chunk.nil? || chunk.empty?
        @eof = true
      else
        @buffer << chunk
      end
    end

    def read_chunk
      if @io.respond_to?(:read)
        return @io.read(@chunk_size)
      end

      # Minimal #getbyte support: assemble one chunk from individual bytes.
      byte = @io.getbyte
      return nil if byte.nil?

      chunk = byte.chr(Encoding::BINARY)
      (@chunk_size - 1).times do
        more = @io.getbyte
        break if more.nil?

        chunk << more
      end
      chunk
    end

    # Discards consumed prefix bytes. Runs only between tokens, never while a
    # scanner is still matching, so positions cannot invalidate mid-scan.
    def compact!
      return if @pos.zero?
      return if @pos >= @buffer.length && @buffer.empty?

      if @pos >= @buffer.length
        @buffer = String.new(encoding: Encoding::BINARY)
      else
        @buffer = @buffer.byteslice(@pos, @buffer.length - @pos)
      end
      @pos = 0
    end

    # A regex match describes a complete token only once either EOF is known
    # or at least one unconsumed byte remains. Without this guard a number
    # split over two chunks would be tokenized too early.
    def final?(scanner)
      @eof || scanner.pos < @buffer.length
    end

    def at_buffer_end?(scanner)
      scanner.pos >= @buffer.length
    end

    # Rejects `[[ name]]` / `[[name ]]` style headers. The check must run on
    # the whole header line before its opening brackets are consumed as
    # individual tokens, and only once the line is fully present.
    def check_spaced_array_of_tables(scanner)
      rest = scanner.rest
      # This rule only applies at the start of a line that begins with a
      # table-header bracket; anywhere else it must not block tokenization.
      return nil unless scanner.bol? && rest.start_with?('[')

      line = rest.index("\n") || rest.index("\r")
      # Header line not fully buffered yet; wait for its end before deciding
      # whether the brackets contain illegal interior spaces.
      return :retry if line.nil? && !@eof

      text = line ? rest.byteslice(0, line) : rest
      return nil if text.nil?

      if SPACED_ARRAY_OF_TABLES_START.match?(text)
        raise ParseError, 'Array of tables has spaces in starting brackets'
      elsif SPACED_ARRAY_OF_TABLES_END.match?(text)
        raise ParseError, 'Array of tables has spaces in ending brackets'
      elsif SPACED_ARRAY_OF_TABLES_BOTH.match?(text)
        raise ParseError, 'Array of tables has spaces in starting and ending brackets'
      end
      nil
    end

    def unexpected_byte(scanner)
      byte = @buffer.getbyte(scanner.pos)

      # A lone "\r" or an incomplete multi-byte UTF-8 sequence sitting on a
      # chunk boundary cannot be rejected until more bytes arrive.
      if !@eof
        return :retry if byte == 0x0D && scanner.pos + 1 >= @buffer.length
        return :retry if incomplete_utf8_tail?(scanner.pos)
      end

      raise ParseError, 'Invalid UTF-8 byte sequence' if invalid_utf8_byte?(byte)

      char = scanner.getch
      @pos = scanner.pos
      [char, char]
    end

    def utf8_lead_byte?(byte)
      (0xC2..0xF4).include?(byte)
    end

    def invalid_utf8_byte?(byte)
      (0x80..0xC1).include?(byte) || (0xF5..0xFF).include?(byte)
    end

    def digit_byte?(byte)
      byte && (0x30..0x39).include?(byte)
    end

    # True when the bytes from +start+ form a valid prefix of a UTF-8 encoding
    # but are missing one or more continuation bytes to complete a character.
    def incomplete_utf8_tail?(start)
      byte = @buffer.getbyte(start)
      return false unless byte

      expected =
        if (0xC2..0xDF).include?(byte) then 1
        elsif (0xE0..0xEF).include?(byte) then 2
        elsif (0xF0..0xF4).include?(byte) then 3
        else
          return false
        end

      available = 0
      (1..expected).each do |offset|
        following = @buffer.getbyte(start + offset)
        break unless following && (0x80..0xBF).include?(following)

        available += 1
      end

      # If every remaining byte is a valid continuation byte but there are not
      # enough of them yet, the character is simply split at the boundary.
      available < expected &&
        (start + 1 + available) >= @buffer.length
    end

    # Decides what to do at a quote that did not form a complete multi-line
    # token. Returns +:retry+ while the construct is still undecidable across
    # a chunk boundary, a stray quote token, or raises at EOF. Returns +nil+
    # when it is a regular (possibly empty) single-line string that the normal
    # regexes should tokenize.
    def string_start_decision(scanner)
      quote = scanner.peek(1)
      rest = scanner.rest

      if rest.start_with?(quote * 3)
        # A complete multi-line token was already handled above; reaching here
        # means an opening `"""` with no closing delimiter in the buffer.
        raise ParseError, 'Unterminated multi-line string' if @eof

        return :retry
      end

      # One or two quote bytes running exactly to the buffer end are only
      # undecidable where a string can *start* (after = . [ , { or space).
      # A closing quote follows string content and must not be held back.
      if !@eof && (rest == quote || rest == quote * 2) &&
         string_opening_position?(scanner)
        return :retry
      end

      # Otherwise a closing quote may exist later in the already buffered
      # bytes (this covers quoted keys, empty strings and full documents read
      # in one chunk). The caller tries the single-line regex first; only if
      # that fails is the quote genuinely stray or the string unterminated.
      :fallthrough
    end

    # Whether the scanner sits where a quoted string may begin (rather than
    # inside one already being read).
    def string_opening_position?(scanner)
      return true if scanner.pos.zero?

      previous = @buffer.getbyte(scanner.pos - 1)
      case previous
      when 0x20, 0x09, 0x0A, 0x0D, 0x3D, 0x2E, 0x5B, 0x2C, 0x7B, 0x5D, 0x7D
        true
      else
        false
      end
    end

    def raise_unterminated_single_line(scanner)
      rest = scanner.rest
      raise ParseError, 'Unterminated string (dangling escape sequence)' if dangling_backslash?(rest)

      raise ParseError, 'Unterminated string'
    end

    def dangling_backslash?(rest)
      index = rest.length - 1
      count = 0
      while index >= 0 && rest.getbyte(index) == 0x5C
        count += 1
        index -= 1
      end
      count.odd?
    end

    # True when a date matched at the buffer end may still be extended by a
    # time component in the next chunk.
    def datetime_continuation?(scanner)
      return false if @eof

      next_byte = @buffer.getbyte(scanner.pos)
      return true if next_byte == 0x54 || next_byte == 0x74 # T/t
      return true if next_byte == 0x20 # space, optional time may follow

      numeric_continuation?(scanner, scanner.matched)
    end

    # Numeric/time tokens are digit sequences joined by `- : . e E _ +`. The
    # match cannot be finalized when it runs to the buffer end on a byte that
    # could be continued, or directly before a separator byte already present
    # in the buffer.
    def numeric_continuation?(scanner, text)
      return false if @eof

      if at_buffer_end?(scanner)
        last = text.getbyte(text.bytesize - 1)
        return true if (0x30..0x39).include?(last)

        return [0x2E, 0x65, 0x45, 0x2B, 0x2D, 0x3A, 0x5F].include?(last)
      end

      # A separator directly following the matched number indicates a time
      # (`:`), fractional/exponent part (`.`, `e`) or date (`-`) continues.
      following = @buffer.getbyte(scanner.pos)
      [0x3A, 0x2E].include?(following) && digit_byte?(text.getbyte(text.bytesize - 1))
    end

    def integer_prefix_at_end?(scanner)
      numeric_continuation?(scanner, scanner.matched)
    end

    def utf8(token)
      return token if token.ascii_only?

      token.dup.force_encoding(Encoding::UTF_8).tap do |forced|
        raise ParseError, 'Invalid UTF-8 byte sequence' unless forced.valid_encoding?
      end
    end

    def process_datetime(match)
      scanner = StringScanner.new(match)
      scanner.scan(DATETIME)
      offset = scanner[7].gsub(/[zZ]/, '+00:00') if scanner[7]
      args = [scanner[0], scanner[1], scanner[2], scanner[3], scanner[4], scanner[5], scanner[6], offset]
      [:DATETIME, args]
    end

    def process_local_time(match)
      scanner = StringScanner.new(match)
      scanner.scan(LOCAL_TIME)
      args = [scanner[0], scanner[1], scanner[2], scanner[3].to_f]
      [:LOCAL_TIME, args]
    end
  end
end
