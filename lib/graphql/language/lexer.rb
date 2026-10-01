# frozen_string_literal: true
module GraphQL
  module Language

    class Lexer
      def initialize(graphql_str, filename: nil, max_tokens: nil, diagnostic: false, errors: nil)
        if !(graphql_str.encoding == Encoding::UTF_8 || graphql_str.ascii_only?)
          graphql_str = graphql_str.dup.force_encoding(Encoding::UTF_8)
        end
        @string = graphql_str
        @filename = filename
        @diagnostic = diagnostic
        @errors = errors
        if !@string.valid_encoding?
          if @diagnostic
            # Keep the malformed bytes scannable (a raw byte never matches a
            # token pattern); the first token read reports the error once and
            # recovery continues byte by byte.
            graphql_str = graphql_str.dup.force_encoding(Encoding::BINARY)
            @string = graphql_str
            @bad_encoding = true
          else
            raise_parse_error("Parse error on bad Unicode escape sequence", nil, nil)
          end
        end
        @scanner = StringScanner.new(graphql_str)
        @pos = nil
        @max_tokens = max_tokens || Float::INFINITY
        @tokens_count = 0
        @finished = false
        @bracket_depth = 0
        @malformed_string_at = nil
        @malformed_string_value = nil
      end

      def finished?
        @finished
      end

      def freeze
        @scanner = nil
        super
      end

      attr_reader :pos, :tokens_count

      def advance
        if @bad_encoding && !@encoding_error_reported
          @encoding_error_reported = true
          record_parse_error("Parse error on bad Unicode escape sequence", nil, nil)
        end
        read_token
      end

      def read_token
        read_token!
      rescue GraphQL::ParseError => err
        raise unless @diagnostic
        @errors ||= []
        unless @errors.any? { |e| e.line == err.line && e.col == err.col }
          @errors << err
        end
        recover_from_lex_error
        if @scanner.eos?
          @finished = true
          false
        else
          read_token
        end
      end

      # The number of unmatched opening brackets (`{`, `(`, `[`) seen so far.
      # Only tracked in diagnostic mode; used by the parser to synchronize
      # after an error without mistaking brackets inside strings for boundaries.
      attr_reader :bracket_depth

      # Called by the parser's definition-level recovery when a new definition
      # keyword begins while brackets from a broken definition are still open:
      # the missing closer is attributed to that broken definition.
      def reset_bracket_depth_for_recovery
        @bracket_depth = 0
      end

      private

      def update_bracket_depth(token)
        return unless @diagnostic
        case token
        when :LCURLY, :LPAREN, :LBRACKET
          @bracket_depth += 1
        end
      end

      CLOSER_TOKENS = [:RCURLY, :RPAREN, :RBRACKET].freeze

      # After a token couldn't be read, move the scanner past the bad bytes so
      # that lexing (and parsing) can continue.
      def recover_from_lex_error
        rest = @scanner.rest
        case rest.getbyte(0)
        when 45 # `-`
          @scanner.scan(/-\s?[a-z0-9]*/i)
          @scanner.pos += 1 if @scanner.matched_size == 0
        when 46 # `.`
          @scanner.pos += 1
        else
          @scanner.pos += 1
        end
      end

      def record_parse_error(message, line, col)
        err = GraphQL::ParseError.new(message, line, col, @string, filename: @filename)
        @errors ||= []
        @errors << err unless @errors.any? { |e| e.line == err.line && e.col == err.col }
        err
      end

      def utf8_char_length(lead_byte)
        if lead_byte >= 0xF0
          4
        elsif lead_byte >= 0xE0
          3
        elsif lead_byte >= 0xC0
          2
        else
          1
        end
      end

      public

      def read_token!
        @manual_token_value = nil
        loop do
          @scanner.skip(IGNORE_REGEXP)
          if @scanner.skip(COMMENT_REGEXP)
            @tokens_count += 1
            next
          end
          break
        end

        if @scanner.eos?
          @finished = true
          return false
        end
        @tokens_count += 1
        if @tokens_count > @max_tokens
          raise_parse_error("This query is too large to execute.")
        end
        @pos = @scanner.pos
        next_byte = @string.getbyte(@pos)
        next_byte_is_for = FIRST_BYTES[next_byte]
        # Track nesting from the real byte at the scanner position (the
        # `next_byte` lookup above is shifted for historical reasons).
        current_punct = PUNCTUATION_NAME_FOR_BYTE[@string.getbyte(@scanner.pos)]
        # A closer is counted against the depth *before* it is consumed, so a
        # well-formed closing token reads at the depth of the bracket it shuts.
        if @diagnostic && CLOSER_TOKENS.include?(current_punct) && @bracket_depth > 0
          @bracket_depth -= 1
        end
        case next_byte_is_for
        when ByteFor::PUNCTUATION
          @scanner.pos += 1
          punct_name = PUNCTUATION_NAME_FOR_BYTE[next_byte]
          update_bracket_depth(punct_name)
          punct_name
        when ByteFor::NAME
          if len = @scanner.skip(KEYWORD_REGEXP)
            case len
            when 2
              :ON
            when 12
              :SUBSCRIPTION
            else
              pos = @pos

              # Use bytes 2 and 3 as a unique identifier for this keyword
              bytes = (@string.getbyte(pos + 2) << 8) | @string.getbyte(pos + 1)
              KEYWORD_BY_TWO_BYTES[_hash(bytes)]
            end
          else
            @scanner.skip(IDENTIFIER_REGEXP)
            :IDENTIFIER
          end
        when ByteFor::IDENTIFIER
          @scanner.skip(IDENTIFIER_REGEXP)
          :IDENTIFIER
        when ByteFor::NUMBER
          if len = @scanner.skip(NUMERIC_REGEXP)

            if GraphQL.reject_numbers_followed_by_names
              new_pos = @scanner.pos
              peek_byte = @string.getbyte(new_pos)
              next_first_byte = FIRST_BYTES[peek_byte]
              if next_first_byte == ByteFor::NAME || next_first_byte == ByteFor::IDENTIFIER
                number_part = token_value
                name_part = @scanner.scan(IDENTIFIER_REGEXP)
                raise_parse_error("Name after number is not allowed (in `#{number_part}#{name_part}`)")
              end
            end
            # Check for a matched decimal:
            @scanner[1] ? :FLOAT : :INT
          else
            # Attempt to find the part after the `-`
            value = @scanner.scan(/-\s?[a-z0-9]*/i)
            invalid_byte_for_number_error_message = "Expected type 'number', but it was malformed#{value.nil? ? "" : ": #{value.inspect}"}."
            raise_parse_error(invalid_byte_for_number_error_message)
          end
        when ByteFor::ELLIPSIS
          if @string.getbyte(@pos + 1) != 46 || @string.getbyte(@pos + 2) != 46
            raise_parse_error("Expected `...`, actual: #{@string[@pos..@pos + 2].inspect}")
          end
          @scanner.pos += 3
          :ELLIPSIS
        when ByteFor::STRING
          if !(@scanner.skip(BLOCK_STRING_REGEXP) || @scanner.skip(QUOTED_STRING_REGEXP))
            if @diagnostic
              # Recover as if the malformed string literal were a complete
              # STRING token so that the enclosing argument/list can keep
              # being parsed. A quote with no closing quote swallows the rest
              # of the source (newlines are illegal inside ordinary strings),
              # since none of the following bytes can reliably be tokenized.
              start_pos = @pos
              @scanner.pos = @string.bytesize
              malformed = @string.byteslice(start_pos, @scanner.pos - start_pos)
              @malformed_string_at = @scanner.pos
              @malformed_string_value = malformed
              record_parse_error("Expected string or block string, but it was malformed", line_number, column_number)
              @finished = true
              return :STRING
            end
            raise_parse_error("Expected string or block string, but it was malformed")
          end
          :STRING
        else
          current_byte = @string.getbyte(@scanner.pos)
          current_byte_kind = FIRST_BYTES[current_byte]
          if @diagnostic
            # Non-ASCII bytes (e.g. CJK) fall through the FIRST_BYTES table.
            # Treat a non-ASCII letter as the start of an identifier so that
            # non-ASCII queries (e.g. Chinese field names) tokenize instead of
            # producing one UNKNOWN_CHAR diagnostic per byte.
            char_length = current_byte >= 0xF0 ? 4 : current_byte >= 0xE0 ? 3 : 2
            char = @string.byteslice(@scanner.pos, char_length)
            if current_byte && current_byte >= 0x80 && char && char.valid_encoding? &&
                (current_byte_kind.nil? || next_byte_is_for == ByteFor::PUNCTUATION)
              matched = @scanner.scan(UNICODE_IDENTIFIER_REGEXP)
              if matched.nil?
                # The scanner may be running against a binary string even
                # though the leading character is valid UTF-8. Consume an
                # ASCII-identifier-compatible run together with complete
                # multibyte characters manually.
                manual_start = @scanner.pos
                len = 0
                rest = @scanner.rest
                while len < rest.bytesize
                  byte = rest.getbyte(len)
                  if byte < 128
                    is_ascii_word =
                      (byte >= 97 && byte <= 122) || # a-z
                      (byte >= 65 && byte <= 90) ||  # A-Z
                      (byte >= 48 && byte <= 57) ||  # 0-9
                      byte == 95                     # _
                    break unless is_ascii_word
                    len += 1
                  else
                    char_len = utf8_char_length(byte)
                    slice = rest.byteslice(len, char_len)
                    break unless slice&.valid_encoding?
                    len += char_len
                  end
                end
                @scanner.pos += [len, 1].max
                @manual_token_value = @string.byteslice(manual_start, @scanner.pos - manual_start)
              end
              return :IDENTIFIER
            end
            @scanner.pos += 1
          else
            @scanner.pos += 1
          end
          :UNKNOWN_CHAR
        end
      end

      def token_value
        return @manual_token_value if @manual_token_value
        matched = @scanner.matched_size
        return @malformed_string_value if matched.nil? && @malformed_string_value
        @string.byteslice(@scanner.pos - matched, matched)
      rescue StandardError => err
        raise GraphQL::Error, "(token_value failed: #{err.class}: #{err.message})"
      end

      # In diagnostic mode, a malformed string literal is consumed as a STRING
      # token anyway. Its raw text is returned here so the parser doesn't
      # interpret the following tokens as a continuation of the broken argument.
      def consume_malformed_string_value
        if @malformed_string_at && @malformed_string_at == @scanner.pos
          @malformed_string_at = nil
          @malformed_string_value
        end
      end

      def debug_token_value(token_name)
        if token_name && Lexer::Punctuation.const_defined?(token_name)
          Lexer::Punctuation.const_get(token_name)
        elsif token_name == :ELLIPSIS
          "..."
        elsif token_name == :STRING
          string_value
        elsif @scanner.matched_size.nil?
          @string.byteslice(@scanner.pos - 1, 1)
        else
          token_value
        end
      end

      ESCAPES = /\\["\\\/bfnrt]/
      ESCAPES_REPLACE = {
        '\\"' => '"',
        "\\\\" => "\\",
        "\\/" => '/',
        "\\b" => "\b",
        "\\f" => "\f",
        "\\n" => "\n",
        "\\r" => "\r",
        "\\t" => "\t",
      }
      UTF_8 = /\\u(?:([\da-f]{4})|\{([\da-f]+)\})(?:\\u([\da-f]{4}))?/i
      VALID_STRING = /\A(?:[^\\]|#{ESCAPES}|#{UTF_8})*\z/o
      ESCAPED = /(?:#{ESCAPES}|#{UTF_8})/o

      def string_value
        str = token_value
        is_block = str.start_with?('"""')
        if is_block
          str.gsub!(/\A"""|"""\z/, '')
          return Language::BlockString.trim_whitespace(str)
        else
          str.gsub!(/\A"|"\z/, '')

          if !str.valid_encoding? || !str.match?(VALID_STRING)
            raise_parse_error("Bad unicode escape in #{str.inspect}")
          else
            begin
              Lexer.replace_escaped_characters_in_place(str)
            rescue RangeError
              raise_parse_error("Bad unicode escape in #{str.inspect}")
            end

            if !str.valid_encoding?
              raise_parse_error("Bad unicode escape in #{str.inspect}")
            else
              str
            end
          end
        end
      end

      def line_number
        @scanner.string.byteslice(0, @pos).b.count("\n".b) + 1
      end

      def column_number
        line_prefix = @scanner.string.byteslice(0, @pos)
        line_prefix = line_prefix.b unless line_prefix.valid_encoding?
        newline_index = line_prefix.rindex("\n")
        newline_index ? line_prefix.length - newline_index : line_prefix.length + 1
      end

      def raise_parse_error(message, line = line_number, col = column_number)
        err = GraphQL::ParseError.new(message, line, col, @string, filename: @filename)
        if @diagnostic
          @errors ||= []
          @errors << err unless @errors.any? { |e| e.line == line && e.col == col }
        end
        raise err
      end

      IGNORE_REGEXP = /[, \c\r\n\t]+/
      COMMENT_REGEXP = /\#[^\n]*/
      IDENTIFIER_REGEXP = /[_A-Za-z][_0-9A-Za-z]*/
      # Diagnostic-mode only: tolerate non-ASCII letters (e.g. CJK) in names so
      # malformed queries containing them produce structural diagnostics
      # instead of one UNKNOWN_CHAR per byte.
      UNICODE_IDENTIFIER_REGEXP = /[_\p{L}][_\p{L}\p{N}]*/
      INT_REGEXP =        /-?(?:[0]|[1-9][0-9]*)/
      FLOAT_DECIMAL_REGEXP = /[.][0-9]+/
      FLOAT_EXP_REGEXP =     /[eE][+-]?[0-9]+/
      # TODO: FLOAT_EXP_REGEXP should not be allowed to follow INT_REGEXP, integers are not allowed to have exponent parts.
      NUMERIC_REGEXP =  /#{INT_REGEXP}(#{FLOAT_DECIMAL_REGEXP}#{FLOAT_EXP_REGEXP}|#{FLOAT_DECIMAL_REGEXP}|#{FLOAT_EXP_REGEXP})?/

      KEYWORDS = [
        "on",
        "fragment",
        "true",
        "false",
        "null",
        "query",
        "mutation",
        "subscription",
        "schema",
        "scalar",
        "type",
        "extend",
        "implements",
        "interface",
        "union",
        "enum",
        "input",
        "directive",
        "repeatable"
      ].freeze

      KEYWORD_REGEXP = /#{Regexp.union(KEYWORDS.sort)}\b/
      KEYWORD_BY_TWO_BYTES = [
        :INTERFACE,
        :MUTATION,
        :EXTEND,
        :FALSE,
        :ENUM,
        :TRUE,
        :NULL,
        nil,
        nil,
        nil,
        nil,
        nil,
        nil,
        nil,
        :QUERY,
        nil,
        nil,
        :REPEATABLE,
        :IMPLEMENTS,
        :INPUT,
        :TYPE,
        :SCHEMA,
        nil,
        nil,
        nil,
        :DIRECTIVE,
        :UNION,
        nil,
        nil,
        :SCALAR,
        nil,
        :FRAGMENT
      ].freeze

      # This produces a unique integer for bytes 2 and 3 of each keyword string
      # See https://tenderlovemaking.com/2023/09/02/fast-tokenizers-with-stringscanner.html
      def _hash key
        (key * 18592990) >> 27 & 0x1f
      end

      module Punctuation
        LCURLY =        '{'
        RCURLY =        '}'
        LPAREN =        '('
        RPAREN =        ')'
        LBRACKET =      '['
        RBRACKET =      ']'
        COLON =         ':'
        VAR_SIGN =      '$'
        DIR_SIGN =      '@'
        EQUALS =        '='
        BANG =          '!'
        PIPE =          '|'
        AMP =           '&'
      end

      # A sparse array mapping the bytes for each punctuation
      # to a symbol name for that punctuation
      PUNCTUATION_NAME_FOR_BYTE = Punctuation.constants.each_with_object([]) { |name, arr|
        punct = Punctuation.const_get(name)
        arr[punct.ord] = name
      }.freeze


      QUOTE =         '"'
      UNICODE_DIGIT = /[0-9A-Za-z]/
      FOUR_DIGIT_UNICODE = /#{UNICODE_DIGIT}{4}/
      N_DIGIT_UNICODE = %r{#{Punctuation::LCURLY}#{UNICODE_DIGIT}+#{Punctuation::RCURLY}}x
      UNICODE_ESCAPE = %r{\\u(?:#{FOUR_DIGIT_UNICODE}|#{N_DIGIT_UNICODE})}
      STRING_ESCAPE = %r{[\\][\\/bfnrt]}
      BLOCK_QUOTE =   '"""'
      ESCAPED_QUOTE = /\\"/;
      STRING_CHAR = /#{ESCAPED_QUOTE}|[^"\\\n\r]|#{UNICODE_ESCAPE}|#{STRING_ESCAPE}/
      QUOTED_STRING_REGEXP = %r{#{QUOTE} (?:#{STRING_CHAR})* #{QUOTE}}x
      BLOCK_STRING_REGEXP = %r{
        #{BLOCK_QUOTE}
        (?: [^"\\]               |  # Any characters that aren't a quote or slash
           (?<!") ["]{1,2} (?!") |  # Any quotes that don't have quotes next to them
           \\"{0,3}(?!")         |  # A slash followed by <= 3 quotes that aren't followed by a quote
           \\                    |  # A slash
           "{1,2}(?!")              # 1 or 2 " followed by something that isn't a quote
        )*
        (?:"")?
        #{BLOCK_QUOTE}
      }xm

      # Use this array to check, for a given byte that will start a token,
      # what kind of token might it start?
      FIRST_BYTES = Array.new(255)

      module ByteFor
        NUMBER = 0 # int or float
        NAME = 1 # identifier or keyword
        STRING = 2
        ELLIPSIS = 3
        IDENTIFIER = 4 # identifier, *not* a keyword
        PUNCTUATION = 5
      end

      (0..9).each { |i| FIRST_BYTES[i.to_s.ord] = ByteFor::NUMBER }
      FIRST_BYTES["-".ord] = ByteFor::NUMBER
      # Some of these may be overwritten below, if keywords start with the same character
      ("A".."Z").each { |char| FIRST_BYTES[char.ord] = ByteFor::IDENTIFIER }
      ("a".."z").each { |char| FIRST_BYTES[char.ord] = ByteFor::IDENTIFIER }
      FIRST_BYTES['_'.ord] = ByteFor::IDENTIFIER
      FIRST_BYTES['.'.ord] = ByteFor::ELLIPSIS
      FIRST_BYTES['"'.ord] = ByteFor::STRING
      KEYWORDS.each { |kw| FIRST_BYTES[kw.getbyte(0)] = ByteFor::NAME }
      Punctuation.constants.each do |punct_name|
        punct = Punctuation.const_get(punct_name)
        FIRST_BYTES[punct.ord] = ByteFor::PUNCTUATION
      end
      FIRST_BYTES.freeze


      # Replace any escaped unicode or whitespace with the _actual_ characters
      # To avoid allocating more strings, this modifies the string passed into it
      def self.replace_escaped_characters_in_place(raw_string)
        raw_string.gsub!(ESCAPED) do |matched_str|
          if (point_str_1 = $1 || $2)
            codepoint_1 = point_str_1.to_i(16)
            if (codepoint_2 = $3)
              codepoint_2 = codepoint_2.to_i(16)
              if (codepoint_1 >= 0xD800 && codepoint_1 <= 0xDBFF) && # leading surrogate
                  (codepoint_2 >= 0xDC00 && codepoint_2 <= 0xDFFF) # trailing surrogate
                # A surrogate pair
                combined = ((codepoint_1 - 0xD800) * 0x400) + (codepoint_2 - 0xDC00) + 0x10000
                [combined].pack('U'.freeze)
              else
                # Two separate code points
                [codepoint_1].pack('U'.freeze) + [codepoint_2].pack('U'.freeze)
              end
            else
              [codepoint_1].pack('U'.freeze)
            end
          else
            ESCAPES_REPLACE[matched_str]
          end
        end
        nil
      end

      # This is not used during parsing because the parser
      # doesn't actually need tokens.
      def self.tokenize(string)
        lexer = GraphQL::Language::Lexer.new(string)
        tokens = []
        while (token_name = lexer.advance)
          new_token = [
            token_name,
            lexer.line_number,
            lexer.column_number,
            lexer.debug_token_value(token_name),
          ]
          tokens << new_token
        end
        tokens
      end
    end
  end
end
