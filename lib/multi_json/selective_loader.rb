require "multi_json" unless defined?(MultiJson::ParseError)

module MultiJson
  # Selectively decodes a handful of values out of a large JSON document
  # that is served as a chunked IO object.
  #
  # Unlike a full MultiJson.load, unselected parts of the document are
  # scanned byte by byte: they are never turned into Ruby values and their
  # raw bytes are not retained. The whole document is still validated, so a
  # syntax error anywhere (including trailing bytes) makes the whole call
  # fail even when every requested path was found early.
  class SelectiveLoader
    # Raised when the same key appears twice in one JSON object.
    # Subclasses ParseError so existing error handling keeps working.
    class DuplicateKeyError < ParseError; end

    # The outcome of a selective load.
    #
    # * +values+  - path => decoded value for paths that matched a non-null
    #   JSON value.
    # * +null+    - paths that matched an explicit JSON +null+.
    # * +missing+ - requested paths that do not exist in the document.
    #
    # Matching null and being absent are deliberately different outcomes.
    class Result
      attr_reader :values, :null, :missing

      def initialize(values, null_paths, missing_paths)
        @values = values
        @null = null_paths
        @missing = missing_paths
        @null_set = null_paths
        @missing_set = missing_paths
      end

      # Returns the decoded value when +path+ matched a value, +nil+ when it
      # matched JSON null.
      def [](path)
        values.fetch(path) { nil if @null_set.include?(path) }
      end

      def found?(path)
        values.key?(path) || @null_set.include?(path)
      end

      def null?(path)
        @null_set.include?(path)
      end

      def missing?(path)
        @missing_set.include?(path)
      end
    end

    DEFAULT_CHUNK_SIZE = 4096
    ACCEPTED_DUPLICATE_KEY_STRATEGIES = [:raise].freeze
    LOADER_OPTIONS = [:adapter, :chunk_size, :duplicate_keys].freeze

    # @param paths [Array<Array>] structural paths; every element is either a
    #   String (object key) or a non-negative Integer (array index).
    # @param options [Hash]
    # @option options [Symbol, String, Class] :adapter (required) adapter used
    #   to decode every selected value.
    # @option options [Integer] :chunk_size (4096) maximum number of bytes
    #   requested from the IO at once.
    # @option options [Symbol] :duplicate_keys (:raise) only +:raise+ is
    #   accepted: duplicate object keys fail the whole load with
    #   MultiJson::SelectiveLoader::DuplicateKeyError.
    # @option options [Boolean] :symbolize_keys passed through to the adapter.
    def initialize(paths, **options)
      @paths = validate_paths(paths)
      @adapter = options.fetch(:adapter) {
        raise ArgumentError, "Selective loading requires an explicit :adapter option"
      }
      @chunk_size = options.fetch(:chunk_size, DEFAULT_CHUNK_SIZE)
      unless @chunk_size.is_a?(Integer) && @chunk_size.positive?
        raise ArgumentError, ":chunk_size must be a positive Integer"
      end
      @duplicate_keys = options.fetch(:duplicate_keys, :raise)
      unless ACCEPTED_DUPLICATE_KEY_STRATEGIES.include?(@duplicate_keys)
        raise ArgumentError,
          "unknown :duplicate_keys strategy #{@duplicate_keys.inspect}, accepted values: :raise"
      end
      @adapter_options = options.reject { |key, _| LOADER_OPTIONS.include?(key) }
    end

    # Streams +io+ fully, validates the document and decodes selected values
    # with the configured adapter.
    #
    # @param io [#read] a chunked IO-like source. The whole document is never
    #   read into memory at once.
    # @return [Result]
    def load(io)
      raise ArgumentError, "expected an IO-like object responding to #read" unless io.respond_to?(:read)

      adapter = MultiJson.current_adapter(adapter: @adapter)
      # A one-shot :adapter load does not go through MultiJson.use, so make
      # sure the adapter options cache is initialized the first time around.
      unless OptionsCache.instance_variable_defined?(:@load_cache)
        OptionsCache.reset
      end
      trie = build_trie(@paths)
      matches = Scanner.new(io, trie, @chunk_size).scan
      build_result(adapter, matches)
    rescue DuplicateKeyError
      raise
    rescue ParseError => e
      raise ParseError.build(e, nil)
    end

    private

    def validate_paths(paths)
      unless paths.is_a?(Array)
        raise ArgumentError, "paths must be an Array of paths"
      end
      paths.map do |path|
        unless path.is_a?(Array) && !path.empty? &&
            path.all? { |segment| valid_key?(segment) || valid_index?(segment) }
          raise ArgumentError,
            "invalid path #{path.inspect}: every segment must be a String key or a non-negative Integer index"
        end
        path.dup.freeze
      end.uniq
    end

    def valid_key?(segment)
      segment.is_a?(String)
    end

    def valid_index?(segment)
      segment.is_a?(Integer) && segment >= 0
    end

    def build_trie(paths)
      root = Scanner::Node.new
      paths.each do |path|
        node = root
        path.each do |segment|
          node = node.children[segment] ||= Scanner::Node.new
        end
        node.terminal = true
      end
      root
    end

    def build_result(adapter, matches)
      values = {}
      null_paths = []
      matches.each do |path, (raw, is_null)|
        if is_null
          null_paths << path
        else
          values[path] = decode(adapter, raw)
        end
      end
      missing_paths = @paths.reject { |path| matches.key?(path) }
      Result.new(values, null_paths, missing_paths)
    end

    def decode(adapter, raw)
      snippet = prepare_snippet(raw)
      adapter.load(snippet, @adapter_options)
    rescue adapter::ParseError, ArgumentError => e
      raise ParseError.build(e, raw)
    end

    # Captured bytes are stored as ASCII-8BIT so byte slicing across chunks is
    # encoding agnostic. The scanner validated them as UTF-8 on the way in, so
    # tag valid snippets accordingly: some adapters (ok_json) cannot decode a
    # binary-encoded string containing non-ASCII bytes.
    def prepare_snippet(raw)
      return raw if raw.encoding == Encoding::UTF_8 && raw.valid_encoding?
      candidate = raw.dup.force_encoding(Encoding::UTF_8)
      candidate.valid_encoding? ? candidate : raw
    end

    # Streaming, byte oriented JSON validator/selector.
    #
    # The scanner keeps at most one chunk of the document buffered plus the
    # raw bytes of the currently selected (possibly overlapping) subtrees.
    # Unselected strings, arrays and objects are parsed and validated without
    # buffering their contents.
    class Scanner
      STRING_ESCAPES = {
        34 => 34, # "
        92 => 92, # \
        47 => 47, # '
        98 => 8,  # b
        102 => 12, # f
        110 => 10, # n
        114 => 13, # r
        116 => 9   # t
      }.freeze

      Node = Struct.new(:children, :terminal) do
        def initialize(*)
          super({}, false)
        end
      end

      # Node returned once a path leaves the requested trie. Everything below
      # it is unselected.
      MISS = Node.new.freeze

      # Non-terminal node used for parts of the document that lie below a
      # selected terminal (whose whole subtree is being captured) but that
      # may still contain deeper, separately requested terminals.
      OPEN = Node.new.freeze

      def initialize(io, root, chunk_size)
        @io = io
        @root = root
        @chunk_size = chunk_size
        @buffer = "".dup.force_encoding(Encoding::ASCII_8BIT)
        @pos = 0
        @offset = 0
        @captures = []
        @found = {}
        @path = []
      end

      # @return [Hash] path => [raw JSON String, null?]
      def scan
        byte = skip_ws
        syntax_error!("unexpected end of document") if byte.nil?
        parse_value(byte, @root)
        trailing = skip_ws
        syntax_error!("unexpected trailing data after the top-level value") unless trailing.nil?
        @found
      end

      private

      # ---- low level chunk handling -------------------------------------

      def take
        if @pos >= @buffer.bytesize
          return nil unless refill
        end
        byte = @buffer.getbyte(@pos)
        @pos += 1
        @offset += 1
        append_to_captures(byte)
        byte
      end

      def peek
        if @pos >= @buffer.bytesize
          return nil unless refill
        end
        @buffer.getbyte(@pos)
      end

      def refill
        chunk = @io.read(@chunk_size)
        chunk = chunk.to_s if chunk
        return false if chunk.nil? || chunk.bytesize.zero?
        chunk = chunk.dup.force_encoding(Encoding::ASCII_8BIT)
        @buffer = chunk
        @pos = 0
        true
      end

      def append_to_captures(byte)
        @captures.each { |capture| capture << byte }
      end

      def start_capture(first)
        capture = "".dup.force_encoding(Encoding::ASCII_8BIT)
        # Seed with the value's first byte, which the caller already
        # consumed: a nested capture starts after the byte was taken, while
        # outer captures also receive it through append_to_captures and must
        # not get it twice.
        capture << first
        @captures.push(capture)
        capture
      end

      def finish_capture(capture)
        @captures.delete(capture)
        capture
      end

      # ---- whitespace / errors ------------------------------------------

      def skip_ws
        loop do
          byte = take
          case byte
          when nil then return nil
          when 32, 9, 10, 13 then next
          else return byte
          end
        end
      end

      def syntax_error!(message)
        raise ParseError, "#{message} at byte offset #{@offset}"
      end

      def expect(byte, expected_byte, what)
        syntax_error!("expected #{what}") unless byte == expected_byte
      end

      # ---- values --------------------------------------------------------

      # Resolves the trie node for a child element. Terminal nodes stay open
      # so deeper, separately requested paths nested inside a captured
      # subtree are still found.
      def child_node(node, label)
        return MISS if node.equal?(MISS) || node.equal?(OPEN)
        node.children.fetch(label) do
          (node.terminal) ? OPEN : MISS
        end
      end

      def parse_value(first, node)
        if node.equal?(MISS)
          skip_value(first)
          return
        end

        if node.terminal && node.children.empty?
          path = @path.dup.freeze
          capture = start_capture(first)
          raw =
            begin
              skip_value(first)
              finish_capture(capture)
            rescue
              @captures.delete(capture)
              raise
            end
          @found[path] = [raw, first == 110] # 'n' => null
          return
        end

        if node.terminal
          # Terminal with nested requests: capture the whole subtree while
          # still walking it, so overlapping deeper paths are selected too.
          capture = start_capture(first)
          path = @path.dup.freeze
          begin
            descend_value(first, node)
            raw = finish_capture(capture)
            @found[path] = [raw, first == 110] # 'n' => null
          rescue
            @captures.delete(capture)
            raise
          end
          return
        end

        descend_value(first, node)
      end

      def descend_value(first, node)
        case first
        when 123 then parse_object(node)
        when 91 then parse_array(node)
        when 34 then consume_string
        when 116, 102, 110 then consume_literal(first)
        when 45, 48..57 then consume_number(first)
        else syntax_error!("unexpected byte #{first.inspect}")
        end
      end

      def parse_object(node)
        byte = skip_ws
        if byte == 125 # '}'
          return
        end

        keys = {}
        loop do
          syntax_error!("expected '}' or an object key") unless byte == 34 # '"'
          key = parse_key
          if keys.key?(key)
            raise DuplicateKeyError,
              "duplicate object key #{key.inspect} at byte offset #{@offset}"
          end
          keys[key] = true

          byte = skip_ws
          expect(byte, 58, "':'") # ':'
          child = child_node(node, key)

          byte = skip_ws
          syntax_error!("unexpected end of document inside object") if byte.nil?
          @path.push(key)
          begin
            parse_value(byte, child)
          ensure
            @path.pop
          end

          byte = skip_ws
          case byte
          when 125 then return # '}'
          when 44 # ','
            byte = skip_ws
            syntax_error!("unexpected trailing comma in object") if byte == 125
          else syntax_error!("expected ',' or '}' in object")
          end
        end
      end

      def parse_array(node)
        byte = skip_ws
        if byte == 93 # ']'
          return
        end

        index = 0
        loop do
          syntax_error!("unexpected end of document inside array") if byte.nil?
          child = child_node(node, index)
          @path.push(index)
          begin
            parse_value(byte, child)
          ensure
            @path.pop
          end

          byte = skip_ws
          case byte
          when 93 then return # ']'
          when 44 # ','
            byte = skip_ws
            syntax_error!("unexpected trailing comma in array") if byte == 93
            index += 1
          else syntax_error!("expected ',' or ']' in array")
          end
        end
      end

      # Fully consumes a value without building anything. Used for unselected
      # subtrees and for validating captured selected values.
      def skip_value(first)
        case first
        when 34 then consume_string
        when 123 # '{'
          byte = skip_ws
          unless byte == 125
            keys = {}
            loop do
              syntax_error!("expected an object key") unless byte == 34
              key = parse_key
              if keys.key?(key)
                raise DuplicateKeyError,
                  "duplicate object key #{key.inspect} at byte offset #{@offset}"
              end
              keys[key] = true
              byte = skip_ws
              syntax_error!("expected ':'") unless byte == 58
              byte = skip_ws
              syntax_error!("unexpected end of document inside object") if byte.nil?
              skip_value(byte)
              byte = skip_ws
              case byte
              when 125 then return
              when 44 then byte = skip_ws
              else syntax_error!("expected ',' or '}' in object")
              end
            end
          end
        when 91 # '['
          byte = skip_ws
          unless byte == 93
            loop do
              syntax_error!("unexpected end of document inside array") if byte.nil?
              skip_value(byte)
              byte = skip_ws
              case byte
              when 93 then return
              when 44 then byte = skip_ws
              else syntax_error!("expected ',' or ']' in array")
              end
            end
          end
        when 116 then consume_literal(116)
        when 102 then consume_literal(102)
        when 110 then consume_literal(110)
        when 45, 48..57 then consume_number(first)
        else syntax_error!("unexpected byte #{first.inspect}")
        end
      end

      # ---- strings -------------------------------------------------------

      # The opening quote has already been consumed by the caller. Buffers
      # the raw key bytes (needed so a selected ancestor capture contains a
      # valid document) and returns the decoded key (needed for path matching
      # and duplicate detection).
      def parse_key
        key = "".dup.force_encoding(Encoding::UTF_8)
        consume_string(key)
      end

      # Shared implementation for object keys and regular strings. The
      # opening quote is assumed consumed; the closing quote is consumed too.
      def consume_string(decoded = nil)
        loop do
          byte = take
          case byte
          when nil then syntax_error!("unterminated string")
          when 34 then return decoded # '"'
          when 92 # '\\'
            byte = take
            syntax_error!("unterminated escape sequence") if byte.nil?
            if byte == 117 # 'u'
              append_unescaped_unicode(decoded)
            else
              mapped = STRING_ESCAPES[byte]
              syntax_error!("invalid escape character #{byte.chr.inspect}") unless mapped
              decoded << mapped.chr(Encoding::UTF_8) if decoded
            end
          when 0..31
            syntax_error!("unescaped control character in string")
          when 0..127
            decoded << byte.chr(Encoding::UTF_8) if decoded
          else
            consume_multibyte(byte, decoded)
          end
        end
      end

      def consume_multibyte(first, decoded)
        if first >= 0xF0
          consume_continuation(first, decoded, 3, 0x10000, 0x10FFFF)
        elsif first >= 0xE0
          consume_continuation(first, decoded, 2, 0x800, 0xFFFF)
        elsif first >= 0xC0
          consume_continuation(first, decoded, 1, 0x80, 0x7FF)
        else
          syntax_error!("invalid UTF-8 leading byte")
        end
      end

      def consume_continuation(first, decoded, count, min, max)
        codepoint = first & (0x7F >> count)
        count.times do
          byte = take
          syntax_error!("truncated multibyte character") if byte.nil?
          unless byte >= 0x80 && byte <= 0xBF
            syntax_error!("invalid UTF-8 continuation byte")
          end
          codepoint = (codepoint << 6) | (byte & 0x3F)
        end
        syntax_error!("overlong UTF-8 sequence") if codepoint < min
        syntax_error!("UTF-8 codepoint out of range") if codepoint > max
        if codepoint >= 0xD800 && codepoint <= 0xDFFF
          syntax_error!("UTF-8 encoded surrogate")
        end
        decoded << codepoint.chr(Encoding::UTF_8) if decoded
      end

      def append_unescaped_unicode(decoded)
        high = take_hex4
        if high >= 0xD800 && high <= 0xDBFF
          byte = take
          syntax_error!("truncated surrogate pair") if byte.nil?
          unless byte == 92 # '\\'
            syntax_error!("lone high surrogate; expected '\\u'")
          end
          byte = take
          syntax_error!("truncated surrogate pair") if byte.nil?
          unless byte == 117 # 'u'
            syntax_error!("lone high surrogate; expected '\\u'")
          end
          low = take_hex4
          unless low >= 0xDC00 && low <= 0xDFFF
            syntax_error!("low surrogate expected after high surrogate")
          end
          codepoint = 0x10000 + ((high - 0xD800) << 10) + (low - 0xDC00)
          decoded << codepoint.chr(Encoding::UTF_8) if decoded
        elsif high >= 0xDC00 && high <= 0xDFFF
          syntax_error!("lone low surrogate")
        else
          decoded << high.chr(Encoding::UTF_8) if decoded
        end
      end

      def take_hex4
        value = 0
        4.times do
          byte = take
          syntax_error!("truncated \\u escape") if byte.nil?
          digit = hex_digit(byte)
          syntax_error!("invalid hexadecimal digit in \\u escape") if digit.nil?
          value = (value << 4) | digit
        end
        value
      end

      def hex_digit(byte)
        if byte >= 48 && byte <= 57 then byte - 48
        elsif byte >= 97 && byte <= 102 then byte - 97 + 10
        elsif byte >= 65 && byte <= 70 then byte - 65 + 10
        end
      end

      # ---- literals / numbers -------------------------------------------

      def consume_literal(first)
        word =
          case first
          when 116 then "true"
          when 102 then "false"
          when 110 then "null"
          else syntax_error!("invalid literal")
          end
        # The first byte was already consumed.
        word.bytesize.pred.times do |i|
          byte = take
          expected = word.getbyte(i + 1)
          syntax_error!("invalid literal") unless byte == expected
        end
      end

      def consume_number(first)
        if first == 45 # '-'
          byte = take
          syntax_error!("truncated number") if byte.nil?
          first = byte
        end

        if first == 48 # '0'
          # no leading zeros allowed
        elsif first >= 49 && first <= 57
          loop do
            byte = peek
            break unless byte && byte >= 48 && byte <= 57
            take
          end
        else
          syntax_error!("invalid number")
        end

        byte = peek
        if byte == 46 # '.'
          take
          byte = take
          syntax_error!("truncated fraction") if byte.nil?
          syntax_error!("digit expected after decimal point") unless byte >= 48 && byte <= 57
          loop do
            byte = peek
            break unless byte && byte >= 48 && byte <= 57
            take
          end
        end

        byte = peek
        if byte == 101 || byte == 69 # 'e' / 'E'
          take
          byte = take
          syntax_error!("truncated exponent") if byte.nil?
          if byte == 43 || byte == 45 # '+' / '-'
            byte = take
            syntax_error!("truncated exponent") if byte.nil?
          end
          syntax_error!("digit expected in exponent") unless byte >= 48 && byte <= 57
          loop do
            byte = peek
            break unless byte && byte >= 48 && byte <= 57
            take
          end
        end
      end
    end
  end

  class << self
    # Streams a large JSON document from +io+ and decodes only the selected
    # paths.
    #
    #   result = MultiJson.load_paths(io, [["meta", "version"], ["items", 0]],
    #                                 adapter: :json_gem)
    #   result.values                # {["meta","version"] => "1.2"}
    #   result.missing               # [["items", 0]]
    #   result.null?(["a", "b"])     # matched an explicit null
    #
    # The document is never read fully and unselected values are never built
    # as Ruby objects, but syntax is validated for the entire document.
    #
    # @see MultiJson::SelectiveLoader#initialize
    def load_paths(io, paths, **options)
      SelectiveLoader.new(paths, **options).load(io)
    end
  end
end
