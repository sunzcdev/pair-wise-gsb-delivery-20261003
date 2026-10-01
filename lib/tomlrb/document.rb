# frozen-string-literal: true

module Tomlrb
  # A position-aware TOML document supporting structure-preserving edits.
  #
  # Unlike Tomlrb.parse, which returns a plain Hash and loses the original
  # layout, Tomlrb::Document keeps the source text and applies edits as
  # surgical byte-range replacements. Comments, whitespace, quoting style and
  # number formats of untouched regions are preserved byte-for-byte.
  #
  #   doc = Tomlrb::Document.load_file('config.toml')
  #   doc.set('database.pool', 10)
  #   doc.rename('database.url', 'dsn')
  #   doc.move('server.timeout', 'database')
  #   doc.set(['servers', 1, 'weight'], 2500) # 2nd [[servers]] instance
  #   File.write('config.toml', doc.to_s)
  #
  # Every edit is validated against a full re-parse of the resulting document.
  # Edits that would introduce duplicate declarations or type conflicts raise
  # Tomlrb::Document::EditError and leave the document unchanged.
  class Document
    class EditError < StandardError; end

    # Wraps a TOML fragment to be inserted verbatim, e.g. to control the exact
    # textual representation of a value: set('server.port', Raw.new('0x1F91'))
    class Raw
      attr_reader :text

      def initialize(text)
        @text = text
      end
    end

    KeyToken = Struct.new(:value, :start, :stop)
    PairInfo = Struct.new(:path, :context, :key_tokens, :value_start, :value_stop, :line_start, :line_stop)
    TableInfo = Struct.new(:path, :array, :header_stop)

    BARE_KEY = /\A[A-Za-z0-9_-]+\z/.freeze
    STRING_ESCAPES = {
      '\\' => '\\\\', '"' => '\\"', "\n" => '\\n', "\t" => '\\t',
      "\r" => '\\r', "\b" => '\\b', "\f" => '\\f'
    }.freeze

    TAB = 0x09
    LF = 0x0A
    CR = 0x0D
    SPACE = 0x20
    QUOTE = 0x22
    APOS = 0x27
    HASH = 0x23
    DOT = 0x2E
    EQUALS = 0x3D
    BACKSLASH = 0x5C
    LBRACKET = 0x5B
    RBRACKET = 0x5D
    LBRACE = 0x7B
    RBRACE = 0x7D

    def self.parse(text)
      new(text)
    end

    def self.load_file(path)
      new(File.read(path, encoding: 'utf-8'))
    end

    def initialize(text)
      @text = text.dup
      rebuild!
    end

    # @return [String] the current document text
    def to_s
      @text
    end

    # @return [Hash] the document parsed into a Ruby data structure
    def to_h
      @hash
    end

    # Replaces the value at the given path, leaving the key, surrounding
    # whitespace and any trailing comment untouched.
    #
    # @param path [String, Array] e.g. 'database.pool', 'servers[1].weight'
    #   or ['servers', 1, 'weight']
    # @param value the new value. Strings, numbers, booleans, arrays, hashes
    #   (serialized as inline tables), Time and Tomlrb date/time objects are
    #   supported. Wrap a string in Raw to insert TOML verbatim.
    def set(path, value)
      pair = find_pair!(normalize_path(path))
      replace_range(pair.value_start...pair.value_stop, serialize_value(value))
    end

    # Renames the last key segment at the given path, preserving its position,
    # value and any comment.
    def rename(path, new_name)
      segments = normalize_path(path)
      pair = find_pair!(segments)
      new_name = new_name.to_s
      ensure_absent!(segments[0..-2] + [new_name]) unless segments.last == new_name
      token = pair.key_tokens.last
      replace_range(token.start...token.stop, serialize_key(new_name))
    end

    # Moves a key-value pair (including dotted keys and multi-line values)
    # into another table. The pair is inserted directly after the target
    # table's header so the table-header scope of neighbouring keys is never
    # disturbed. If the target table does not exist, a new [table] section is
    # appended at the end of the document.
    #
    # @param from_path [String, Array] path of the pair to move
    # @param to_table_path [String, Array] path of the destination table; an
    #   integer segment addresses a specific [[array]] instance
    # @param as [String, nil] optional new name for the moved key
    def move(from_path, to_table_path, as: nil)
      from = normalize_path(from_path)
      to = normalize_path(to_table_path)
      pair = find_pair!(from)

      key_segments = pair.key_tokens.map(&:value)
      moved = @text.byteslice(pair.line_start...pair.line_stop)
      if as
        token = pair.key_tokens.last
        rel_start = token.start - pair.line_start
        rel_stop = token.stop - pair.line_start
        moved = moved.byteslice(0...rel_start) + serialize_key(as.to_s) + moved.byteslice(rel_stop..-1).to_s
        key_segments = key_segments[0..-2] + [as.to_s]
      end
      moved += "\n" unless moved.end_with?("\n")

      destination = to + key_segments
      ensure_absent!(destination) unless destination == pair.path

      remaining = @text.byteslice(0...pair.line_start) + @text.byteslice(pair.line_stop..-1).to_s

      table = find_table(to)
      new_text =
        if table
          insert_at = table.header_stop
          insert_at -= pair.line_stop - pair.line_start if pair.line_start < insert_at
          remaining.byteslice(0...insert_at) + moved + remaining.byteslice(insert_at..-1).to_s
        else
          ensure_creatable_table!(to)
          header = to.map { |segment| serialize_key(segment) }.join('.')
          separator = if remaining.empty? || remaining.end_with?("\n\n")
                        ''
                      elsif remaining.end_with?("\n")
                        "\n"
                      else
                        "\n\n"
                      end
          remaining + separator + "[#{header}]\n" + moved
        end
      commit!(new_text)
    end

    private

    def rebuild!
      @hash = Tomlrb.parse(@text)
      @pairs = []
      @tables = []
      scan_document
    end

    def commit!(new_text)
      Tomlrb.parse(new_text) # validation only; raises ParseError on conflicts
      @text = new_text
      rebuild!
      self
    rescue Tomlrb::ParseError => e
      raise EditError, "edit rejected, document left unchanged: #{e.message}"
    end

    def replace_range(range, replacement)
      commit!(@text.byteslice(0...range.begin) + replacement + @text.byteslice(range.end..-1).to_s)
    end

    # --- structural lookups -------------------------------------------------

    def find_pair!(segments)
      pair = @pairs.find { |p| p.path == segments }
      raise EditError, "no key found at #{format_path(segments)}" unless pair

      pair
    end

    def find_table(segments)
      @tables.find { |t| t.path == segments }
    end

    # Walks the parsed structure along segments.
    # Returns [:missing], [:value, object] or [:conflict, index] where index
    # is the first segment that hits a non-table value.
    def semantic_lookup(segments)
      current = @hash
      segments.each_with_index do |segment, index|
        case current
        when Hash
          return [:missing] unless current.key?(segment)

          current = current[segment]
        when Array
          return [:missing] unless segment.is_a?(Integer) && segment >= 0 && segment < current.length

          current = current[segment]
        else
          return [:conflict, index]
        end
      end
      [:value, current]
    end

    def ensure_absent!(segments)
      status, = semantic_lookup(segments)
      case status
      when :value
        raise EditError, "key #{format_path(segments)} is already defined"
      when :conflict
        raise EditError, "key #{format_path(segments)} conflicts with a non-table value"
      end
    end

    def ensure_creatable_table!(segments)
      if segments.any? { |segment| segment.is_a?(Integer) }
        raise EditError, "table #{format_path(segments)} does not exist"
      end

      status, value, = semantic_lookup(segments)
      return if status == :missing
      if status == :conflict || !value.is_a?(Hash)
        raise EditError, "cannot create table #{format_path(segments)}: it conflicts with an existing value"
      end

      # A Hash without a header can only be extended with a new [table]
      # section when it was implied by sub-table headers. Tables created by
      # dotted keys or inline tables cannot be redefined.
      dotted = @pairs.any? do |pair|
        strict_prefix?(pair.context, segments) && pair.path[0, segments.length] == segments
      end
      if dotted
        raise EditError, "cannot move into #{format_path(segments)}: it is defined by an inline table or dotted key"
      end
    end

    def strict_prefix?(prefix, segments)
      prefix.length < segments.length && segments[0, prefix.length] == prefix
    end

    # --- path handling ------------------------------------------------------

    def normalize_path(path)
      segments =
        case path
        when Array
          path.map { |segment| segment.is_a?(Integer) ? segment : segment.to_s }
        else
          parse_string_path(path.to_s)
        end
      if segments.empty? || segments.any? { |segment| !segment.is_a?(Integer) && segment.empty? }
        raise EditError, "invalid path #{path.inspect}"
      end

      segments
    end

    # Parses paths such as: servers[1].host, "quoted.key".x, 'literal'.y
    def parse_string_path(path)
      segments = []
      index = 0
      while index < path.length
        char = path[index]
        case char
        when '"'
          stop = index + 1
          value = +''
          while stop < path.length && path[stop] != '"'
            if path[stop] == '\\'
              value << path[stop + 1]
              stop += 2
            else
              value << path[stop]
              stop += 1
            end
          end
          segments << value
          index = stop + 1
        when "'"
          stop = path.index("'", index + 1) || path.length
          segments << path[(index + 1)...stop]
          index = stop + 1
        when '['
          stop = path.index(']', index) || path.length
          segments << path[(index + 1)...stop].to_i
          index = stop + 1
        else
          stop = index
          stop += 1 while stop < path.length && path[stop] != '.' && path[stop] != '['
          segments << path[index...stop]
          index = stop
        end
        index += 1 if path[index] == '.'
      end
      segments
    end

    def format_path(segments)
      segments.map { |segment| segment.is_a?(Integer) ? "[#{segment}]" : segment }.join('.')
    end

    # --- serialization ------------------------------------------------------

    def serialize_key(name)
      BARE_KEY.match?(name) ? name : serialize_basic_string(name)
    end

    def serialize_basic_string(string)
      out = +'"'
      string.each_char do |char|
        escaped = STRING_ESCAPES[char]
        if escaped
          out << escaped
        elsif char.ord < 0x20 || char.ord == 0x7F
          out << format('\\u%04X', char.ord)
        else
          out << char
        end
      end
      out << '"'
      out
    end

    def serialize_value(value)
      case value
      when Raw
        value.text
      when String
        serialize_basic_string(value)
      when Symbol
        serialize_basic_string(value.to_s)
      when Integer
        value.to_s
      when Float
        serialize_float(value)
      when true, false
        value.to_s
      when Time
        value.iso8601
      when LocalDate, LocalDateTime, LocalTime
        value.to_s
      when Array
        "[#{value.map { |element| serialize_value(element) }.join(', ')}]"
      when Hash
        pairs = value.map { |key, element| "#{serialize_key(key.to_s)} = #{serialize_value(element)}" }
        "{#{pairs.join(', ')}}"
      when nil
        raise EditError, 'nil is not a TOML value'
      else
        raise EditError, "cannot serialize #{value.class} to TOML"
      end
    end

    def serialize_float(value)
      if value.nan?
        'nan'
      elsif value.infinite?
        value.positive? ? 'inf' : '-inf'
      else
        value.to_s
      end
    end

    # --- byte-level scanner -------------------------------------------------
    #
    # Builds the position map (@pairs, @tables) used to locate edits. All
    # offsets are byte offsets into @text. TOML syntax characters are ASCII,
    # so scanning byte-by-byte is safe for multi-byte UTF-8 content.

    def scan_document
      pos = 0
      bytesize = @text.bytesize
      context = []
      array_defs = {}
      counters = Hash.new(0)
      latest = {}

      while pos < bytesize
        line_start = pos
        pos = skip_inline_space(pos)
        break if pos >= bytesize

        case @text.getbyte(pos)
        when LF, CR, HASH
          pos = skip_line(pos)
        when LBRACKET
          table, pos = scan_header(pos, array_defs, counters, latest)
          context = table.path
          @tables << table
        else
          pair, pos = scan_pair(pos, line_start, context)
          @pairs << pair
        end
      end
    end

    def scan_header(pos, array_defs, counters, latest)
      pos += 1 # past [
      array = @text.getbyte(pos) == LBRACKET
      pos += 1 if array

      segments = []
      loop do
        pos = skip_inline_space(pos)
        token, pos = scan_key_token(pos)
        segments << token.value
        pos = skip_inline_space(pos)
        case @text.getbyte(pos)
        when DOT then pos += 1
        when RBRACKET
          pos += 1
          pos += 1 if array
          break
        else
          raise ParseError, 'malformed table header'
        end
      end
      pos = skip_line(pos)

      path = resolve_table_path(segments, array, array_defs, counters, latest)
      [TableInfo.new(path, array, pos), pos]
    end

    # Resolves header segments to an absolute path, numbering each
    # array-of-tables segment with its instance index.
    def resolve_table_path(segments, is_array, array_defs, counters, latest)
      array_defs[segments.join("\0")] = true if is_array
      path = []
      segments.each_with_index do |segment, index|
        path << segment
        next unless array_defs[segments[0..index].join("\0")]

        key = path.join("\0")
        instance =
          if index == segments.length - 1 && is_array
            counters[key] += 1
            counters[key] - 1
          else
            latest[key] || 0
          end
        latest[key] = instance
        path << instance
      end
      path
    end

    def scan_pair(pos, line_start, context)
      key_tokens = []
      loop do
        pos = skip_inline_space(pos)
        token, pos = scan_key_token(pos)
        key_tokens << token
        pos = skip_inline_space(pos)
        case @text.getbyte(pos)
        when DOT then pos += 1
        when EQUALS
          pos += 1
          break
        else
          raise ParseError, 'malformed key-value pair'
        end
      end
      pos = skip_inline_space(pos)
      value_start = pos
      pos = scan_value(pos)
      value_stop = pos
      pos = skip_line(pos)

      path = context + key_tokens.map(&:value)
      [PairInfo.new(path, context, key_tokens, value_start, value_stop, line_start, pos), pos]
    end

    def scan_key_token(pos)
      start = pos
      case @text.getbyte(pos)
      when QUOTE
        pos = scan_basic_string(pos)
        value = StringUtils.replace_escaped_chars(@text.byteslice(start + 1...pos - 1))
      when APOS
        pos = scan_literal_string(pos)
        value = @text.byteslice(start + 1...pos - 1)
      else
        pos += 1 while pos < @text.bytesize && bare_key_byte?(@text.getbyte(pos))
        value = @text.byteslice(start...pos)
      end
      [KeyToken.new(value, start, pos), pos]
    end

    def bare_key_byte?(byte)
      (byte >= 0x30 && byte <= 0x39) || # 0-9
        (byte >= 0x41 && byte <= 0x5A) || # A-Z
        (byte >= 0x61 && byte <= 0x7A) || # a-z
        byte == 0x2D || byte == 0x5F # - _
    end

    def scan_value(pos)
      case @text.getbyte(pos)
      when QUOTE then scan_basic_string(pos)
      when APOS then scan_literal_string(pos)
      when LBRACKET, LBRACE then scan_container(pos)
      else scan_scalar(pos)
      end
    end

    def scan_basic_string(pos)
      if @text.getbyte(pos + 1) == QUOTE && @text.getbyte(pos + 2) == QUOTE
        scan_multiline_string(pos, QUOTE, true)
      else
        pos += 1
        while pos < @text.bytesize
          byte = @text.getbyte(pos)
          break if byte == QUOTE

          pos += byte == BACKSLASH ? 2 : 1
        end
        pos + 1
      end
    end

    def scan_literal_string(pos)
      if @text.getbyte(pos + 1) == APOS && @text.getbyte(pos + 2) == APOS
        scan_multiline_string(pos, APOS, false)
      else
        pos += 1
        pos += 1 while pos < @text.bytesize && @text.getbyte(pos) != APOS
        pos + 1
      end
    end

    def scan_multiline_string(pos, delimiter, escaped)
      pos += 3
      bytesize = @text.bytesize
      while pos < bytesize
        byte = @text.getbyte(pos)
        if escaped && byte == BACKSLASH
          pos += 2
        elsif byte == delimiter && @text.getbyte(pos + 1) == delimiter && @text.getbyte(pos + 2) == delimiter
          run = 3
          run += 1 while run < 5 && @text.getbyte(pos + run) == delimiter
          return pos + run
        else
          pos += 1
        end
      end
      pos
    end

    def scan_container(pos)
      stack = [@text.getbyte(pos)]
      pos += 1
      bytesize = @text.bytesize
      while pos < bytesize && !stack.empty?
        byte = @text.getbyte(pos)
        case byte
        when QUOTE
          pos = scan_basic_string(pos)
        when APOS
          pos = scan_literal_string(pos)
        when HASH
          pos = skip_line(pos)
        when LBRACKET, LBRACE
          stack.push(byte)
          pos += 1
        when RBRACKET, RBRACE
          raise ParseError, 'unbalanced brackets' if stack.pop != matching_open(byte)

          pos += 1
        else
          pos += 1
        end
      end
      pos
    end

    def matching_open(byte)
      byte == RBRACKET ? LBRACKET : LBRACE
    end

    def scan_scalar(pos)
      line_end = pos
      line_end += 1 while line_end < @text.bytesize && @text.getbyte(line_end) != LF
      scalar = @text.byteslice(pos...line_end)
      comment = scalar.index('#')
      scalar = scalar[0...comment] if comment
      scalar = scalar.sub(/[ \t\r]+\z/, '')
      pos + scalar.bytesize
    end

    def skip_inline_space(pos)
      pos += 1 while pos < @text.bytesize && [SPACE, TAB].include?(@text.getbyte(pos))
      pos
    end

    # Advances past the end of the current line (including its newline).
    def skip_line(pos)
      pos += 1 while pos < @text.bytesize && @text.getbyte(pos) != LF
      pos < @text.bytesize ? pos + 1 : pos
    end
  end
end
