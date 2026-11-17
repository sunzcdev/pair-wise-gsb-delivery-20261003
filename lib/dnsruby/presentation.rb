# frozen_string_literal: true

module Dnsruby
  # Shared lexical handling for RR strings and zone-file continuation lines.
  # Character-string escapes remain intact for the record-specific parser.
  module Presentation #:nodoc: all
    # Header tokens preserve escaped whitespace and punctuation in owners.
    def self.tokens(text)
      text.scan(/(?:\\.|[^\s])+/m)
    end

    # Decimal escapes keep escaped whitespace intact through older record
    # parsers and layout normalization, without changing the represented octet.
    def self.escape_whitespace(text)
      text.gsub(/\\(.)/m) do |escaped|
        /\s/.match?(escaped[1]) ? format('\\%03d', escaped[1].ord) : escaped
      end
    end

    # TXT.parse historically accepts C-style escapes. Zone files instead use
    # RFC 1035's literal nonnumeric escapes; express them as octets before
    # handing the string to that compatibility parser.
    def self.character_string_escapes(text)
      text.gsub(/\\([^0-9])/m) do |escaped|
        escaped[1].bytes.map { |octet| format('\\%03d', octet) }.join
      end
    end

    def self.unescaped_quote(text)
      position = 0
      while (token = /\\.|"/m.match(text, position))
        return token.begin(0) if token[0] == '"'
        position = token.end(0)
      end
      nil
    end

    # TXT (and its SPF subclass) historically also accept single quotes. Do
    # not extend that convention to domain names or other RDATA formats, where
    # an apostrophe is an ordinary character. Return the end of the type token
    # so that even a leading apostrophe in the owner cannot open a TXT string.
    def self.single_quoted_start(text, owner_omitted: false)
      # Grouping parentheses can touch the type (TXT('value')), while an
      # escaped parenthesis in the owner remains part of that owner's token.
      fields = []
      position = 0
      while fields.length < 4 && (field = /\G[\s()]*((?:\\.|[^\s()])+)/m.match(text, position))
        fields << [field[1], field.end(0)]
        position = field.end(0)
      end
      fields.shift unless owner_omitted
      # Numeric classes learned from received records must not make parsing
      # unrelated records rebuild an ever-growing regular expression.
      ttl_seen = class_seen = false
      2.times do
        token = fields.first&.first
        if token && !ttl_seen && /\A\d/.match?(token)
          ttl_seen = true
        elsif token && !class_seen && (Classes.to_code(token) || /\ACLASS\d+\z/i.match?(token))
          class_seen = true
        else
          break
        end
        fields.shift
      end
      type_token = fields.first&.first
      type_code = Types.to_code(type_token) if type_token
      if !type_code && (numeric = /\ATYPE(\d+)\z/i.match(type_token.to_s))
        type_code = numeric[1].to_i
      end
      fields.first.last if [Types::TXT, Types::SPF].include?(type_code)
    end

    def self.scan(text, depth = 0, quoted = false, allow_unbalanced: false,
                  single_quoted_from: nil)
      # Most zone records have no lexical syntax to interpret. Preserve the
      # current continuation depth and only fold physical line endings.
      if !quoted && !/[\\'"();]/.match?(text)
        return [text.tr("\r\n", '  '), depth, false]
      end

      result = +''
      quoted = '"' if quoted == true
      just_closed_quote = false
      position = 0

      # Copy ordinary text in runs. Quoted TXT values often contain hundreds
      # of characters but only two delimiters; allocating one String for each
      # character makes that common case unnecessarily expensive. Regexp and
      # String offsets both count characters, including for Unicode owners.
      while (syntax = /[\\'"();\r\n]/.match(text, position))
        index = syntax.begin(0)
        if index > position
          result << text[position...index]
          just_closed_quote = false
        end
        char = syntax[0]
        position = index + 1
        after_quote = just_closed_quote
        just_closed_quote = false

        if char == '\\'
          result << char
          if (escaped = text[position])
            result << escaped
            position += 1
          end
        elsif char == '"' || (single_quoted_from && index >= single_quoted_from && char == "'" &&
              (quoted == "'" || (!quoted && (after_quote || index.zero? || /[\s(]/.match?(text[index - 1])))))
          result << char
          if quoted == char
            quoted = false
            just_closed_quote = true
          elsif !quoted
            quoted = char
          end
        elsif !quoted && char == ';'
          # Comments end only at LF, matching the zone reader's physical
          # line handling. Leave that newline for the ordinary layout branch.
          position = text.index("\n", position) || text.length
        elsif !quoted && (char == '(' || char == ')')
          depth += char == '(' ? 1 : -1
          if depth.negative? && !allow_unbalanced
            raise DecodeError.new('Unmatched closing parenthesis in record')
          end
          # Parentheses separate tokens when neither neighbor is whitespace.
          # Preserve existing spacing otherwise; some older RR parsers use
          # offsets into the original RDATA rather than whitespace tokens.
          if !result.empty? && /\S/.match?(result[-1]) &&
             text[index + 1] && /\S/.match?(text[index + 1])
            result << ' '
          end
        elsif !quoted && (char == "\n" || char == "\r")
          # A physical line break outside a quoted value is layout, not an
          # octet in a TXT string or part of an RFC3597 hexadecimal value.
          result << ' '
        else
          result << char
        end
      end
      result << text[position..-1] if position < text.length

      [result, depth, quoted]
    end
  end
  private_constant :Presentation
end
