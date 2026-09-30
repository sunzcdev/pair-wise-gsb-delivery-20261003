require "stringio"

module MultiJson
  # Media type of JSON Text Sequences as registered by RFC 7464.
  SEQUENCE_CONTENT_TYPE = "application/json-seq"

  # Base class for errors raised while processing JSON Text Sequences.
  class SequenceError < StandardError; end

  # A single JSON Text Sequence record could not be parsed.
  class MalformedRecord < SequenceError
    # @return [Range(Integer, Integer)] half-open byte range, relative to the
    #   start of the stream, covering the record payload between the RS and
    #   the LF framing bytes
    attr_reader :byte_range

    # @return [String, nil] raw bytes of the bad record payload, or +nil+ when
    #   they were discarded because the record exceeded the size limit
    attr_reader :raw_bytes

    # @return [Exception, nil] adapter exception that caused the failure
    attr_reader :cause

    def initialize(message = nil, byte_range: nil, raw_bytes: nil, cause: nil)
      super(message || "malformed JSON sequence record")
      @byte_range = byte_range
      @raw_bytes = raw_bytes
      @cause = cause
    end
  end

  # A record was never terminated with an LF (end of stream or a new RS was
  # reached first), so per RFC 7464 section 2.1 it must not be reported as a
  # complete JSON value.
  class RecordTruncated < MalformedRecord
    def initialize(byte_range:, raw_bytes:)
      super("truncated JSON sequence record", byte_range: byte_range, raw_bytes: raw_bytes)
    end
  end

  # A record exceeded the configured +max_record_bytes+ limit.
  class RecordTooLarge < MalformedRecord
    # @return [Integer] configured record size limit in bytes
    attr_reader :limit

    def initialize(limit, byte_range:)
      super("JSON sequence record exceeds #{limit} byte limit", byte_range: byte_range, raw_bytes: nil)
      @limit = limit
    end
  end

  # Adapter exposing a byte-oriented source as the single-byte +getbyte+
  # interface the reader frames records with.
  class SequenceByteReader
    def initialize(io)
      @io = io
    end

    def getbyte
      chunk = @io.read(1)
      return nil if chunk.nil? || chunk.empty?

      chunk.ord
    end
  end

  # Streaming reader for JSON Text Sequences (RFC 7464).
  #
  # The stream is consumed one byte at a time. Only the payload of the record
  # currently being parsed is buffered, so arbitrarily large streams can be
  # processed as records arrive.
  class SequenceReader
    include Enumerable

    RS = 0x1E # ASCII record separator
    LF = 0x0A # line feed

    # @return [Integer] number of bytes consumed from the source so far
    attr_reader :position

    # @param source [String, #getbyte, #read] string or byte-oriented IO
    # @param on_bad_record [:raise, :skip, #call] policy for records that fail
    #   to parse or are truncated: +:raise+ stops the stream, +:skip+ resumes
    #   at the next record, a callable receives the {MalformedRecord}
    # @param max_record_bytes [Integer, nil] maximum accepted record payload
    #   size in bytes; larger records are reported as {RecordTooLarge} and
    #   discarded without being buffered
    # @param load_options options forwarded to the current adapter
    def initialize(source, on_bad_record: :raise, max_record_bytes: nil, **load_options)
      @io = wrap_source(source)
      @on_bad_record = on_bad_record
      @max_record_bytes = max_record_bytes
      @load_options = load_options
      @position = 0
      @inside_record = false
      validate_policy!
    end

    # Yields each successfully parsed record. Without a block returns an
    # Enumerator, making the reader usable with +next+ and lazy chains.
    def each
      return to_enum(:each) unless block_given?

      loop do
        event = next_event
        break if event.nil?

        status, payload = event
        case status
        when :ok
          yield payload
        when :bad
          handle_bad_record(payload)
        when :empty
          # silently skipped
        end
      end

      self
    end

    private

    def next_event
      loop do
        unless @inside_record
          return nil unless seek_record_start

          start_record
        end

        byte = read_byte

        if byte.nil?
          @inside_record = false
          return nil if blank?(@buffer)

          return [:bad, RecordTruncated.new(
            byte_range: @record_start...@position,
            raw_bytes: @buffer.dup
          )]
        end

        case byte
        when RS
          # RFC 7464 section 2.4: resynchronize at the next RS. An empty or
          # whitespace-only segment is skipped silently; a non-empty segment is
          # an unterminated, possibly truncated record.
          if blank?(@buffer)
            start_record
          else
            event = [:bad, RecordTruncated.new(
              byte_range: @record_start...(@position - 1),
              raw_bytes: @buffer.dup
            )]
            start_record
            return event
          end
        when LF
          @inside_record = false
          return complete_record
        else
          buffer_byte(byte)
        end
      end
    end

    def seek_record_start
      loop do
        byte = read_byte
        return false if byte.nil?
        return true if byte == RS
      end
    end

    def start_record
      @record_start = @position
      @buffer = empty_buffer
      @too_large = false
      @inside_record = true
    end

    def buffer_byte(byte)
      if !@too_large && @max_record_bytes && @buffer.bytesize >= @max_record_bytes
        @too_large = true
        @buffer = empty_buffer
        return
      end

      @buffer << byte unless @too_large
    end

    def complete_record
      range = @record_start...(@position - 1)

      if @too_large
        return [:bad, RecordTooLarge.new(@max_record_bytes, byte_range: range)]
      end

      return [:empty, nil] if blank?(@buffer)

      begin
        [:ok, MultiJson.load(@buffer, @load_options)]
      rescue ParseError => e
        [:bad, MalformedRecord.new(
          byte_range: range,
          raw_bytes: @buffer.dup,
          cause: e
        )]
      end
    end

    def handle_bad_record(error)
      if @on_bad_record.respond_to?(:call)
        @on_bad_record.call(error)
      elsif @on_bad_record == :skip
        # resume with the next record
      else
        raise error
      end
    end

    def validate_policy!
      policy = @on_bad_record
      return if policy.nil? || policy == :raise || policy == :skip || policy.respond_to?(:call)

      raise SequenceError, ":on_bad_record must be :raise, :skip, or a callable"
    end

    def read_byte
      byte = @io.getbyte
      return nil if byte.nil?

      @position += 1
      byte
    end

    def blank?(buffer)
      buffer.strip.empty?
    end

    def empty_buffer
      String.new.force_encoding(Encoding::ASCII_8BIT)
    end

    def wrap_source(source)
      case source
      when String
        StringIO.new(source.b)
      else
        if source.respond_to?(:getbyte)
          source
        elsif source.respond_to?(:read)
          SequenceByteReader.new(source)
        else
          raise SequenceError, "JSON sequence source must be a String or respond to #read or #getbyte"
        end
      end
    end
  end

  # Streaming writer for JSON Text Sequences (RFC 7464).
  #
  # Each object is dumped with the current adapter and immediately framed with
  # RS/LF bytes, so records can be consumed while they are being written.
  class SequenceWriter
    RS = "\x1E"
    LF = "\n"

    # @return [IO] wrapped destination stream
    attr_reader :io

    # @param io [IO, nil] destination stream; when nil an internal StringIO is
    #   used and the built sequence is available through #string
    # @param dump_options options forwarded to the current adapter
    def initialize(io = nil, **dump_options)
      @io = io || StringIO.new(empty_buffer)
      @dump_options = dump_options
    end

    # Dumps one object as an RS-framed record.
    def write(object)
      @io.write(RS)
      @io.write(MultiJson.dump(object, @dump_options))
      @io.write(LF)
      self
    end

    alias_method :<<, :write

    # Flushes the destination stream when supported.
    def flush
      @io.flush if @io.respond_to?(:flush)
      self
    end

    # @return [String, nil] accumulated sequence when writing to an internal
    #   StringIO, nil otherwise
    def string
      flush
      @io.string if @io.respond_to?(:string)
    end

    private

    def empty_buffer
      String.new.force_encoding(Encoding::ASCII_8BIT)
    end
  end
end
