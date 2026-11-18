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

    def test_load_attribute_is_attached_to_the_following_value
      assert_parses(
        RESP3::Attribute.new("value", { "key" => "attr" }),
        "|1\r\n+key\r\n+attr\r\n+value\r\n"
      )
    end

    def test_load_attribute_does_not_shadow_plain_map
      # A regular RESP3 map decodes to a Hash, while an attribute frame must
      # remain distinguishable from the value it decorates.
      assert_parses({ "key" => "attr" }, "%1\r\n+key\r\n+attr\r\n")

      raw_io = StringIO.new("|1\r\n+key\r\n+attr\r\n%1\r\n+k\r\n:1\r\n".b)
      io = build_io(raw_io)
      actual = RESP3.load(io)
      assert_instance_of RESP3::Attribute, actual
      assert_equal({ "key" => "attr" }, actual.attributes)
      assert_equal({ "k" => 1 }, actual.value)
      assert io.eof?, "Expected IO to be fully consumed: #{raw_io.read.inspect}"
    end

    def test_load_attribute_can_be_skipped
      payload = "|1\r\n+key\r\n+attr\r\n+value\r\n"
      raw_io = StringIO.new(payload.b)
      io = build_io(raw_io)

      assert_equal "value", RESP3.load(io, with_attributes: false)
      assert io.eof?, "Expected IO to be fully consumed: #{raw_io.read.inspect}"
    end

    def test_load_attribute_skip_is_recursive
      payload = "*2\r\n|1\r\n+first\r\n+a\r\n+one\r\n" \
        "%1\r\n+k\r\n|1\r\n+second\r\n+b\r\n:2\r\n"
      raw_io = StringIO.new(payload.b)
      io = build_io(raw_io)

      assert_equal ["one", { "k" => 2 }], RESP3.load(io, with_attributes: false)
      assert io.eof?, "Expected IO to be fully consumed: #{raw_io.read.inspect}"
    end

    def test_load_attribute_keys_keep_protocol_types
      assert_parses(
        RESP3::Attribute.new("value", { 42 => "integer", true => true, "blob" => 1 }),
        "|3\r\n:42\r\n+integer\r\n#t\r\n#t\r\n$4\r\nblob\r\n:1\r\n+value\r\n"
      )
    end

    def test_load_unknown_attribute_is_preserved_as_is
      assert_parses(
        RESP3::Attribute.new("value", { "unknown" => { "nested" => [1, 2] } }),
        "|1\r\n+unknown\r\n%1\r\n+nested\r\n*2\r\n:1\r\n:2\r\n+value\r\n"
      )
    end

    def test_load_attribute_on_nested_container
      assert_parses(
        [RESP3::Attribute.new("one", { "first" => "a" }), "two"],
        "*2\r\n|1\r\n+first\r\n+a\r\n+one\r\n+two\r\n"
      )
      assert_parses(
        { "k" => RESP3::Attribute.new(2, { "second" => "b" }) },
        "%1\r\n+k\r\n|1\r\n+second\r\n+b\r\n:2\r\n"
      )
      assert_parses(
        [RESP3::Attribute.new("x", { "in" => "set" })],
        "~1\r\n|1\r\n+in\r\n+set\r\n+x\r\n"
      )
    end

    def test_load_consecutive_attribute_blocks_are_merged
      assert_parses(
        RESP3::Attribute.new("value", { "a" => 1, "b" => 2 }),
        "|1\r\n+a\r\n:1\r\n|1\r\n+b\r\n:2\r\n+value\r\n"
      )
    end

    def test_load_consecutive_attribute_blocks_closest_to_value_wins
      assert_parses(
        RESP3::Attribute.new("value", { "a" => 2 }),
        "|1\r\n+a\r\n:1\r\n|1\r\n+a\r\n:2\r\n+value\r\n"
      )
    end

    def test_load_attribute_preceding_push
      assert_parses(
        RESP3::Attribute.new(["message", "hello"], { "a" => 1 }),
        "|1\r\n+a\r\n:1\r\n>2\r\n+message\r\n+hello\r\n"
      )
    end

    def test_load_attribute_preceding_push_in_middle_of_stream
      # The frame before the push belongs to the push, the frame after it
      # belongs to the next reply. Neither frame is a reply of its own.
      payload = "|1\r\n+push_tag\r\n:1\r\n" \
        ">2\r\n+message\r\n+hello\r\n" \
        "|1\r\n+value_tag\r\n:2\r\n" \
        "+value\r\n"
      raw_io = StringIO.new(payload.b)
      io = build_io(raw_io)

      push = RESP3.load(io)
      assert_equal RESP3::Attribute.new(["message", "hello"], { "push_tag" => 1 }), push
      assert_equal RESP3::Attribute.new("value", { "value_tag" => 2 }), RESP3.load(io)
      assert io.eof?, "Expected IO to be fully consumed: #{raw_io.read.inspect}"
    end

    def test_load_attribute_inside_push_element
      assert_parses(
        [RESP3::Attribute.new("message", { "a" => 1 }), "hello"],
        ">2\r\n|1\r\n+a\r\n:1\r\n+message\r\n+hello\r\n"
      )
    end

    def test_load_error_reply_with_attribute
      raw_io = StringIO.new("|1\r\n+key\r\n+value\r\n-ERR boom\r\n".b)
      io = build_io(raw_io)

      error = RESP3.load(io)
      assert_instance_of RedisClient::CommandError, error
      assert_equal({ "key" => "value" }, error.attributes)
      assert io.eof?, "Expected IO to be fully consumed: #{raw_io.read.inspect}"
    end

    def test_load_error_reply_attributes_are_kept_when_skipping
      raw_io = StringIO.new("|1\r\n+key\r\n+value\r\n-ERR boom\r\n".b)
      io = build_io(raw_io)

      error = RESP3.load(io, with_attributes: false)
      assert_instance_of RedisClient::CommandError, error
      assert_equal({ "key" => "value" }, error.attributes)
      assert io.eof?, "Expected IO to be fully consumed: #{raw_io.read.inspect}"
    end

    def test_load_sequential_replies_stay_aligned
      # One reply per `load` call, attributes never taking an extra slot.
      payload = "|1\r\n+ttl\r\n:10\r\n+one\r\n" \
        "|1\r\n+ttl\r\n:20\r\n+two\r\n" \
        "+three\r\n"
      raw_io = StringIO.new(payload.b)
      io = build_io(raw_io)

      assert_equal RESP3::Attribute.new("one", { "ttl" => 10 }), RESP3.load(io)
      assert_equal RESP3::Attribute.new("two", { "ttl" => 20 }), RESP3.load(io)
      assert_equal "three", RESP3.load(io)
      assert io.eof?, "Expected IO to be fully consumed: #{raw_io.read.inspect}"
    end

    def test_load_truncated_attribute_frame_is_not_dropped
      # Truncated frame: the attribute key is cut short.
      assert_raises EOFError do
        RESP3.load(build_io(StringIO.new("|1\r\n+key".b)))
      end

      # Truncated frame: the declared pair count is not reached.
      assert_raises EOFError do
        RESP3.load(build_io(StringIO.new("|2\r\n+key\r\n:1\r\n".b)))
      end

      # Complete frame but the decorated value never arrives: the reply is
      # incomplete, so it must not be reported as a complete reply.
      assert_raises EOFError do
        RESP3.load(build_io(StringIO.new("|1\r\n+key\r\n:1\r\n".b)))
      end
    end

    def test_unwrap_removes_nested_attributes
      value = RESP3::Attribute.new(
        [RESP3::Attribute.new("one", { "a" => 1 }), { "k" => RESP3::Attribute.new(2, { "b" => 2 }) }],
        { "top" => true }
      )

      assert_equal ["one", { "k" => 2 }], RESP3.unwrap(value)
      assert_equal ["one", { "k" => 2 }], value.unwrap
    end

    private

    def build_io(raw_io)
      RedisClient::RubyConnection::BufferedIO.new(raw_io, read_timeout: 1, write_timeout: 1)
    end

    def assert_parses(expected, payload, io_class: StringIO)
      raw_io = io_class.new(payload.b)
      io = build_io(raw_io)
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
