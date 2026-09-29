require 'dentaku/token'
require 'dentaku/token_matcher'
require 'dentaku/token_scanner'
require 'dentaku/source_map'

module Dentaku
  class Tokenizer
    attr_reader :aliases

    LPAREN = TokenMatcher.new(:grouping, :open)
    RPAREN = TokenMatcher.new(:grouping, :close)

    def tokenize(string, options = {})
      @nesting = 0
      @tokens  = []
      @offset  = 0
      @open_locations = []
      @source_maps = []
      @aliases = options.fetch(:aliases, global_aliases)
      input    = string.to_s.dup
      input    = strip_comments(input)
      input    = replace_aliases(input)

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
          fail! :parse_error, at: input, location: locate(@offset, @offset + 1)
        end
      end

      fail! :too_many_opening_parentheses, location: @open_locations.last if @nesting > 0

      @tokens
    end

    def last_token
      @tokens.last
    end

    def scan(string, scanner)
      if tokens = scanner.scan(string, last_token)
        position = @offset

        tokens.each do |token|
          token.location = locate(position, position + token.length)
          position += token.length

          if token.empty?
            fail! :unexpected_zero_width_match,
                  token_category: token.category, at: string,
                  location: token.location
          end

          @nesting += 1 if LPAREN == token
          @nesting -= 1 if RPAREN == token
          @open_locations << token.location if LPAREN == token
          @open_locations.pop if RPAREN == token && @nesting >= 0
          fail! :too_many_closing_parentheses, location: token.location if @nesting < 0

          @tokens << token unless token.is?(:whitespace)
        end

        match_length = tokens.map(&:length).reduce(:+)
        @offset += match_length
        [true, string[match_length..-1]]
      else
        [false, string]
      end
    end

    def strip_comments(input)
      rewritten, map = SourceMap.rewrite(input, /\/\*[^*]*\*+(?:[^*\/][^*]*\*+)*\//) { "" }
      (@source_maps ||= []).unshift(map)
      rewritten
    end

    def replace_aliases(string)
      return string unless @aliases.any?

      rewritten, map = SourceMap.rewrite(string, alias_regex) do |match|
        match_regex = /^#{Regexp.escape(match)}$/i

        @aliases.detect do |(_key, aliases)|
          !aliases.grep(match_regex).empty?
        end.first
      end

      (@source_maps ||= []).unshift(map)
      rewritten
    end

    def alias_regex
      values = @aliases.values.flatten.join('|')
      /(?<=\p{Punct}|[[:space:]]|\A)(#{values})(?=\s*\()/i
    end

    private

    # translate a character range in the rewritten input back to the
    # original formula the user wrote
    def locate(start_offset, end_offset)
      start_pos = @source_maps.inject(start_offset) { |pos, map| map.map_begin(pos) }
      end_pos   = @source_maps.inject(end_offset)   { |pos, map| map.map_end(pos) }
      start_pos...end_pos
    end

    def global_aliases
      return {} unless Dentaku.respond_to?(:aliases)
      Dentaku.aliases
    end

    def fail!(reason, **meta)
      error = TokenizerError.for(reason, **meta)
      error.location ||= meta[:location]
      raise error
    end
  end
end
