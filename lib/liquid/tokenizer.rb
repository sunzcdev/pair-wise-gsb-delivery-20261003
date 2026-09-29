# frozen_string_literal: true

require "strscan"

module Liquid
  class Tokenizer
    attr_reader :line_number, :for_liquid_tag, :token_position

    TAG_END = /%\}/
    TAG_OR_VARIABLE_START = /\{[\{\%]/
    NEWLINE = /\n/

    OPEN_CURLEY = "{".ord
    CLOSE_CURLEY = "}".ord
    PERCENTAGE = "%".ord

    def initialize(
      source:,
      string_scanner:,
      line_numbers: false,
      line_number: nil,
      for_liquid_tag: false,
      positions: false
    )
      @line_number = line_number || (line_numbers ? 1 : nil)
      @for_liquid_tag = for_liquid_tag
      @source = source.to_s.to_str
      @offset = 0
      @tokens = []
      @positions = positions && !for_liquid_tag
      @token_position = nil

      if @source
        @ss = string_scanner
        @ss.string = @source
        compute_line_offsets if @positions
        tokenize
      end
    end

    def shift
      token = @tokens[@offset]

      return unless token

      compute_token_position if @positions

      @offset += 1

      if @line_number
        @line_number += @for_liquid_tag ? 1 : token.count("\n")
      end

      token
    end

    private

    # Byte offsets of the start of each line, used to derive 1-based
    # [line, column] positions for tokens. Only computed when position
    # tracking is enabled (source_map).
    def compute_line_offsets
      @token_offsets = []
      @line_offsets  = [0]
      while @ss.skip_until(NEWLINE)
        @line_offsets << @ss.pos
      end
      @ss.reset
    end

    def compute_token_position
      token_start = @token_offsets[@offset]
      line = @line_offsets.bsearch_index { |line_start| line_start > token_start } || @line_offsets.length
      @token_position = [line, token_start - @line_offsets[line - 1] + 1]
    end

    def tokenize
      if @for_liquid_tag
        @tokens = @source.split("\n")
      else
        @tokens << shift_normal until @ss.eos?
      end

      @source = nil
      @ss = nil
    end

    def shift_normal
      token = next_token

      return unless token

      @token_offsets << @last_token_start if @positions

      token
    end

    def next_token
      # possible states: :text, :tag, :variable
      byte_a = @ss.peek_byte

      if byte_a == OPEN_CURLEY
        @ss.scan_byte

        byte_b = @ss.peek_byte

        if byte_b == PERCENTAGE
          @ss.scan_byte
          return next_tag_token
        elsif byte_b == OPEN_CURLEY
          @ss.scan_byte
          return next_variable_token
        end

        @ss.pos -= 1
      end

      next_text_token
    end

    def next_text_token
      start = @ss.pos
      @last_token_start = start

      unless @ss.skip_until(TAG_OR_VARIABLE_START)
        token = @ss.rest
        @ss.terminate
        return token
      end

      pos = @ss.pos -= 2
      @source.byteslice(start, pos - start)
    rescue ::ArgumentError => e
      if e.message == "invalid byte sequence in #{@ss.string.encoding}"
        raise SyntaxError, "Invalid byte sequence in #{@ss.string.encoding}"
      else
        raise
      end
    end

    def next_variable_token
      start = @ss.pos - 2
      @last_token_start = start

      byte_a = byte_b = @ss.scan_byte

      while byte_b
        byte_a = @ss.scan_byte while byte_a && byte_a != CLOSE_CURLEY && byte_a != OPEN_CURLEY

        break unless byte_a

        if @ss.eos?
          return byte_a == CLOSE_CURLEY ? @source.byteslice(start, @ss.pos - start) : "{{"
        end

        byte_b = @ss.scan_byte

        if byte_a == CLOSE_CURLEY
          if byte_b == CLOSE_CURLEY
            return @source.byteslice(start, @ss.pos - start)
          elsif byte_b != CLOSE_CURLEY
            @ss.pos -= 1
            return @source.byteslice(start, @ss.pos - start)
          end
        elsif byte_a == OPEN_CURLEY && byte_b == PERCENTAGE
          return next_tag_token_with_start(start)
        end

        byte_a = byte_b
      end

      "{{"
    end

    def next_tag_token
      start = @ss.pos - 2
      @last_token_start = start

      if (len = @ss.skip_until(TAG_END))
        @source.byteslice(start, len + 2)
      else
        "{%"
      end
    end

    def next_tag_token_with_start(start)
      @last_token_start = start
      @ss.skip_until(TAG_END)
      @source.byteslice(start, @ss.pos - start)
    end
  end
end
