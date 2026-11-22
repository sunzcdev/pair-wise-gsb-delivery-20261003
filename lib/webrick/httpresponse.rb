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
require_relative 'accesslog'

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

    attr_accessor :body

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
    # The response header as it was handed to the transport.
    #
    # Each entry is a <tt>[name, value]</tt> pair, in the order the pair was
    # written.  Unlike #header (a Hash that keeps a single value per field
    # name) this keeps every occurrence, so a field written twice, such as two
    # <tt>Set-Cookie</tt> lines, appears twice.  The name keeps the spelling
    # used on the wire and the value is the value that passed #check_header.
    #
    # The Array is empty until #send_header is called.

    attr_reader :sent_header

    ##
    # How much of the response header reached the transport.
    #
    # :unsent::   #send_header has not handed the header to the transport
    # :partial::  the header was handed to the transport but the write did not
    #             finish; the transport may have received any prefix of it
    # :complete:: the whole header, including the terminating blank line, was
    #             written

    attr_reader :header_write_state

    ##
    # Number of response header bytes confirmed written to the transport.
    # Zero while the header has not been written and equal to #sent_header_size
    # once #header_write_state is :complete.

    attr_reader :header_bytes_written

    ##
    # Total size of the header block handed to the transport, status line and
    # terminating blank line included.  Zero until #send_header is called.

    attr_reader :sent_header_size

    ##
    # The status code carried by a status line that the transport confirmed
    # wrote, or nil.  A partial write can confirm the status line while the
    # rest of the header is lost, so this may be set while
    # #header_write_state is :partial.  Compare #status, which is the status
    # this response would send.

    attr_reader :sent_status

    ##
    # The WEBrick::AccessLog::ResponseHeaderView used when this response is
    # logged, or nil.  Assign an options Hash, an Array of field names or a
    # ResponseHeaderView; see WEBrick::AccessLog::ResponseHeaderView.

    attr_reader :response_header_view

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
      @sent_header = []
      @header_write_state = :unsent
      @header_bytes_written = 0
      @sent_header_size = 0
      @sent_status = nil
      @response_header_view = nil
    end

    ##
    # The response's HTTP status line

    def status_line
      "HTTP/#@http_version #@status #@reason_phrase".rstrip << CRLF
    end

    ##
    # Sets the view used to select and mask response header fields in the
    # access log.  See WEBrick::AccessLog::ResponseHeaderView.

    def response_header_view=(spec)
      @response_header_view =
        if spec.is_a?(AccessLog::ResponseHeaderView)
          spec
        else
          AccessLog::ResponseHeaderView.build(spec)
        end
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
    #
    # The header is built as a single block and handed to the transport in one
    # write.  #sent_header records the pairs as they appear in that block, so
    # repeated fields stay distinct, and #header_write_state reports whether
    # the transport confirmed the whole block.  When the write raises, the
    # state stays :partial: the transport may have received a prefix of the
    # header and the caller can tell that no complete header was sent.

    def send_header(socket) # :nodoc:
      if @http_version.major > 0
        begin
          data = status_line().dup
          status_line_size = data.bytesize
          pairs = []
          @header.each{|key, value|
            tmp = key.gsub(/\bwww|^te$|\b\w/){ $&.upcase }
            checked = check_header(value)
            data << "#{tmp}: #{checked}" << CRLF
            pairs << [tmp, checked]
          }
          @cookies.each{|cookie|
            checked = check_header(cookie.to_s)
            data << "Set-Cookie: " << checked << CRLF
            pairs << ["Set-Cookie", checked]
          }
          data << CRLF
        rescue InvalidHeader => e
          @header.clear
          @cookies.clear
          set_error e
          retry
        end

        # The header is complete and about to be handed to the transport.
        # Anything that goes wrong from here on leaves the state at :partial.
        @sent_header = pairs
        @sent_header_size = data.bytesize
        @sent_status = nil
        @header_bytes_written = 0
        @header_write_state = :partial
        written = socket.write(data)
        written =
          if written.is_a?(Integer)
            [written, data.bytesize].min
          else
            data.bytesize
          end
        @header_bytes_written = written
        # A status line that the transport confirmed wrote is reported even
        # when the rest of the header was lost, so the log can say a response
        # was started but not completed.
        @sent_status = @status if written >= status_line_size
        @header_write_state = :complete if written >= data.bytesize
      end
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
          # do nothing
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
        @body.close
      end
      remove_body_tempfile
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
