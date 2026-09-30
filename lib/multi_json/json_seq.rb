require "multi_json"

module MultiJson
  # RFC 7464 *JavaScript Object Notation (JSON) Text Sequences*
  # (+application/json-seq+).
  #
  # A JSON text sequence is a stream of self contained JSON values. Each
  # value is prefixed with an ASCII Record Separator (+RS+, <tt>"\u001E"</tt>)
  # and terminated with an ASCII Line Feed (+LF+, <tt>"\n"</tt>). Unlike a
  # JSON array the format is stream friendly: records may be consumed while
  # they are still arriving and a single malformed record does not invalidate
  # the records that follow it.
  #
  # The actual encoding and decoding of every individual record is delegated
  # to the currently selected MultiJson adapter. This module only takes care
  # of the framing, reading an unbounded stream one byte at a time and
  # recovering from malformed records.
  module JsonSeq
    # ASCII Record Separator: marks the beginning of a record.
    RS = "\x1E".b.freeze

    # ASCII Line Feed: marks the end of a record.
    LF = "\n".b.freeze

    # Base class for all errors raised while processing a sequence.
    class Error < StandardError; end

    # Raised (in +:stop+ mode) when a record cannot be decoded as JSON. The
    # exception carries the absolute byte range of the offending record so
    # callers can report or skip it precisely.
    class MalformedRecordError < Error
      attr_reader :start_byte, :end_byte, :raw, :record_error

      def self.build(bad_record)
        message = +"malformed JSON sequence record at bytes #{bad_record.start_byte}...#{bad_record.end_byte}"
        message << ": #{bad_record.error.message}" unless bad_record.error.nil? || bad_record.error.message.nil?
        new(message, bad_record)
      end

      def initialize(message, bad_record)
        super(message)
        @start_byte = bad_record.start_byte
        @end_byte = bad_record.end_byte
        @raw = bad_record.raw
        @record_error = bad_record.error
      end
    end

    # Raised when a record grows beyond the configured +max_record_size+.
    # By the time this is raised the reader has already consumed the rest of
    # the oversized record and is positioned at the beginning of the next
    # record, so callers may rescue the error and keep reading.
    class RecordTooLargeError < Error
      attr_reader :start_byte, :end_byte, :raw

      def initialize(message, start_byte, end_byte, raw)
        super(message)
        @start_byte = start_byte
        @end_byte = end_byte
        @raw = raw
      end
    end

    # Placeholder returned for a record whose bytes could not be decoded when
    # the reader was created with <tt>on_invalid: :continue</tt>. It exposes
    # the absolute byte range and the raw bytes of the bad record.
    class BadRecord
      attr_reader :start_byte, :end_byte, :raw, :error

      def initialize(start_byte, end_byte, raw, error)
        @start_byte = start_byte
        @end_byte = end_byte
        @raw = raw
        @error = error
      end

      def bad?
        true
      end

      def inspect
        "#<#{self.class} bytes #{start_byte}...#{end_byte}: #{error&.message}>"
      end
    end

    # Sentinel returned by {Reader#next_record} once the stream is exhausted.
    EOF = Class.new do
      def inspect
        "#<MultiJson::JsonSeq::EOF>"
      end
      alias_method :to_s, :inspect
    end.allocate.freeze

    # @private internal marker for an empty JSON text (RFC 7464 section 2.2)
    EMPTY_RECORD = Object.new.freeze

    # Writes JSON values to an IO as an RFC 7464 sequence.
    #
    #   writer = MultiJson.json_seq_writer($stdout)
    #   writer.write(name: "foo")
    #   writer << [1, 2, 3]
    #
    # Encoding is performed by the current adapter (or one given through the
    # +adapter:+ option); any additional options are forwarded to
    # MultiJson.dump.
    class Writer
      # @param io [#write] object the framed records are written to
      # @param adapter [Symbol, Class, nil] adapter to use for encoding
      # @param dump_options [Hash] options forwarded to MultiJson.dump
      def initialize(io, adapter: nil, **dump_options)
        @io = io
        @adapter = adapter
        @dump_options = dump_options
      end

      # Encodes +object+ as a single framed record.
      # @return [Writer] self
      def write(object)
        payload = MultiJson.dump(object, dump_options_with_adapter)
        record = RS.dup
        record << payload.b
        record << LF
        @io.write(record)
        self
      end

      alias_method :<<, :write

      # Flushes the underlying IO when it supports flushing.
      # @return [Writer] self
      def flush
        @io.flush if @io.respond_to?(:flush)
        self
      end

      # Flushes and closes the underlying IO when it supports closing.
      def close
        flush
        @io.close if @io.respond_to?(:close)
        nil
      end

      private

      def dump_options_with_adapter
        @dump_options.merge(adapter: @adapter)
      end
    end

    # Reads JSON values from an RFC 7464 sequence.
    #
    # The reader pulls bytes from the given IO one byte at a time, so input
    # may arrive in arbitrarily small chunks (for example a log file that is
    # being appended to or a long lived socket). Only a single record is held
    # in memory at a time.
    #
    #   reader = MultiJson.json_seq_reader(io, on_invalid: :continue)
    #   reader.each do |record|
    #     next if record.is_a?(MultiJson::JsonSeq::BadRecord)
    #     process(record)
    #   end
    class Reader
      include Enumerable

      # @param io [#read] stream containing the sequence
      # @param adapter [Symbol, Class, nil] adapter to use for decoding
      # @param max_record_size [Integer, nil] maximum allowed size, in bytes,
      #   of a single record (excluding the framing RS and LF)
      # @param on_invalid [Symbol] +:stop+ (the default) raises a
      #   {MalformedRecordError} on the first bad record, +:continue+ skips to
      #   the next RS and returns a {BadRecord} instead
      # @param on_bad_record [#call, nil] optional callback invoked with every
      #   {BadRecord}; useful for logging while iterating
      # @param load_options [Hash] options forwarded to MultiJson.load
      def initialize(io, adapter: nil, max_record_size: nil, on_invalid: :stop,
                     on_bad_record: nil, **load_options)
        unless on_invalid.equal?(:stop) || on_invalid.equal?(:continue)
          raise ArgumentError, "on_invalid must be :stop or :continue"
        end
        if max_record_size && (!max_record_size.is_a?(Integer) || max_record_size < 1)
          raise ArgumentError, "max_record_size must be a positive integer"
        end

        @io = io
        @adapter = adapter
        @max_record_size = max_record_size
        @on_invalid = on_invalid
        @on_bad_record = on_bad_record
        @load_options = load_options

        @offset = 0
        @final_eof = false
        @pending_byte = nil
      end

      # @return [Integer] number of bytes consumed from the stream so far
      attr_reader :offset

      # Returns the next decoded record, a {BadRecord} (in +:continue+ mode)
      # or the {EOF} sentinel once the stream is exhausted.
      #
      # Records terminated by EOF instead of LF are handled according to
      # RFC 7464: complete non numeric values are returned, while a top level
      # number without the terminating LF can be a prefix of a longer number
      # and is therefore never accepted as a complete value.
      def next_record
        return EOF if @final_eof

        loop do
          return EOF if skip_to_record_start == :eof

          start_byte = @offset - 1
          result = read_record_body(start_byte)
          return result unless result.equal?(EMPTY_RECORD)
        end
      end

      # Convenience wrapper around {#next_record} that returns +nil+ at EOF
      # instead of the {EOF} sentinel.
      def read_record
        record = next_record
        record.equal?(EOF) ? nil : record
      end

      # Iterates over records until the stream is exhausted.
      def each
        return to_enum(:each) unless block_given?

        loop do
          record = next_record
          break if record.equal?(EOF)
          yield record
        end
        self
      end

      private

      # Consumes bytes up to and including the RS that opens a record. Bytes
      # encountered before the first RS (data from an incomplete previous
      # record or arbitrary garbage) are silently discarded.
      def skip_to_record_start
        loop do
          byte = read_byte
          if byte.nil?
            @final_eof = true
            return :eof
          end
          return :start if byte == RS
        end
      end

      # Reads everything between the opening RS and the next LF, RS or EOF.
      def read_record_body(start_byte)
        buffer = String.new.b
        terminator = nil
        oversized = false

        loop do
          byte = read_byte

          if byte.nil?
            terminator = :eof
            @final_eof = true
            break
          end

          if byte == RS
            # A record without a terminating LF. The RS belongs to the next
            # record, so push it back for the next iteration.
            @pending_byte = byte
            terminator = :rs
            break
          end

          if byte == LF
            buffer << byte
            terminator = :lf
            break
          end

          if @max_record_size && buffer.bytesize >= @max_record_size
            oversized = true
          else
            buffer << byte
          end
        end

        if oversized
          drain_oversized_record(start_byte, buffer, terminator)
        else
          decode_record(start_byte, buffer, terminator)
        end
      end

      # Keeps consuming an oversized record without retaining its bytes and
      # raises once its end has been reached.
      def drain_oversized_record(start_byte, prefix, initial_terminator)
        terminator = initial_terminator

        until terminator
          byte = read_byte
          if byte.nil?
            terminator = :eof
            @final_eof = true
          elsif byte == RS
            @pending_byte = byte
            terminator = :rs
          elsif byte == LF
            terminator = :lf
          end
        end

        end_byte = terminator == :rs ? @offset - 1 : @offset
        raw = RS.dup << prefix
        raise RecordTooLargeError.new(
          "JSON sequence record at bytes #{start_byte}...#{end_byte} exceeds " \
          "max_record_size of #{@max_record_size} bytes",
          start_byte, end_byte, raw
        )
      end

      def decode_record(start_byte, framed_buffer, terminator)
        # RFC 7464: a JSON text consisting of the empty string (an RS that is
        # immediately followed by LF, RS or EOF) must be ignored.
        content =
          if terminator == :lf
            framed_buffer.byteslice(0, framed_buffer.bytesize - 1)
          else
            framed_buffer
          end
        return EMPTY_RECORD if content.nil? || content.empty?

        value =
          begin
            MultiJson.load(content, load_options_with_adapter)
          rescue StandardError => parse_error
            return bad_record(start_byte, framed_buffer, terminator, parse_error)
          end

        if terminator != :lf && value.is_a?(Numeric)
          # Without a terminating LF a bare number can always be the prefix of
          # a longer number, so it must not be treated as complete.
          truncation_error = MultiJson::ParseError.new(
            "top-level number without a terminating LF may be truncated"
          )
          return bad_record(start_byte, framed_buffer, terminator, truncation_error)
        end

        value
      end

      def bad_record(start_byte, framed_content, terminator, error)
        end_byte = terminator == :rs ? @offset - 1 : @offset
        raw = RS.dup << framed_content
        record = BadRecord.new(start_byte, end_byte, raw, error)
        @on_bad_record&.call(record)

        raise MalformedRecordError.build(record) if @on_invalid == :stop

        record
      end

      def read_byte
        if @pending_byte
          byte = @pending_byte
          @pending_byte = nil
          byte
        else
          byte = @io.read(1)
          byte = nil if byte == ""
          @offset += 1 unless byte.nil?
          byte
        end
      end

      def load_options_with_adapter
        @load_options.merge(adapter: @adapter)
      end
    end
  end
end
