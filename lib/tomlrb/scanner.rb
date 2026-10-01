# frozen-string-literal: true

require 'strscan'

module Tomlrb
  class Scanner
    COMMENT =
      /#[^\u0000-\u0008\u000A-\u001F\u007F]*/.freeze
    IDENTIFIER =
      /[A-Za-z0-9_-]+/.freeze
    SPACE =
      /[ \t]/.freeze
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

    def initialize(io)
      @ss = StringScanner.new(io.read)
      @eos = false
      @line = 1
      @line_start_pos = 0
      @location = Location.new(1, 1)
    end

    attr_reader :location

    def next_token
      start_pos = @ss.pos
      start_line = @line
      start_column = start_pos - @line_start_pos + 1
      case
      when @ss.scan(NEWLINE)
        update_location(@ss[0], start_pos)
        [:NEWLINE, nil]
      when @ss.scan(SPACED_ARRAY_OF_TABLES_START)
        update_location(@ss[0], start_pos)
        raise ParseError.new('Array of tables has spaces in starting brackets')
      when @ss.scan(SPACED_ARRAY_OF_TABLES_END)
        update_location(@ss[0], start_pos)
        raise ParseError.new('Array of tables has spaces in ending brackets')
      when @ss.scan(SPACED_ARRAY_OF_TABLES_BOTH)
        update_location(@ss[0], start_pos)
        raise ParseError.new('Array of tables has spaces in starting and ending brackets')
      when @ss.scan(SPACE)
        update_location(@ss[0], start_pos)
        next_token
      when @ss.scan(COMMENT)
        update_location(@ss[0], start_pos)
        next_token
      when @ss.scan(DATETIME)
        @location = Location.new(start_line, start_column)
        process_datetime
      when @ss.scan(LOCAL_TIME)
        @location = Location.new(start_line, start_column)
        process_local_time
      when text = @ss.scan(STRING_MULTI)
        @location = Location.new(start_line, start_column)
        update_location(text, start_pos)
        [:STRING_MULTI, text[3..-4]]
      when text = @ss.scan(STRING_BASIC)
        @location = Location.new(start_line, start_column)
        [:STRING_BASIC, text[1..-2]]
      when text = @ss.scan(STRING_LITERAL_MULTI)
        @location = Location.new(start_line, start_column)
        update_location(text, start_pos)
        [:STRING_LITERAL_MULTI, text[3..-4]]
      when text = @ss.scan(STRING_LITERAL)
        @location = Location.new(start_line, start_column)
        [:STRING_LITERAL, text[1..-2]]
      when text = @ss.scan(FLOAT)
        @location = Location.new(start_line, start_column)
        [:FLOAT, text]
      when text = @ss.scan(FLOAT_KEYWORD)
        @location = Location.new(start_line, start_column)
        [:FLOAT_KEYWORD, text]
      when text = @ss.scan(INTEGER)
        @location = Location.new(start_line, start_column)
        [:INTEGER, text]
      when text = @ss.scan(NON_DEC_INTEGER)
        @location = Location.new(start_line, start_column)
        [:NON_DEC_INTEGER, text]
      when text = @ss.scan(BOOLEAN)
        @location = Location.new(start_line, start_column)
        [:BOOLEAN, text]
      when text = @ss.scan(IDENTIFIER)
        @location = Location.new(start_line, start_column)
        [:IDENTIFIER, text]
      when @ss.eos?
        process_eos
      else
        x = @ss.getch
        @location = Location.new(start_line, start_column)
        update_location(x, start_pos)
        [x, x]
      end
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

    private

    def update_location(matched, start_pos)
      newline_count = matched.count("\n")
      unless newline_count.zero?
        @line += newline_count
        @line_start_pos = start_pos + matched.rindex("\n") + 1
      end
    end
  end
end
