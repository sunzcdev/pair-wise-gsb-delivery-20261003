# frozen_string_literal: true
#
# httpresponse.rb -- HTTPResponse Class
#
# Author: IPR -- Internet Programming with Ruby -- writers
# Copyright (c) 2000, 2001 TAKAHASHI Masayoshi, GOTOU Yuuzou
# Copyright (c) 2002 Internet Programming with Ruby writers. All rights
# reserved.
#
# $IPR: httpresponse.rb,v 1.45 2003/07/11 11:02:25 gotoyuzo Exp $

require 'time'
require 'uri'
require_relative 'httpversion'
require_relative 'htmlutils'
require_relative 'httputils'
require_relative 'httpstatus'

module WEBrick
  ##
  # An HTTP response.  This is filled in by the service or do_* methods of a
  # WEBrick HTTP Servlet.

  class HTTPResponse
    class InvalidHeader < StandardError
    end

    ##
    # HTTP Response version

    attr_reader :http_version

    ##
    # Response status code (200)

    attr_reader :status

    ##
    # Response header

    attr_reader :header

    ##
    # Response cookies

    attr_reader :cookies

    ##
    # Response reason phrase ("OK")

    attr_accessor :reason_phrase

    ##
    # Body may be:
    # * a String;
    # * an IO-like object that responds to +#read+ and +#readpartial+;
    # * a Proc-like object that responds to +#call+.
    #
    # In the latter case, either #chunked= should be set to +true+,
    # or <code>header['content-length']</code> explicitly provided.
    # Example:
    #
    #   server.mount_proc '/' do |req, res|
    #     res.chunked = true
    #     # or
    #     # res.header['content-length'] = 10
    #     res.body = proc { |out| out.write(Time.now.to_s) }
    #   end
    #
    # When the body is an IO, #body_range may be used to send only one part
    # of it.  See #body_range for details.

    attr_reader :body

    ##
    # Sets the response body.  A declared #body_range only applies to an
    # IO-like body; assigning anything else clears it.

    def body=(body)
      @body_send_window = nil
      @body_range = nil unless body.nil? || body.respond_to?(:readpartial)
      @body = body
    end

    ##
    # The part of an IO-like body that will be sent, or +nil+ to use the
    # default behaviour.
    #
    # A servlet may hand the response an IO it has already read from, or that
    # should only be sent partially.  Assigning #body_range makes the
    # Content-Length, the offset at which the transfer starts, and the bytes
    # actually written all come from the same view of the body:
    #
    # * +:current+ - send from the IO's current position to the end of the
    #   body;
    # * an Integer - send from that byte offset to the end of the body;
    # * a Range - send that inclusive byte range.
    #
    # The total size is taken from a regular file's stat, or from the body's
    # +size+ method when one is available.  A body whose length cannot be
    # determined is sent until EOF and the connection is closed.  A body that
    # ends before the declared length is wrapped up by closing the connection
    # instead of leaving the client waiting for bytes that will never arrive.
    #
    # HEAD responses use the same Content-Length as a GET response and do not
    # read the body, so only the cursor of a ranged body is observed, never
    # advanced.  The response still closes the body it was given.
    #
    # Example:
    #
    #   io = File.open(path, "rb")
    #   io.seek(1024, IO::SEEK_SET)
    #   res['content-type'] = "application/octet-stream"
    #   res.body = io
    #   res.body_range = :current

    attr_reader :body_range

    ##
    # Declares which part of an IO-like body to send.  +range+ may be
    # +:current+, a non-negative Integer, a Range of non-negative Integers,
    # or +nil+ to restore the default behaviour.

    def body_range=(range)
      case range
      when nil, :current
        # accepted as-is
      when Integer
        raise ArgumentError, "negative body offset: #{range}" if range < 0
      when Range
        first = range.begin
        last = range.end
        last -= 1 if last.is_a?(Integer) && range.exclude_end?
        unless first.is_a?(Integer) && last.is_a?(Integer) &&
               first >= 0 && last >= first
          raise ArgumentError, "invalid body range: #{range.inspect}"
        end
      else
        raise ArgumentError, "invalid body range: #{range.inspect}"
      end
      @body_range = range
      @body_send_window = nil
      range
    end

    ##
    # Request method for this response

    attr_accessor :request_method

    ##
    # Request URI for this response

    attr_accessor :request_uri

    ##
    # Request HTTP version for this response

    attr_accessor :request_http_version

    ##
    # Filename of the static file in this response.  Only used by the
    # FileHandler servlet.

    attr_accessor :filename

    ##
    # Is this a keep-alive response?

    attr_accessor :keep_alive

    ##
    # Configuration for this response

    attr_reader :config

    ##
    # Bytes sent in this response

    attr_reader :sent_size

    ##
    # Set the response body proc as an streaming/upgrade response.

    attr_accessor :upgrade

    ##
    # Creates a new HTTP response object.  WEBrick::Config::HTTP is the
    # default configuration.

    def initialize(config)
      @config = config
      @buffer_size = config[:OutputBufferSize]
      @logger = config[:Logger]
      @header = Hash.new
      @status = HTTPStatus::RC_OK
      @reason_phrase = nil
      @http_version = HTTPVersion::convert(@config[:HTTPVersion])
      @body = +""
      @keep_alive = true
      @cookies = []
      @request_method = nil
      @request_uri = nil
      @request_http_version = @http_version  # temporary
      @chunked = false
      @filename = nil
      @sent_size = 0
      @bodytempfile = nil
      @body_range = nil
      @body_send_window = nil
    end

    ##
    # The response's HTTP status line

    def status_line
      "HTTP/#@http_version #@status #@reason_phrase".rstrip << CRLF
    end

    ##
    # Sets the response's status to the +status+ code

    def status=(status)
      @status = status
      @reason_phrase = HTTPStatus::reason_phrase(status)
    end

    ##
    # Retrieves the response header +field+

    def [](field)
      @header[field.downcase]
    end

    ##
    # Sets the response header +field+ to +value+

    def []=(field, value)
      @chunked = value.to_s.downcase == 'chunked' if field.downcase == 'transfer-encoding'
      @header[field.downcase] = value.to_s
    end

    ##
    # The content-length header

    def content_length
      if len = self['content-length']
        return Integer(len)
      end
    end

    ##
    # Sets the content-length header to +len+

    def content_length=(len)
      self['content-length'] = len.to_s
    end

    ##
    # The content-type header

    def content_type
      self['content-type']
    end

    ##
    # Sets the content-type header to +type+

    def content_type=(type)
      self['content-type'] = type
    end

    ##
    # Iterates over each header in the response

    def each
      @header.each{|field, value|  yield(field, value) }
    end

    ##
    # Will this response body be returned using chunked transfer-encoding?

    def chunked?
      @chunked
    end

    ##
    # Enables chunked transfer encoding.

    def chunked=(val)
      @chunked = val ? true : false
    end

    ##
    # Will this response's connection be kept alive?

    def keep_alive?
      @keep_alive
    end

    ##
    # Sets the response to be a streaming/upgrade response.
    # This will disable keep-alive and chunked transfer encoding.

    def upgrade!(protocol)
      @upgrade = protocol
      @keep_alive = false
      @chunked = false
    end

    ##
    # Sends the response on +socket+

    def send_response(socket) # :nodoc:
      begin
        setup_header()
        send_header(socket)
        send_body(socket)
      rescue Errno::EPIPE, Errno::ECONNRESET, Errno::ENOTCONN => ex
        @logger.debug(ex)
        @keep_alive = false
      rescue Exception => ex
        @logger.error(ex)
        @keep_alive = false
      end
    end

    ##
    # Sets up the headers for sending

    def setup_header() # :nodoc:
      @reason_phrase    ||= HTTPStatus::reason_phrase(@status)
      @header['server'] ||= @config[:ServerSoftware]
      @header['date']   ||= Time.now.httpdate

      if @upgrade
        @header['connection'] = 'upgrade'
        @header['upgrade'] = @upgrade
        @keep_alive = false

        return
      end

      # HTTP/0.9 features
      if @request_http_version < "1.0"
        @http_version = HTTPVersion.new("0.9")
        @keep_alive = false
      end

      # HTTP/1.0 features
      if @request_http_version < "1.1"
        if chunked?
          @chunked = false
          ver = @request_http_version.to_s
          msg = "chunked is set for an HTTP/#{ver} request. (ignored)"
          @logger.warn(msg)
        end
      end

      # Determine the message length (RFC2616 -- 4.4 Message Length)
      if @status == 304 || @status == 204 || HTTPStatus::info?(@status)
        @header.delete('content-length')
        @body = ""
      elsif chunked?
        @header["transfer-encoding"] = "chunked"
        @header.delete('content-length')
      elsif %r{^multipart/byteranges} =~ @header['content-type']
        @header.delete('content-length')
      elsif @body_range && @body.respond_to?(:readpartial)
        # The declared body range is the single source of truth for both the
        # Content-Length and the bytes that will actually be written.  A body
        # of unknown length can only be delimited by closing the connection.
        _, length = body_send_window
        if length
          @header['content-length'] = length.to_s
        else
          @header.delete('content-length')
          @header['connection'] = 'close'
        end
      elsif @header['content-length'].nil?
        if @body.respond_to?(:bytesize)
          @header['content-length'] = @body.bytesize.to_s
        else
          @header['connection'] = 'close'
        end
      end

      # Keep-Alive connection.
      if @header['connection'] == "close"
         @keep_alive = false
      elsif keep_alive?
        if chunked? || @header['content-length'] || @status == 304 || @status == 204 || HTTPStatus.info?(@status)
          @header['connection'] = "Keep-Alive"
        else
          msg = "Could not determine content-length of response body. Set content-length of the response or set Response#chunked = true"
          @logger.warn(msg)
          @header['connection'] = "close"
          @keep_alive = false
        end
      else
        @header['connection'] = "close"
      end

      # Location is a single absoluteURI.
      if location = @header['location']
        if @request_uri
          @header['location'] = @request_uri.merge(location).to_s
        end
      end
    end

    def make_body_tempfile # :nodoc:
      return if @bodytempfile
      bodytempfile = Tempfile.create("webrick")
      if @body.nil?
        # nothing
      elsif @body.respond_to? :readpartial
        IO.copy_stream(@body, bodytempfile)
        @body.close
      elsif @body.respond_to? :call
        @body.call(bodytempfile)
      else
        bodytempfile.write @body
      end
      bodytempfile.rewind
      @body = @bodytempfile = bodytempfile
      # The whole body has been buffered, so any earlier byte range no longer
      # applies and the length is the buffered size.
      @body_range = nil
      @body_send_window = nil
      @header['content-length'] = bodytempfile.stat.size.to_s
    end

    def remove_body_tempfile # :nodoc:
      if @bodytempfile
        @bodytempfile.close
        File.unlink @bodytempfile.path
        @bodytempfile = nil
      end
    end


    ##
    # Sends the headers on +socket+

    def send_header(socket) # :nodoc:
      if @http_version.major > 0
        data = status_line().dup
        @header.each{|key, value|
          tmp = key.gsub(/\bwww|^te$|\b\w/){ $&.upcase }
          data << "#{tmp}: #{check_header(value)}" << CRLF
        }
        @cookies.each{|cookie|
          data << "Set-Cookie: " << check_header(cookie.to_s) << CRLF
        }
        data << CRLF
        socket.write(data)
      end
    rescue InvalidHeader => e
      @header.clear
      @cookies.clear
      set_error e
      retry
    end

    ##
    # Sends the body on +socket+

    def send_body(socket) # :nodoc:
      if @body.respond_to? :readpartial then
        send_body_io(socket)
      elsif @body.respond_to?(:call) then
        send_body_proc(socket)
      else
        send_body_string(socket)
      end
    end

    ##
    # Redirects to +url+ with a WEBrick::HTTPStatus::Redirect +status+.
    #
    # Example:
    #
    #   res.set_redirect WEBrick::HTTPStatus::TemporaryRedirect

    def set_redirect(status, url)
      url = URI(url).to_s
      @body = "<HTML><A HREF=\"#{url}\">#{url}</A>.</HTML>\n"
      @header['location'] = url
      raise status
    end

    ##
    # Creates an error page for exception +ex+ with an optional +backtrace+

    def set_error(ex, backtrace=false)
      case ex
      when HTTPStatus::Status
        @keep_alive = false if HTTPStatus::error?(ex.code)
        self.status = ex.code
      else
        @keep_alive = false
        self.status = HTTPStatus::RC_INTERNAL_SERVER_ERROR
      end
      @header['content-type'] = "text/html; charset=ISO-8859-1"

      if respond_to?(:create_error_page)
        create_error_page()
        return
      end

      if @request_uri
        host, port = @request_uri.host, @request_uri.port
      else
        host, port = @config[:ServerName], @config[:Port]
      end

      error_body(backtrace, ex, host, port)
    end

    private

    def check_header(header_value)
      header_value = header_value.to_s
      if /[\r\n]/ =~ header_value
        raise InvalidHeader
      else
        header_value
      end
    end

    # :stopdoc:

    def error_body(backtrace, ex, host, port)
      @body = +""
      @body_range = nil
      @body_send_window = nil
      @body << <<-_end_of_html_
