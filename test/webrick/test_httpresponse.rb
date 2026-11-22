# frozen_string_literal: false
require "webrick"
require "stringio"
require "net/http"
require "tmpdir"

module WEBrick
  class TestHTTPResponse < Test::Unit::TestCase
    class FakeLogger
      attr_reader :messages

      def initialize
        @messages = []
      end

      def warn msg
        @messages << msg
      end
    end

    attr_reader :config, :logger, :res

    def setup
      super
      @logger          = FakeLogger.new
      @config          = Config::HTTP
      @config[:Logger] = logger
      @res             = HTTPResponse.new config
      @res.keep_alive  = true
    end

    def test_response_body_not_frozen
      refute @res.body.frozen?
    end

    def test_prevent_response_splitting_headers_crlf
      res['X-header'] = "malicious\r\nCookie: cracked_indicator_for_test"
      io = StringIO.new
      res.send_response io
      io.rewind
      res = Net::HTTPResponse.read_new(Net::BufferedIO.new(io))
      assert_equal '500', res.code
      refute_match 'cracked_indicator_for_test', io.string
    end

    def test_prevent_response_splitting_cookie_headers_crlf
      user_input = "malicious\r\nCookie: cracked_indicator_for_test"
      res.cookies << WEBrick::Cookie.new('author', user_input)
      io = StringIO.new
      res.send_response io
      io.rewind
      res = Net::HTTPResponse.read_new(Net::BufferedIO.new(io))
      assert_equal '500', res.code
      refute_match 'cracked_indicator_for_test', io.string
    end

    def test_prevent_response_splitting_headers_cr
      res['X-header'] = "malicious\rCookie: cracked_indicator_for_test"
      io = StringIO.new
      res.send_response io
      io.rewind
      res = Net::HTTPResponse.read_new(Net::BufferedIO.new(io))
      assert_equal '500', res.code
      refute_match 'cracked_indicator_for_test', io.string
    end

    def test_prevent_response_splitting_cookie_headers_cr
      user_input = "malicious\rCookie: cracked_indicator_for_test"
      res.cookies << WEBrick::Cookie.new('author', user_input)
      io = StringIO.new
      res.send_response io
      io.rewind
      res = Net::HTTPResponse.read_new(Net::BufferedIO.new(io))
      assert_equal '500', res.code
      refute_match 'cracked_indicator_for_test', io.string
    end

    def test_prevent_response_splitting_headers_lf
      res['X-header'] = "malicious\nCookie: cracked_indicator_for_test"
      io = StringIO.new
      res.send_response io
      io.rewind
      res = Net::HTTPResponse.read_new(Net::BufferedIO.new(io))
      assert_equal '500', res.code
      refute_match 'cracked_indicator_for_test', io.string
    end

    def test_prevent_response_splitting_cookie_headers_lf
      user_input = "malicious\nCookie: cracked_indicator_for_test"
      res.cookies << WEBrick::Cookie.new('author', user_input)
      io = StringIO.new
      res.send_response io
      io.rewind
      res = Net::HTTPResponse.read_new(Net::BufferedIO.new(io))
      assert_equal '500', res.code
      refute_match 'cracked_indicator_for_test', io.string
    end

    def test_set_redirect_response_splitting
      url = "malicious\r\nCookie: cracked_indicator_for_test"
      assert_raise(URI::InvalidURIError) do
        res.set_redirect(WEBrick::HTTPStatus::MultipleChoices, url)
      end
    end

    def test_set_redirect_html_injection
      url = 'http://example.com////?a</a><head></head><body><img src=1></body>'
      assert_raise(WEBrick::HTTPStatus::MultipleChoices) do
        res.set_redirect(WEBrick::HTTPStatus::MultipleChoices, url)
      end
      res.status = 300
      io = StringIO.new
      res.send_response(io)
      io.rewind
      res = Net::HTTPResponse.read_new(Net::BufferedIO.new(io))
      assert_equal '300', res.code
      refute_match(/<img/, io.string)
    end

    def test_304_does_not_log_warning
      res.status      = 304
      res.setup_header
      assert_equal 0, logger.messages.length
    end

    def test_204_does_not_log_warning
      res.status      = 204
      res.setup_header

      assert_equal 0, logger.messages.length
    end

    def test_1xx_does_not_log_warnings
      res.status      = 105
      res.setup_header

      assert_equal 0, logger.messages.length
    end

    def test_200_chunked_does_not_set_content_length
      res.chunked     = false
      res["Transfer-Encoding"] = 'chunked'
      res.setup_header
      assert_nil res.header.fetch('content-length', nil)
    end

    def test_send_body_io
      IO.pipe {|body_r, body_w|
        body_w.write 'hello'
        body_w.close

        @res.body = body_r

        IO.pipe {|r, w|

          @res.send_body w

          w.close

          assert_equal 'hello', r.read
        }
      }
      assert_equal 0, logger.messages.length
    end

    def test_send_body_string
      @res.body = 'hello'

      IO.pipe {|r, w|
        @res.send_body w

        w.close

        assert_equal 'hello', r.read
      }
      assert_equal 0, logger.messages.length
    end

    def test_send_body_string_io
      @res.body = StringIO.new 'hello'

      IO.pipe {|r, w|
        @res.send_body w

        w.close

        assert_equal 'hello', r.read
      }
      assert_equal 0, logger.messages.length
    end

    def test_send_body_io_chunked
      @res.chunked = true

      IO.pipe {|body_r, body_w|

        body_w.write 'hello'
        body_w.close

        @res.body = body_r

        IO.pipe {|r, w|
          @res.send_body w

          w.close

          r.binmode
          assert_equal "5\r\nhello\r\n0\r\n\r\n", r.read
        }
      }
      assert_equal 0, logger.messages.length
    end

    def test_send_body_string_chunked
      @res.chunked = true

      @res.body = 'hello'

      IO.pipe {|r, w|
        @res.send_body w

        w.close

        r.binmode
        assert_equal "5\r\nhello\r\n0\r\n\r\n", r.read
      }
      assert_equal 0, logger.messages.length
    end

    def test_send_body_string_io_chunked
      @res.chunked = true

      @res.body = StringIO.new 'hello'

      IO.pipe {|r, w|
        @res.send_body w

        w.close

        r.binmode
        assert_equal "5\r\nhello\r\n0\r\n\r\n", r.read
      }
      assert_equal 0, logger.messages.length
    end

    def test_send_body_proc
      @res.body = Proc.new { |out| out.write('hello') }
      IO.pipe do |r, w|
        @res.send_body(w)
        w.close
        r.binmode
        assert_equal 'hello', r.read
      end
      assert_equal 0, logger.messages.length
    end

    def test_send_body_proc_chunked
      @res.body = Proc.new { |out| out.write('hello') }
      @res.chunked = true
      IO.pipe do |r, w|
        @res.send_body(w)
        w.close
        r.binmode
        assert_equal "5\r\nhello\r\n0\r\n\r\n", r.read
      end
      assert_equal 0, logger.messages.length
    end

    def test_send_body_proc_upgrade
      @res.body = Proc.new { |out| out.write('hello'); out.close }
      @res.upgrade!("text")

      IO.pipe do |r, w|
        @res.send_response(w)
        w.close
        assert_match(/Connection: upgrade\r\nUpgrade: text\r\n\r\nhello/, r.read)
      end
      assert_empty logger.messages
    end

    def test_send_body_proc_stream
      @res.body = Proc.new do |socket|
        chunk = socket.read
        socket.write(chunk)
        socket.close
      end

      UNIXSocket.pair do |s1, s2|
        thread = Thread.new do
          @res.send_response(s1)
        end

        s2.write("hello")
        s2.close_write
        chunk = s2.read
        assert_match(/Connection: close\r\n\r\nhello/, chunk)
        s2.close

        thread.join
      end
      assert_empty logger.messages
    end

    # A read-only stream that can only be consumed forwards.  It has no
    # #seek, so it stands in for pipes and other non-seekable sources.
    class FakeReadOnlyStream
      attr_reader :pos

      def initialize(data, size: data.bytesize)
        @data = data.b
        @pos = 0
        @size = size
      end

      def size
        @size
      end

      def eof?
        @pos >= @data.bytesize
      end

      def read(length = nil, outbuf = nil)
        return nil if eof?
        chunk = @data.byteslice(@pos, length || remaining)
        @pos += chunk.bytesize
        outbuf ? outbuf.replace(chunk) : chunk
      end

      def readpartial(length, outbuf = nil)
        raise EOFError if eof?
        read([length, remaining].min, outbuf)
      end

      private

      def remaining
        @data.bytesize - @pos
      end
    end

    # A non-seekable stream whose total length cannot be discovered either.
    class FakeUnknownLengthStream < FakeReadOnlyStream
      undef_method :size
    end

    DATA = "0123456789ABCDEF"

    def ranged_response(method, body, range)
      res = HTTPResponse.new config
      res.request_method = method
      res.keep_alive = true
      res["content-type"] = "text/plain"
      res.body = body
      res.body_range = range
      res
    end

    def send_body_to_string(res)
      out = StringIO.new
      out.binmode
      res.setup_header
      res.send_body(out)
      out.string
    end

    def test_body_range_current_position_get
      io = StringIO.new(DATA)
      io.pos = 4
      res = ranged_response("GET", io, :current)

      body = send_body_to_string(res)

      assert_equal "12", res["content-length"]
      assert_equal 12, res.content_length
      assert_equal DATA.byteslice(4..-1), body
      assert_equal 12, res.sent_size
      assert_equal DATA.bytesize, io.pos
    end

    def test_body_range_current_position_head
      io = StringIO.new(DATA)
      io.pos = 4
      res = ranged_response("HEAD", io, :current)

      body = send_body_to_string(res)

      assert_equal "12", res["content-length"]
      assert_equal "", body
      assert_equal 0, res.sent_size
      assert_equal 4, io.pos, "HEAD must not advance the body"
    end

    def test_body_range_current_position_fake_stream_get
      stream = FakeReadOnlyStream.new(DATA)
      stream.read(4)
      res = ranged_response("GET", stream, :current)

      body = send_body_to_string(res)

      assert_equal "12", res["content-length"]
      assert_equal DATA.byteslice(4..-1), body
      assert_equal 12, res.sent_size
      assert_equal DATA.bytesize, stream.pos
    end

    def test_body_range_current_position_fake_stream_head
      stream = FakeReadOnlyStream.new(DATA)
      stream.read(4)
      res = ranged_response("HEAD", stream, :current)

      body = send_body_to_string(res)

      assert_equal "12", res["content-length"]
      assert_equal "", body
      assert_equal 0, res.sent_size
      assert_equal 4, stream.pos, "HEAD must not advance the body"
    end

    def test_body_range_integer_offset_get
      io = StringIO.new(DATA)
      res = ranged_response("GET", io, 6)

      body = send_body_to_string(res)

      assert_equal "10", res["content-length"]
      assert_equal DATA.byteslice(6..-1), body
      assert_equal 10, res.sent_size
    end

    def test_body_range_byte_range_get_and_head
      io = StringIO.new(DATA)
      res = ranged_response("GET", io, 5..9)

      body = send_body_to_string(res)

      assert_equal "5", res["content-length"]
      assert_equal DATA.byteslice(5..9), body
      assert_equal 5, res.sent_size

      io = StringIO.new(DATA)
      res = ranged_response("HEAD", io, 5..9)

      body = send_body_to_string(res)

      assert_equal "5", res["content-length"]
      assert_equal "", body
      assert_equal 0, res.sent_size
      assert_equal 0, io.pos
    end

    def test_body_range_offset_ahead_of_fake_stream_skips_forward
      stream = FakeReadOnlyStream.new(DATA)
      stream.read(4)
      res = ranged_response("GET", stream, 8)

      body = send_body_to_string(res)

      assert_equal "8", res["content-length"]
      assert_equal DATA.byteslice(8..-1), body
      assert_equal 8, res.sent_size
      assert_equal DATA.bytesize, stream.pos
    end

    def test_body_range_offset_behind_fake_stream_starts_at_cursor
      stream = FakeReadOnlyStream.new(DATA)
      stream.read(4)
      res = ranged_response("GET", stream, 2)

      body = send_body_to_string(res)

      assert_equal "12", res["content-length"]
      assert_equal DATA.byteslice(4..-1), body,
                   "a non-seekable stream cannot rewind"
      assert_equal 12, res.sent_size
    end

    def test_body_range_early_eof_closes_connection
      stream = FakeReadOnlyStream.new(DATA.byteslice(0, 8), size: DATA.bytesize)
      res = ranged_response("GET", stream, :current)

      body = send_body_to_string(res)

      assert_equal DATA.bytesize.to_s, res["content-length"]
      assert_equal DATA.byteslice(0, 8), body
      assert_equal 8, res.sent_size
      assert_equal "close", res["connection"]
      refute res.keep_alive?
    end

    def test_body_range_unknown_length_is_close_delimited
      stream = FakeUnknownLengthStream.new(DATA)
      res = ranged_response("GET", stream, :current)

      body = send_body_to_string(res)

      assert_nil res["content-length"]
      assert_equal "close", res["connection"]
      assert_equal DATA, body
      assert_equal DATA.bytesize, res.sent_size
    end

    def test_body_range_rejects_invalid_values
      @res.body = StringIO.new(DATA)
      assert_raise(ArgumentError) { @res.body_range = -1 }
      assert_raise(ArgumentError) { @res.body_range = 9..1 }
      assert_raise(ArgumentError) { @res.body_range = 1...1 }
      assert_raise(ArgumentError) { @res.body_range = "current" }
    end

    def test_body_range_cleared_when_body_is_not_io
      @res.body = StringIO.new(DATA)
      @res.body_range = :current
      @res.body = "hello"
      assert_nil @res.body_range
    end

    def test_body_range_current_position_seekable_file
      Dir.mktmpdir do |dir|
        path = File.join(dir, "data.bin")
        File.binwrite(path, DATA)
        io = File.open(path, "rb")
        io.read(4)
        begin
          res = ranged_response("GET", io, :current)

          body = send_body_to_string(res)

          assert_equal "12", res["content-length"]
          assert_equal DATA.byteslice(4..-1), body
          assert_equal 12, res.sent_size
        ensure
          io.close unless io.closed?
        end
      end
    end

    def test_body_range_integer_can_seek_backwards_on_file
      Dir.mktmpdir do |dir|
        path = File.join(dir, "data.bin")
        File.binwrite(path, DATA)
        io = File.open(path, "rb")
        io.read(4)
        begin
          res = ranged_response("GET", io, 0)

          body = send_body_to_string(res)

          assert_equal DATA.bytesize.to_s, res["content-length"]
          assert_equal DATA, body
        ensure
          io.close unless io.closed?
        end
      end
    end

    def test_body_range_clamped_to_known_size
      io = StringIO.new(DATA)
      res = ranged_response("GET", io, 10..100)

      body = send_body_to_string(res)

      assert_equal "6", res["content-length"]
      assert_equal DATA.byteslice(10..-1), body
    end

    def test_body_range_content_length_matches_written_bytes
      io = StringIO.new(DATA)
      io.pos = 4
      res = ranged_response("GET", io, :current)

      body = send_body_to_string(res)

      assert_equal res.content_length, body.bytesize
      assert_equal res.content_length, res.sent_size
    end

    def test_body_range_overrides_stale_content_length
      io = StringIO.new(DATA)
      io.pos = 4
      res = ranged_response("GET", io, :current)
      res["content-length"] = "999"

      body = send_body_to_string(res)

      assert_equal "12", res["content-length"]
      assert_equal 12, body.bytesize
    end

    def test_body_range_chunked
      io = StringIO.new(DATA)
      io.pos = 4
      res = ranged_response("GET", io, :current)
      res.chunked = true

      body = send_body_to_string(res)

      assert_nil res["content-length"]
      assert_equal "c\r\n#{DATA.byteslice(4..-1)}\r\n0\r\n\r\n".b, body
      assert_equal 12, res.sent_size
    end

    def test_body_range_chunked_truncated_closes_connection
      stream = FakeReadOnlyStream.new(DATA.byteslice(0, 8), size: DATA.bytesize)
      res = ranged_response("GET", stream, :current)
      res.chunked = true

      send_body_to_string(res)

      assert_equal "close", res["connection"]
      refute res.keep_alive?
    end

    def test_set_error
      status = 400
      message = 'missing attribute'
      @res.status = status
      error = WEBrick::HTTPStatus[status].new(message)
      body = @res.set_error(error)
      assert_match(/#{@res.reason_phrase}/, body)
      assert_match(/#{message}/, body)
    end

    def test_no_extraneous_space
      [200, 300, 400, 500].each do |status|
        @res.status = status
        assert_match(/\S\r\n/, @res.status_line)
      end
    end
  end
end
