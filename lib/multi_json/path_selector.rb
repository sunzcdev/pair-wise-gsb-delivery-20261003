require "multi_json/parse_error"

module MultiJson
  # Streaming, path-selective JSON reader.
  #
  # Reads a JSON document from a chunked IO and only materializes the values
  # at the requested structural paths. Unselected portions of the document
  # are scanned and validated but never turned into Ruby objects, and the
  # input is never buffered in full: consumed chunks are discarded as the
  # parse advances. The whole document is always validated, so a syntax
  # error is reported even when it appears after every selected value.
  #
  # Paths are arrays of String object keys and non-negative Integer array
  # indexes, and several (possibly overlapping) paths can be selected in a
  # single pass. The raw JSON text of each selected value is decoded by an
  # explicitly chosen existing adapter.
  #
  #   result = MultiJson::PathSelector.load(io, [["meta", "version"], ["items", 0]],
  #     adapter: :json_gem, chunk_size: 4096)
  #   result.values  # => {["meta", "version"] => "1.0", ["items", 0] => {...}}
  #   result.missing # => paths that never matched
  class PathSelector
    # Raised when any object in the document contains the same key twice.
    # Duplicate keys are always rejected, even inside unselected subtrees.
    class DuplicateKeyError < ParseError; end

    DEFAULT_CHUNK_SIZE = 16_384

    # Outcome of a selective load. Paths that matched a value (including a
    # JSON null, which is returned as nil) appear in #values; paths that
    # never matched are listed separately in #missing.
    class Result
      attr_reader :values, :missing

      def initialize(values, missing)
        @values = values
        @missing = missing
      end

      def [](path)
        @values[path]
      end

      def fetch(path)
        @values.fetch(path) { raise KeyError, "path not found: #{path.inspect}" }
      end

      def found?(path)
        @values.key?(path)
      end

      def to_h
        @values.dup
      end
    end

    # Trie of the requested paths, used to decide which subtrees to capture
    # while the document is scanned.
    class Node
      attr_reader :terminal_paths

      def initialize
        @children = {}
        @terminal_paths = []
      end

      def add(path, depth = 0)
        if depth == path.length
          @terminal_paths << path
        else
          (@children[path[depth]] ||= Node.new).add(path, depth + 1)
        end
      end

      def child_for(key)
        @children[key]
      end

      def terminal?
        !@terminal_paths.empty?
      end
    end

    # Pull-based byte reader over a chunked IO. Only a sliding window of the
    # input is kept in memory; consumed bytes are discarded on refill. While
    # one or more captures are active, every consumed byte is also appended
    # to each active capture buffer.
    class ChunkedReader
      attr_reader :consumed

      def initialize(io, chunk_size)
        raise ArgumentError, "io must respond to #read" unless io.respond_to?(:read)
        raise ArgumentError, "chunk_size must be a positive Integer" unless chunk_size.is_a?(Integer) && chunk_size > 0

        @io = io
        @chunk_size = chunk_size
        @buffer = (+"")
        @buffer.force_encoding(Encoding::BINARY)
        @pos = 0
        @eof = false
        @consumed = 0
        @captures = []
      end

      # Returns the current byte without consuming it, or nil at end of input.
      def peek
        fill if @pos >= @buffer.bytesize
        @buffer.getbyte(@pos)
      end

      # Consumes and returns the current byte, or nil at end of input.
      def read_byte
        byte = peek
        return nil if byte.nil?

        @pos += 1
        @consumed += 1
        @captures.each { |capture| capture << byte } unless @captures.empty?
        byte
      end

      def start_capture(buffer)
        @captures << buffer
      end

      def stop_capture(buffer)
        @captures.delete(buffer)
      end

      # Validates and skips the body of a string (the opening quote has
      # already been consumed) without building a Ruby String. Raises
      # ParseError on invalid escapes, control characters, or truncation.
      def skip_string_body
        loop do
          fill if @pos >= @buffer.bytesize
          raise ParseError, "unterminated string at byte #{@consumed}" if @pos >= @buffer.bytesize

          done = if @captures.empty?
            skip_string_run
          else
            skip_string_byte
          end
          return if done
        end
      end

      private

      # Scans the current buffer for the next byte that needs attention
      # (closing quote, escape, or control character). Returns true once the
      # closing quote has been consumed.
      def skip_string_run
        index = @buffer.index(/["\\\x00-\x1F]/, @pos)
        if index.nil?
          @consumed += @buffer.bytesize - @pos
          @pos = @buffer.bytesize
          return false
        end

        byte = @buffer.getbyte(index)
        @consumed += index - @pos + 1
        @pos = index + 1
        case byte
        when 0x22 # "
          true
        when 0x5C # backslash
          skip_escape
          false
        else
          raise ParseError, "unescaped control character in string at byte #{@consumed - 1}"
        end
      end

      def skip_string_byte
        byte = read_byte
        case byte
        when nil
          raise ParseError, "unterminated string at byte #{@consumed}"
        when 0x22 # "
          true
        when 0x5C # backslash
          skip_escape
          false
        when 0x00..0x1F
          raise ParseError, "unescaped control character in string at byte #{@consumed - 1}"
        else
          false
        end
      end

      def skip_escape
        byte = read_byte
        case byte
        when 0x22, 0x5C, 0x2F, 0x62, 0x66, 0x6E, 0x72, 0x74 # " \ / b f n r t
          nil
        when 0x75 # u
          4.times do
            hex = read_byte
            raise ParseError, "invalid unicode escape at byte #{@consumed}" unless hex && PathSelector.hex_digit?(hex)
          end
        when nil
          raise ParseError, "unterminated escape sequence at byte #{@consumed}"
        else
          raise ParseError, "invalid escape sequence at byte #{@consumed}"
        end
      end

      def fill
        return if @eof

        if @pos > 0
          @buffer = @buffer.byteslice(@pos, @buffer.bytesize - @pos) || (+"")
          @buffer.force_encoding(Encoding::BINARY)
          @pos = 0
        end

        chunk = @io.read(@chunk_size)
        if chunk.nil? || chunk.empty?
          @eof = true
        else
          @buffer << chunk.b
        end
      end
    end

    def self.load(io, paths, options = {})
      new(io, paths, options).run
    end

    def initialize(io, paths, options = {})
      @adapter = resolve_adapter(options)
      @reader = ChunkedReader.new(io, options.fetch(:chunk_size, DEFAULT_CHUNK_SIZE))
      @paths = normalize_paths(paths)
      @root = Node.new
      @paths.each { |path| @root.add(path) }
      @captured = {}
    end

    def run
      parse_document
      decode_captures
    end

    def self.hex_digit?(byte)
      (byte >= 0x30 && byte <= 0x39) || (byte >= 0x41 && byte <= 0x46) || (byte >= 0x61 && byte <= 0x66)
    end

    private

    SIMPLE_ESCAPES = {
      0x22 => "\"",
      0x5C => "\\",
      0x2F => "/",
      0x62 => "\b",
      0x66 => "\f",
      0x6E => "\n",
      0x72 => "\r",
      0x74 => "\t"
    }.freeze

    def resolve_adapter(options)
      adapter_option = options[:adapter]
      if adapter_option.nil?
        raise ArgumentError, "MultiJson::PathSelector requires an explicitly chosen adapter via the :adapter option"
      end

      MultiJson.load_adapter(adapter_option)
    end

    def normalize_paths(paths)
      raise ArgumentError, "paths must be an Array of path arrays" unless paths.is_a?(Array)

      paths.map do |path|
        unless path.is_a?(Array)
          raise ArgumentError, "each path must be an Array of String keys and Integer indexes, got #{path.inspect}"
        end

        path.map do |element|
          case element
          when String
            element.dup
          when Integer
            raise ArgumentError, "array indexes in paths must be non-negative, got #{element}" if element.negative?

            element
          else
            raise ArgumentError, "path elements must be String keys or Integer indexes, got #{element.inspect}"
          end
        end.freeze
      end.uniq
    end

    def parse_document
      parse_value(@root)
      skip_whitespace
      syntax_error("unexpected data after top-level value") unless @reader.peek.nil?
    end

    def parse_value(node)
      skip_whitespace
      syntax_error("unexpected end of input") if @reader.peek.nil?

      if node && node.terminal?
        parse_captured_value(node)
      else
        parse_value_inner(node)
      end
    end

    def parse_captured_value(node)
      raw = (+"")
      raw.force_encoding(Encoding::BINARY)
      @reader.start_capture(raw)
      begin
        parse_value_inner(node)
      ensure
        @reader.stop_capture(raw)
      end
      raw.force_encoding(Encoding::UTF_8)
      node.terminal_paths.each { |path| @captured[path] = raw }
    end

    def parse_value_inner(node)
      case @reader.peek
      when 0x7B then parse_object(node) # {
      when 0x5B then parse_array(node) # [
      when 0x22 then skip_string # "
      when 0x74 then expect_literal("true") # t
      when 0x66 then expect_literal("false") # f
      when 0x6E then expect_literal("null") # n
      when 0x2D, 0x30..0x39 then parse_number # - or digit
      else
        syntax_error("unexpected character")
      end
    end

    def parse_object(node)
      @reader.read_byte # {
      skip_whitespace
      if @reader.peek == 0x7D # }
        @reader.read_byte
        return
      end

      seen = {}
      loop do
        skip_whitespace
        syntax_error("expected object key") unless @reader.peek == 0x22 # "

        key = parse_key
        if seen.key?(key)
          raise DuplicateKeyError, "duplicate object key #{key.inspect} at byte #{@reader.consumed}"
        end
        seen[key] = true

        skip_whitespace
        expect_byte(0x3A) # :
        parse_value(node && node.child_for(key))
        skip_whitespace

        case @reader.read_byte
        when 0x2C then next # ,
        when 0x7D then return # }
        when nil then syntax_error("unexpected end of input in object")
        else syntax_error("expected ',' or '}' in object")
        end
      end
    end

    def parse_array(node)
      @reader.read_byte # [
      skip_whitespace
      if @reader.peek == 0x5D # ]
        @reader.read_byte
        return
      end

      index = 0
      loop do
        parse_value(node && node.child_for(index))
        index += 1
        skip_whitespace

        case @reader.read_byte
        when 0x2C then next # ,
        when 0x5D then return # ]
        when nil then syntax_error("unexpected end of input in array")
        else syntax_error("expected ',' or ']' in array")
        end
      end
    end

    def skip_string
      @reader.read_byte # opening quote
      @reader.skip_string_body
    end

    # Reads and decodes an object key. Keys are always decoded (they are
    # needed for path matching and duplicate detection); unselected string
    # values are never decoded.
    def parse_key
      @reader.read_byte # opening quote
      buffer = (+"")
      buffer.force_encoding(Encoding::BINARY)
      loop do
        byte = @reader.read_byte
        case byte
        when nil then syntax_error("unterminated string")
        when 0x22 then break # "
        when 0x5C then buffer << decode_escape # backslash
        when 0x00..0x1F then syntax_error("unescaped control character in string")
        else buffer << byte
        end
      end
      buffer.force_encoding(Encoding::UTF_8)
    end

    def decode_escape
      byte = @reader.read_byte
      case byte
      when nil then syntax_error("unterminated escape sequence")
      when 0x75 then decode_unicode_escape # u
      else
        simple = SIMPLE_ESCAPES[byte]
        syntax_error("invalid escape sequence") unless simple

        simple
      end
    end

    def decode_unicode_escape
      high = read_hex4
      if high.between?(0xD800, 0xDBFF)
        unless @reader.read_byte == 0x5C && @reader.read_byte == 0x75 # \u
          syntax_error("expected a low surrogate escape after a high surrogate")
        end
        low = read_hex4
        syntax_error("invalid low surrogate escape") unless low.between?(0xDC00, 0xDFFF)

        (0x10000 + ((high - 0xD800) << 10) + (low - 0xDC00)).chr(Encoding::UTF_8)
      elsif high.between?(0xDC00, 0xDFFF)
        syntax_error("unexpected low surrogate escape")
      else
        high.chr(Encoding::UTF_8)
      end
    end

    def read_hex4
      value = 0
      4.times do
        byte = @reader.read_byte
        syntax_error("invalid unicode escape") unless byte && self.class.hex_digit?(byte)

        value = (value << 4) | hex_value(byte)
      end
      value
    end

    def hex_value(byte)
      case byte
      when 0x30..0x39 then byte - 0x30
      when 0x41..0x46 then byte - 0x41 + 10
      when 0x61..0x66 then byte - 0x61 + 10
      end
    end

    def parse_number
      @reader.read_byte if @reader.peek == 0x2D # -

      case @reader.peek
      when 0x30 # 0
        @reader.read_byte
      when 0x31..0x39 # 1-9
        @reader.read_byte
        @reader.read_byte while digit?(@reader.peek)
      else
        syntax_error("invalid number")
      end

      if @reader.peek == 0x2E # .
        @reader.read_byte
        syntax_error("invalid number") unless digit?(@reader.peek)
        @reader.read_byte while digit?(@reader.peek)
      end

      peek = @reader.peek
      if peek == 0x65 || peek == 0x45 # e or E
        @reader.read_byte
        peek = @reader.peek
        @reader.read_byte if peek == 0x2B || peek == 0x2D # + or -
        syntax_error("invalid number") unless digit?(@reader.peek)
        @reader.read_byte while digit?(@reader.peek)
      end
    end

    def expect_literal(word)
      word.each_byte do |expected|
        syntax_error("invalid literal") unless @reader.read_byte == expected
      end
    end

    def skip_whitespace
      loop do
        byte = @reader.peek
        break unless byte == 0x20 || byte == 0x09 || byte == 0x0A || byte == 0x0D

        @reader.read_byte
      end
    end

    def expect_byte(expected)
      syntax_error("expected #{expected.chr.inspect}") unless @reader.read_byte == expected
    end

    def digit?(byte)
      !byte.nil? && byte >= 0x30 && byte <= 0x39
    end

    def syntax_error(message)
      raise ParseError, "#{message} at byte #{@reader.consumed}"
    end

    def decode_captures
      values = {}
      @paths.each do |path|
        next unless @captured.key?(path)

        raw = @captured[path]
        values[path] = begin
          @adapter.load(raw)
        rescue @adapter::ParseError => e
          raise ParseError.build(e, raw)
        end
      end
      missing = @paths.reject { |path| @captured.key?(path) }
      Result.new(values, missing)
    end
  end
end