<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 4.0//EN">
<HTML>
  <HEAD><TITLE>#{HTMLUtils::escape(@reason_phrase)}</TITLE></HEAD>
  <BODY>
    <H1>#{HTMLUtils::escape(@reason_phrase)}</H1>
    #{HTMLUtils::escape(ex.message)}
    <HR>
      _end_of_html_

      if backtrace && $DEBUG
        @body << "backtrace of '#{HTMLUtils::escape(ex.class.to_s)}' "
        @body << "#{HTMLUtils::escape(ex.message)}"
        @body << "<PRE>"
        ex.backtrace.each{|line| @body << "\t#{line}\n"}
        @body << "</PRE><HR>"
      end

      @body << <<-_end_of_html_
    <ADDRESS>
     #{HTMLUtils::escape(@config[:ServerSoftware])} at
     #{host}:#{port}
    </ADDRESS>
  </BODY>
</HTML>
      _end_of_html_
    end

    def send_body_io(socket)
      begin
        if @request_method == "HEAD"
          # Never read the body.  A ranged body's cursor is observed when the
          # header is built, but not advanced here.
        elsif @body_range
          send_body_io_range(socket)
        elsif chunked?
          buf = +''
          begin
            @body.readpartial(@buffer_size, buf)
            size = buf.bytesize
            data = +"#{size.to_s(16)}#{CRLF}#{buf}#{CRLF}"
            socket.write(data)
            data.clear
            @sent_size += size
          rescue EOFError
            break
          end while true
          buf.clear
          socket.write("0#{CRLF}#{CRLF}")
        else
          if %r{\Abytes (\d+)-(\d+)/\d+\z} =~ @header['content-range']
            offset = $1.to_i
            size = $2.to_i - offset + 1
          else
            offset = nil
            size = @header['content-length']
            size = size.to_i if size
          end
          begin
            @sent_size = IO.copy_stream(@body, socket, size, offset)
          rescue NotImplementedError
            @body.seek(offset, IO::SEEK_SET)
            @sent_size = IO.copy_stream(@body, socket, size)
          end
        end
      ensure
        @body.close if @body.respond_to?(:close)
      end
      remove_body_tempfile
    end

    # Sends the part of the IO body described by #body_range.  The offset and
    # length come from #body_send_window, which is the same source the header
    # used, so the announced Content-Length and the written bytes agree.
    def send_body_io_range(socket)
      offset, length = body_send_window
      align_body(offset)

      if chunked?
        remaining = length
        truncated = false
        buf = +''
        begin
          while remaining.nil? || remaining > 0
            want = @buffer_size
            want = remaining if remaining && remaining < want
            @body.readpartial(want, buf)
            size = buf.bytesize
            remaining -= size if remaining
            socket.write(+"#{size.to_s(16)}#{CRLF}#{buf}#{CRLF}")
            @sent_size += size
          end
        rescue EOFError
          # When the whole length was known, ending early means the body is
          # short; report it by dropping the connection instead of closing the
          # chunked message as if it were complete.
          truncated = !remaining.nil? && remaining > 0
        end
        if truncated
          @keep_alive = false
          @header['connection'] = 'close'
        else
          socket.write("0#{CRLF}#{CRLF}")
        end
      else
        sent = IO.copy_stream(@body, socket, length)
        @sent_size = sent
        if length && sent < length
          # The body ended before the announced window.  The header is already
          # on the wire, so the only correct finish is to close the connection
          # instead of letting the client wait for bytes that will not come.
          @keep_alive = false
          @header['connection'] = 'close'
        end
      end
    end

    # The [offset, length] pair that both the header and the body writer use.
    # +length+ is +nil+ when the body's total size is unknown.  The result is
    # memoized so a later read of the body cannot shift what was announced.
    #
    # A non-seekable body cannot move backwards.  If the declared start is
    # behind its current position the window is rebased to the current
    # position, so the header and the bytes sent still agree.
    def body_send_window # :nodoc:
      return @body_send_window if @body_send_window

      size = body_size
      start, finish = requested_body_window(size)
      start = effective_body_start(start)
      length =
        if start.nil?
          # The body's position is not reported, so the size of what remains
          # is unknowable: the response must be delimited by connection close.
          nil
        elsif finish
          finish - start + 1
        elsif size
          size - start
        end
      length = 0 if length && length < 0

      if length.nil? && (declared = @header['content-length'])
        # The body's size cannot be probed, but the caller declared one.  Use
        # it so the header and the bytes written still describe one window.
        begin
          length = Integer(declared)
          length = 0 if length < 0
        rescue ArgumentError
          length = nil
        end
      end

      @body_send_window = [start, length]
    end

    # Translates #body_range into an inclusive [start, finish] window, or
    # +nil+ finish for "until the end of the body".  +start+ is +nil+ for
    # +:current+ when the body cannot report its position.
    def requested_body_window(size) # :nodoc:
      case range = @body_range
      when :current
        [body_position, size && size - 1]
      when Integer
        [range, size && size - 1]
      when Range
        finish = range.end
        finish -= 1 if range.exclude_end?
        finish = size - 1 if size && finish >= size
        [range.begin, finish]
      else
        [0, size && size - 1]
      end
    end

    # The position the body can actually start from.  A seekable body can be
    # moved anywhere; a non-seekable one can only move forward, so a start
    # behind its current position is replaced by the current position.
    def effective_body_start(start) # :nodoc:
      position = body_position
      return start if position.nil? || start.nil?
      return start if position <= start
      return start if body_seekable?
      position
    end

    def body_seekable? # :nodoc:
      return false unless @body.respond_to?(:seek)
      if @body.respond_to?(:stat)
        stat = @body.stat
        return false unless stat.file?
      end
      true
    rescue IOError, SystemCallError
      false
    end

    # Total number of bytes in the body, or +nil+ when it cannot be known.
    def body_size # :nodoc:
      if @body.respond_to?(:stat)
        begin
          stat = @body.stat
          return stat.size if stat.file?
        rescue IOError, SystemCallError
          # fall through to the size probe
        end
      end
      @body.size if @body.respond_to?(:size)
    rescue IOError, SystemCallError
      nil
    end

    # Current byte position of the body, or +nil+ when it is not reported.
    def body_position # :nodoc:
      @body.pos if @body.respond_to?(:pos)
    rescue IOError, SystemCallError
      nil
    end

    # Moves the body so its next byte is at +offset+.  A seekable body is
    # repositioned with #seek.  A non-seekable body can only move forward, so
    # it is advanced by reading and discarding.  A start behind a non-seekable
    # body was already rebased to its current position by #effective_body_start,
    # so no backwards move is attempted here.
    def align_body(offset) # :nodoc:
      return if offset.nil?

      position = body_position
      return if position == offset
      return if position.nil? # trust the caller that the body is positioned

      if @body.respond_to?(:seek) && body_seekable?
        begin
          @body.seek(offset, IO::SEEK_SET)
          return
        rescue IOError, NotImplementedError, SystemCallError
          # The body claimed to be seekable but is not; skip forward instead.
        end
      end

      # A non-seekable body cannot go backwards.  A start behind the current
      # position was already rebased to it, so there is nothing to skip.
      return if offset <= position

      skip_body_bytes(offset - position)
    end

    # Reads and discards up to +count+ bytes.  Returns the number of bytes
    # actually skipped, which is smaller than +count+ on early EOF.
    def skip_body_bytes(count) # :nodoc:
      remaining = count
      while remaining > 0
        want = remaining < @buffer_size ? remaining : @buffer_size
        chunk =
          if @body.respond_to?(:read)
            @body.read(want)
          else
            begin
              @body.readpartial(want)
            rescue EOFError
              nil
            end
          end
        break if chunk.nil? || chunk.empty?
        remaining -= chunk.bytesize
      end
      count - remaining
    end

    def send_body_string(socket)
      if @request_method == "HEAD"
        # do nothing
      elsif chunked?
        body ? @body.bytesize : 0
        while buf = @body[@sent_size, @buffer_size]
          break if buf.empty?
          size = buf.bytesize
          data = "#{size.to_s(16)}#{CRLF}#{buf}#{CRLF}"
          buf.clear
          socket.write(data)
          @sent_size += size
        end
        socket.write("0#{CRLF}#{CRLF}")
      else
        if @body && @body.bytesize > 0
          socket.write(@body)
          @sent_size = @body.bytesize
        end
      end
    end

    def send_body_proc(socket)
      if @request_method == "HEAD"
        # do nothing
      elsif chunked?
        @body.call(ChunkedWrapper.new(socket, self))
        socket.write("0#{CRLF}#{CRLF}")
      else
        if @bodytempfile
          @bodytempfile.rewind
          IO.copy_stream(@bodytempfile, socket)
        else
          @body.call(socket)
        end

        if content_length = @header['content-length']
          @sent_size = content_length.to_i
        end
      end
    end

    class ChunkedWrapper
      def initialize(socket, resp)
        @socket = socket
        @resp = resp
      end

      def write(buf)
        return 0 if buf.empty?
        socket = @socket
        @resp.instance_eval {
          size = buf.bytesize
          data = +"#{size.to_s(16)}#{CRLF}#{buf}#{CRLF}"
          socket.write(data)
          data.clear
          @sent_size += size
          size
        }
      end

      def <<(*buf)
        write(buf)
        self
      end
    end

    # preserved for compatibility with some 3rd-party handlers
    def _write_data(socket, data)
      socket << data
    end

    # :startdoc:
  end

end
