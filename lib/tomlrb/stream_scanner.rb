# frozen-string-literal: true

require 'strscan'

module Tomlrb
  # Tokenizes TOML pulled from an IO in chunks.
  #
  # Unlike Scanner, which turns the whole input into a single String up
  # front, StreamScanner reads from the IO on demand and only keeps a
  # bounded window of already consumed input around. A chunk boundary may
  # fall in the middle of a UTF-8 character, an escape sequence or a
  # multi-line string: a token is only emitted once it is provably
  # complete, i.e. once the rest of its line is buffered (no TOML token
  # except multi-line strings spans a newline, and those are delimited by
  # their closing quotes) or the IO is exhausted. Errors that can only be
  # detected at end of input - an unterminated string, a truncated UTF-8
  # sequence - are raised as Tomlrb::ParseError once the IO returns EOF.
  class StreamScanner
    DEFAULT_CHUNK_SIZE = 16 * 1024
    TRIM_THRESHOLD = 64 * 1024

    def initialize(io, chunk_size: DEFAULT_CHUNK_SIZE)
      @io = io
      @chunk_size = chunk_size
      @pending = +''.b # raw bytes that may be the start of a multi-byte UTF-8 character
      @buffer = +''    # consumed-input window, always valid UTF-8
      @buffer.force_encoding(Encoding::UTF_8)
      @ss = StringScanner.new(@buffer)
      @eof = false
      @eos = false
    end

    def next_token
      until @eof
        token = catch(:incomplete) { scan_token }
        return token if token

        refill
      end
      scan_token
    end

    private

    def scan_token
      loop do
        check_spaced_array_of_tables
        case
        when scan(Scanner::NEWLINE) then return [:NEWLINE, nil]
        when scan(Scanner::SPACE) then next
        when scan(Scanner::COMMENT) then next
        when scan(Scanner::DATETIME) then return process_datetime
        when scan(Scanner::LOCAL_TIME) then return process_local_time
        when text = scan(Scanner::STRING_MULTI) then return [:STRING_MULTI, text[3..-4]]
        when text = scan(Scanner::STRING_BASIC) then return string_token(:STRING_BASIC, text)
        when text = scan(Scanner::STRING_LITERAL_MULTI) then return [:STRING_LITERAL_MULTI, text[3..-4]]
        when text = scan(Scanner::STRING_LITERAL) then return string_token(:STRING_LITERAL, text)
        when text = scan(Scanner::FLOAT) then return [:FLOAT, text]
        when text = scan(Scanner::FLOAT_KEYWORD) then return [:FLOAT_KEYWORD, text]
        when text = scan(Scanner::INTEGER) then return [:INTEGER, text]
        when text = scan(Scanner::NON_DEC_INTEGER) then return [:NON_DEC_INTEGER, text]
        when text = scan(Scanner::BOOLEAN) then return [:BOOLEAN, text]
        when text = scan(Scanner::IDENTIFIER) then return [:IDENTIFIER, text]
        else return getch_token
        end
      end
    end

    # Matches +regexp+ at the current position. Returns the matched text,
    # or nil when there is no match. A match is only final once the rest
    # of its line is buffered (or the IO is exhausted): no TOML token
    # except multi-line strings spans a newline, and those are delimited
    # by their closing quotes, so a newline after the end of the match
    # proves the token cannot grow any further (more digits, an optional
    # time part after a date, more comment text, a longer run of quotes,
    # ...). Otherwise the position is restored and :incomplete is thrown
    # so the caller refills the buffer and retries.
    def scan(regexp)
      pos = @ss.pos
      matched = @ss.scan(regexp)
      return nil unless matched

      if !@eof && !newline_ahead?
        @ss.pos = pos
        throw :incomplete
      end
      matched
    end

    def newline_ahead?
      # String#index is used instead of StringScanner#check_until because
      # the latter would clobber the match registers (@ss[1] etc.) that
      # process_datetime and process_local_time rely on.
      !@buffer.index("\n", @ss.charpos).nil?
    end

    def getch_token
      if @ss.eos?
        return process_eos if @eof

        return nil
      end

      char = @ss.peek(1)
      unless @eof
        # Wait until the rest of the line is buffered: any character may
        # be the beginning of a longer token (string, number, comment,
        # ...), and '[' additionally needs its whole line so the spaced
        # array-of-tables errors stay accurate.
        return nil unless line_complete?
      end

      # A quote at token position always opens a string. If no string
      # regexp matched even though the line is complete (or EOF is
      # reached), the string is unterminated. Multi-line string openers
      # never reach this point: their first two quotes already matched as
      # an empty string, which string_token holds back.
      raise ParseError, 'Unterminated string' if char == '"' || char == "'"

      @ss.getch
      [char, char]
    end

    # An empty string matched right before another quote of the same kind
    # may actually be the opening of a multi-line string ("""...""") whose
    # body has not been received yet: wait for more input before deciding.
    # The newline check in #scan cannot catch this: the newline ending the
    # first line of a multi-line string would wrongly finalize the empty
    # string.
    def string_token(type, text)
      if !@eof && text == text[0] * 2 && @ss.peek(1) == text[0]
        @ss.pos -= 2
        throw :incomplete
      end
      [type, text[1..-2]]
    end

    def check_spaced_array_of_tables
      return unless @ss.peek(1) == '['
      return unless line_complete?

      if @ss.scan(Scanner::SPACED_ARRAY_OF_TABLES_START)
        raise ParseError, 'Array of tables has spaces in starting brackets'
      elsif @ss.scan(Scanner::SPACED_ARRAY_OF_TABLES_END)
        raise ParseError, 'Array of tables has spaces in ending brackets'
      elsif @ss.scan(Scanner::SPACED_ARRAY_OF_TABLES_BOTH)
        raise ParseError, 'Array of tables has spaces in starting and ending brackets'
      end
    end

    def line_complete?
      @eof || newline_ahead?
    end

    def refill
      return if @eof

      trim_buffer
      chunk = @io.read(@chunk_size)
      if chunk.nil? || chunk.empty?
        @eof = true
        raise ParseError, 'Invalid or truncated UTF-8 byte sequence' unless @pending.empty?

        return
      end

      @pending << chunk.b
      complete = extract_complete_utf8
      raise ParseError, 'Invalid byte sequence in UTF-8' if complete.nil?

      @buffer << complete
    end

    # Splits @pending into the longest prefix that is valid UTF-8 and a
    # remainder of at most 3 bytes that may be the beginning of a
    # multi-byte character split across chunks. Returns the prefix, or nil
    # when the bytes are not valid UTF-8 at all.
    def extract_complete_utf8
      candidate = @pending.dup.force_encoding(Encoding::UTF_8)
      if candidate.valid_encoding?
        @pending = +''.b
        return candidate
      end

      1.upto(3) do |i|
        prefix = @pending[0...-i]
        next if prefix.nil?

        candidate = prefix.dup.force_encoding(Encoding::UTF_8)
        if candidate.valid_encoding?
          @pending = @pending[-i..-1]
          return candidate
        end
      end

      nil
    end

    # Drops the already consumed prefix of the buffer, but only up to a
    # line boundary so that the `^` anchors used by the spaced
    # array-of-tables regexps keep their "beginning of line" meaning.
    def trim_buffer
      byte_pos = @ss.pos
      return if byte_pos < TRIM_THRESHOLD

      char_pos = @ss.charpos
      return if char_pos.zero?

      newline = @buffer.rindex("\n", char_pos - 1)
      return unless newline

      consumed = @buffer[0..newline]
      @buffer.replace(@buffer[(newline + 1)..-1])
      @ss.string = @buffer
      @ss.pos = byte_pos - consumed.bytesize
    end

    def process_datetime
      offset = @ss[7].gsub(/[zZ]/, '+00:00') if @ss[7]
      args = [@ss[0], @ss[1], @ss[2], @ss[3], @ss[4], @ss[5], @ss[6], offset]
      [:DATETIME, args]
    end

    def process_local_time
      args = [@ss[0], @ss[1], @ss[2], @ss[3].to_f]
      [:LOCAL_TIME, args]
    end

    def process_eos
      return if @eos

      @eos = true
      [:EOS, nil]
    end
  end
end
