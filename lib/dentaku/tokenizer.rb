require 'dentaku/token'
require 'dentaku/token_matcher'
require 'dentaku/token_scanner'
require 'dentaku/offset_map'
require 'dentaku/source_range'

module Dentaku
  class Tokenizer
    attr_reader :aliases

    LPAREN = TokenMatcher.new(:grouping, :open)
    RPAREN = TokenMatcher.new(:grouping, :close)

    def tokenize(string, options = {})
      @nesting = 0
      @tokens  = []
      @offset  = 0
      @aliases = options.fetch(:aliases, global_aliases)
      input    = strip_comments(string.to_s.dup)
      input, @offset_map = replace_aliases_with_map(input)

      scanner_options = {
        case_sensitive: options.fetch(:case_sensitive, false),
        raw_date_literals: options.fetch(:raw_date_literals, true)
      }

      until input.empty?
        scanned = TokenScanner.scanners(scanner_options).any? do |scanner|
          scanned, input = scan(input, scanner)
          scanned
        end

        unless scanned
          fail! :parse_error, at: input, range: source_range_for(@offset, @offset + 1)
        end
      end

      fail! :too_many_opening_parentheses, range: last_token && last_token.source_range if @nesting > 0

      @tokens
    end

    def last_token
      @tokens.last
    end

    def scan(string, scanner)
      if tokens = scanner.scan(string, last_token)
        position = @offset

        tokens.each do |token|
          if token.empty?
            fail! :unexpected_zero_width_match,
                  token_category: token.category, at: string,
                  range: source_range_for(position, position)
          end

          token.source_range = source_range_for(position, position + token.length)
          position += token.length

          @nesting += 1 if LPAREN == token
          @nesting -= 1 if RPAREN == token
          fail! :too_many_closing_parentheses, range: token.source_range if @nesting < 0

          @tokens << token unless token.is?(:whitespace)
        end

        match_length = tokens.map(&:length).reduce(:+)
        @offset += match_length
        [true, string[match_length..-1]]
      else
        [false, string]
      end
    end

    # comments are blanked out rather than removed so that every character
    # after them keeps its original offset (newlines are preserved to keep
    # line numbers stable in multi-line formulas)
    def strip_comments(input)
      input.gsub(/\/\*[^*]*\*+(?:[^*\/][^*]*\*+)*\//) do |comment|
        comment.gsub(/[^\r\n]/, ' ')
      end
    end

    def replace_aliases(string)
      replace_aliases_with_map(string).first
    end

    # like replace_aliases, but also returns an OffsetMap that translates
    # offsets in the expanded string back to the original input
    def replace_aliases_with_map(string)
      return [string, OffsetMap.identity] unless @aliases.any?

      replacements = []

      result = string.gsub(alias_regex) do |match|
        match_start = Regexp.last_match.begin(0)
        match_regex = /^#{Regexp.escape(match)}$/i

        replacement = @aliases.detect do |(_key, aliases)|
          !aliases.grep(match_regex).empty?
        end.first

        replacements << [match_start, match.length, replacement.length]
        replacement
      end

      [result, OffsetMap.new(replacements)]
    end

    def alias_regex
      values = @aliases.values.flatten.join('|')
      /(?<=\p{Punct}|[[:space:]]|\A)(#{values})(?=\s*\()/i
    end

    private

    def global_aliases
      return {} unless Dentaku.respond_to?(:aliases)
      Dentaku.aliases
    end

    def source_range_for(start_offset, end_offset)
      SourceRange.new(
        @offset_map.translate(start_offset),
        @offset_map.translate(end_offset)
      )
    end

    def fail!(reason, **meta)
      range = meta.delete(:range)
      error = TokenizerError.for(reason, **meta)
      error.attach_source(range) if range
      raise error
    end
  end
end
