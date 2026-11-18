# frozen_string_literal: true

require "test_helper"

require "redis_client/ruby_connection/buffered_io"
require "redis_client/ruby_connection/resp3"

class RedisClient
  class RESP3Test < RedisClientTestCase
    class StringIO < ::StringIO
      def skip(offset)
        seek(offset, IO::SEEK_CUR)
        nil
      end
    end

    class ChunkedStringIO < StringIO
      def read_nonblock(maxlen, *args, **kwargs)
        super([maxlen, 1024].min, *args, **kwargs)
      end
    end

    def test_dump_mixed_encoding
      assert_dumps ["SET", "fée", "\xC6bIJ"], "*3\r\n$3\r\nSET\r\n$4\r\nfée\r\n$4\r\n\xC6bIJ\r\n"
    end

    def test_dump_string
      assert_dumps ["Hello World!"], "*1\r\n$12\r\nHello World!\r\n"
      assert_dumps ["Hello\r\nWorld!"], "*1\r\n$13\r\nHello\r\nWorld!\r\n"
    end

    def test_dump_integer
      assert_dumps [42], "*1\r\n$2\r\n42\r\n"
    end

    def test_dump_true
      assert_raises TypeError do
        RESP3.dump([true])
      end
    end

    def test_dump_false
      assert_raises TypeError do
        RESP3.dump([false])
      end
    end

    def test_dump_nil
      assert_raises TypeError do
        RESP3.dump([nil])
      end
    end

    def test_dump_big_integer
      assert_dumps [1_000_000_000_000_000_000_000], "*1\r\n$22\r\n1000000000000000000000\r\n"
    end

    def test_dump_float
      assert_dumps [42.42], "*1\r\n$5\r\n42.42\r\n"
      # TODO: What about NaN, Infinity, -Infinity
    end

    def test_dump_array
      assert_dumps ["PRINT", [1, 2, 3]], "*4\r\n$5\r\nPRINT\r\n$1\r\n1\r\n$1\r\n2\r\n$1\r\n3\r\n"
    end

    def test_dump_hash
      assert_dumps(["PRINT", { 'first' => 1, 'second' => 2 }], "*5\r\n$5\r\nPRINT\r\n$5\r\nfirst\r\n$1\r\n1\r\n$6\r\nsecond\r\n$1\r\n2\r\n")
    end

    def test_dump_subclasses
      my_string_class = Class.new(String)
      assert_dumps [my_string_class.new("Hello")], "*1\r\n$5\r\nHello\r\n"
    end

    def test_load_blob_string
      assert_parses "Hello World!", "$12\r\nHello World!\r\n"
    end

    def test_load_simple_string
      assert_parses "Hello World!", "+Hello World!\r\n"
    end

    def test_load_error
      assert_parses CommandError.parse("SOMEERROR"), "-SOMEERROR\r\n"
    end

    def test_load_integer
      assert_parses 42, ":42\r\n"
      assert_parses(-42, ":-42\r\n")
      assert_parses 3_492_890_328_409_238_509_324_850_943_850_943_825_024_385, "(3492890328409238509324850943850943825024385\r\n"
    end

    def test_load_double
      assert_parses 42.42, ",42.42\r\n"
      assert_parses(-42.42, ",-42.42\r\n")
      assert_parses Float::INFINITY, ",inf\r\n"
      assert_parses(-Float::INFINITY, ",-inf\r\n")
      assert_parses(nil, ",nan\r\n") do |actual|
        assert_predicate actual, :nan?
      end
    end

    def test_load_null
      assert_parses nil, "_\r\n"
    end

    def test_load_boolean
      assert_parses true, "#t\r\n"
      assert_parses false, "#f\r\n"
    end

    def test_load_array
      assert_parses [1, 2, 3], "*3\r\n:1\r\n:2\r\n:3\r\n"
    end

    def test_load_multibyte_chars
      # Check that the buffer is properly operating over bytes and not characters
      assert_parses ["€™€™", 2], "*2\r\n$12\r\n€™€™\r\n:2\r\n"
    end

    def test_load_set
      assert_parses ['orange', 'apple', true, 100, 999], "~5\r\n+orange\r\n+apple\r\n#t\r\n:100\r\n:999\r\n"
    end

    def test_load_map
      assert_parses({ 'first' => 1, 'second' => 2 }, "%2\r\n+first\r\n:1\r\n+second\r\n:2\r\n")
    end

    def test_load_missing_type_byte
      error = assert_raises RESP3::Error do
        RESP3.load(StringIO.new("".b))
      end
      assert_includes error.message, "end of stream"
    end

    def test_load_large_map
      entries = 100_000
      payload = +"%#{entries}\r\n"
      entries.times do |i|
        payload << "+#{i}\r\n:#{i}\r\n"
      end
      expected = entries.times.each_with_object({}) { |i, h| h[i.to_s] = i }
      assert_parses(expected, payload)
    end

    def test_load_blob_string_across_multiple_reads
      # Each read returns at most 1KB, so the blob is assembled from many reads
      value = Array.new(4_000) { |i| "#{i}," }.join
      assert_parses value, "$#{value.bytesize}\r\n#{value}\r\n", io_class: ChunkedStringIO
    end

    def test_load_verbatim_string
      assert_parses "Some string", "=15\r\ntxt:Some string\r\n"
    end

    def test_load_streamed_blob_string
      assert_parses "Hello World!", "$?\r\n;5\r\nHello\r\n;7\r\n World!\r\n;0\r\n"
    end

    def test_load_streamed_blob_string_empty
      assert_parses "", "$?\r\n;0\r\n"
    end

    def test_load_streamed_blob_string_across_multiple_reads
      assert_parses "Hello World!", "$?\r\n;5\r\nHello\r\n;7\r\n World!\r\n;0\r\n", io_class: ChunkedStringIO
    end

    def test_load_streamed_blob_string_multibyte_split_across_chunks
      # The UTF-8 encoding of € is E2 82 AC, split here across two chunks
      assert_parses "a€b", "$?\r\n;2\r\na\xE2\r\n;3\r\n\x82\xACb\r\n;0\r\n".b
    end

    def test_load_streamed_blob_string_invalid_encoding
      assert_parses "\xFF\xFE".b, "$?\r\n;1\r\n\xFF\r\n;1\r\n\xFE\r\n;0\r\n".b do |actual|
        assert_equal Encoding::BINARY, actual.encoding
      end
    end

    def test_load_streamed_blob_string_with_block
      chunks = []
      raw_io = StringIO.new("$?\r\n;5\r\nHello\r\n;7\r\n World!\r\n;0\r\n".b)
      io = RedisClient::RubyConnection::BufferedIO.new(raw_io, read_timeout: 1, write_timeout: 1)
      result = RESP3.load(io) { |chunk| chunks << chunk }

      assert_nil result
      assert_equal ["Hello", " World!"], chunks
      assert_equal [Encoding::BINARY, Encoding::BINARY], chunks.map(&:encoding)
      assert io.eof?, "Expected IO to be fully consumed: #{raw_io.read.inspect}"
    end

    def test_load_streamed_blob_string_inside_aggregate
      assert_parses ["ab", 1], "*2\r\n$?\r\n;2\r\nab\r\n;0\r\n:1\r\n"
    end

    def test_load_streamed_blob_string_terminator_does_not_swallow_next_reply
      raw_io = StringIO.new("$?\r\n;1\r\na\r\n;0\r\n+OK\r\n".b)
      io = RedisClient::RubyConnection::BufferedIO.new(raw_io, read_timeout: 1, write_timeout: 1)
      assert_equal "a", RESP3.load(io)
      assert_equal "OK", RESP3.load(io)
      assert io.eof?, "Expected IO to be fully consumed: #{raw_io.read.inspect}"
    end

    def test_load_streamed_blob_string_truncated
      error = assert_raises EOFError do
        RESP3.load(buffered_io("$?\r\n;5\r\nHel"))
      end
      assert_equal "EOFError", error.message
    end

    def test_load_streamed_blob_string_truncated_before_terminator
      assert_raises EOFError do
        RESP3.load(buffered_io("$?\r\n;5\r\nHello\r\n"))
      end
    end

    def test_load_streamed_blob_string_invalid_chunk_type
      error = assert_raises RESP3::SyntaxError do
        RESP3.load(buffered_io("$?\r\n+5\r\nHello\r\n;0\r\n"))
      end
      assert_includes error.message, "Expected streamed string chunk"
    end

    def test_load_streamed_blob_string_invalid_chunk_size
      error = assert_raises RESP3::SyntaxError do
        RESP3.load(buffered_io("$?\r\n;-1\r\n"))
      end
      assert_includes error.message, "Invalid streamed string chunk size"
    end

    def test_load_streamed_array
      assert_parses [1, 2, 3], "*?\r\n:1\r\n:2\r\n:3\r\n.\r\n"
    end

    def test_load_streamed_array_empty
      assert_parses [], "*?\r\n.\r\n"
    end

    def test_load_streamed_set
      assert_parses ["a", "b"], "~?\r\n+a\r\n+b\r\n.\r\n"
    end

    def test_load_streamed_map
      assert_parses({ "a" => 1, "b" => 2 }, "%?\r\n+a\r\n:1\r\n+b\r\n:2\r\n.\r\n")
    end

    def test_load_streamed_nested_aggregates
      assert_parses [[1], "x", { "k" => 9 }],
        "*?\r\n*?\r\n:1\r\n.\r\n$?\r\n;1\r\nx\r\n;0\r\n%?\r\n+k\r\n:9\r\n.\r\n.\r\n"
    end

    def test_load_streamed_aggregate_terminator_does_not_swallow_next_reply
      raw_io = StringIO.new("*?\r\n:1\r\n.\r\n:2\r\n".b)
      io = RedisClient::RubyConnection::BufferedIO.new(raw_io, read_timeout: 1, write_timeout: 1)
      assert_equal [1], RESP3.load(io)
      assert_equal 2, RESP3.load(io)
      assert io.eof?, "Expected IO to be fully consumed: #{raw_io.read.inspect}"
    end

    def test_load_streamed_aggregate_truncated
      assert_raises EOFError do
        RESP3.load(buffered_io("*?\r\n:1\r\n"))
      end
    end

    def test_load_streamed_aggregate_invalid_terminator
      error = assert_raises RESP3::SyntaxError do
        RESP3.load(buffered_io("*?\r\n:1\r\n.X\r\n"))
      end
      assert_includes error.message, "Expected CRLF"
    end

    private

    def buffered_io(payload, io_class: StringIO)
      RedisClient::RubyConnection::BufferedIO.new(io_class.new(payload.b), read_timeout: 1, write_timeout: 1)
    end

    def assert_parses(expected, payload, io_class: StringIO)
      raw_io = io_class.new(payload.b)
      io = RedisClient::RubyConnection::BufferedIO.new(raw_io, read_timeout: 1, write_timeout: 1)
      actual = RESP3.load(io)
      if block_given?
        yield actual
      elsif expected.nil?
        assert_nil actual
      else
        assert_equal(expected, actual)
      end

      assert io.eof?, "Expected IO to be fully consumed: #{raw_io.read.inspect}"
    end

    def assert_dumps(payload, expected)
      buffer = RESP3.dump(payload)
      assert_equal expected.b, buffer
      assert_equal Encoding::BINARY, buffer.encoding
    end
  end
end
