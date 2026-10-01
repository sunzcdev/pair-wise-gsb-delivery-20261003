# frozen_string_literal: true

require 'strscan'
require 'tomlrb/scanner'

module Tomlrb
  # A scanner that yields every byte of a TOML document (whitespace,
  # newlines and comments included) together with the byte range it
  # occupies.  Unlike {Tomlrb::Scanner} it never discards layout, which is
  # what allows {Document} to re-emit untouched regions verbatim.
  class DocumentScanner
    COMMENT          = Scanner::COMMENT
    IDENTIFIER       = Scanner::IDENTIFIER
    STRING_BASIC     = Scanner::STRING_BASIC
    STRING_MULTI     = Scanner::STRING_MULTI
    STRING_LITERAL   = Scanner::STRING_LITERAL
    STRING_LITERAL_MULTI = Scanner::STRING_LITERAL_MULTI
    DATETIME         = Scanner::DATETIME
    LOCAL_TIME       = Scanner::LOCAL_TIME
    FLOAT            = Scanner::FLOAT
    FLOAT_KEYWORD    = Scanner::FLOAT_KEYWORD
    INTEGER          = Scanner::INTEGER
    NON_DEC_INTEGER  = Scanner::NON_DEC_INTEGER
    BOOLEAN          = Scanner::BOOLEAN

    Token = Struct.new(:type, :text, :from, :to) do
      def range
        (from...to)
      end
    end

    def initialize(text)
      @ss = StringScanner.new(text)
    end

    def each_token
      return to_enum(:each_token) unless block_given?

      until @ss.eos?
        start = @ss.pos
        type, text = scan_one
        raise DocumentParseError, "Unexpected character at byte #{start}" if type.nil?
        yield Token.new(type, text, start, @ss.pos)
      end
      yield Token.new(:EOS, nil, @ss.pos, @ss.pos)
    end

    private

    def scan_one
      case
      when (text = @ss.scan(/[ \t]+/))      then [:WS, text]
      when (text = @ss.scan(/\r?\n/))       then [:NEWLINE, text]
      when (text = @ss.scan(COMMENT))       then [:COMMENT, text]
      when (text = @ss.scan(STRING_MULTI))  then [:STRING_MULTI, text]
      when (text = @ss.scan(STRING_LITERAL_MULTI)) then [:STRING_LITERAL_MULTI, text]
      when (text = @ss.scan(STRING_BASIC))  then [:STRING_BASIC, text]
      when (text = @ss.scan(STRING_LITERAL)) then [:STRING_LITERAL, text]
      when (text = @ss.scan(DATETIME))      then [:DATETIME, text]
      when (text = @ss.scan(LOCAL_TIME))    then [:LOCAL_TIME, text]
      when (text = @ss.scan(FLOAT))         then [:FLOAT, text]
      when (text = @ss.scan(FLOAT_KEYWORD)) then [:FLOAT_KEYWORD, text]
      when (text = @ss.scan(NON_DEC_INTEGER)) then [:NON_DEC_INTEGER, text]
      when (text = @ss.scan(INTEGER))       then [:INTEGER, text]
      when (text = @ss.scan(BOOLEAN))       then [:BOOLEAN, text]
      when (text = @ss.scan(IDENTIFIER))    then [:IDENTIFIER, text]
      when (text = @ss.scan(/./m))          then [text, text]
      end
    end
  end
end
