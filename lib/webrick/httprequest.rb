# frozen_string_literal: true
#
# httprequest.rb -- HTTPRequest Class
#
# Author: IPR -- Internet Programming with Ruby -- writers
# Copyright (c) 2000, 2001 TAKAHASHI Masayoshi, GOTOU Yuuzou
# Copyright (c) 2002 Internet Programming with Ruby writers. All rights
# reserved.
#
# $IPR: httprequest.rb,v 1.64 2003/07/13 17:18:22 gotoyuzo Exp $

require 'fiber'
require 'uri'
require_relative 'httpversion'
require_relative 'httpstatus'
require_relative 'httputils'
require_relative 'cookie'

module WEBrick

  ##
  # An HTTP request.  This is consumed by service and do_* methods in
  # WEBrick servlets

  class HTTPRequest

    BODY_CONTAINABLE_METHODS = [ "POST", "PUT" ] # :nodoc:

    # :section: Request line

    ##
    # The complete request line such as:
    #
    #   GET / HTTP/1.1

    attr_reader :request_line

    ##
    # The request method, GET, POST, PUT, etc.

    attr_reader :request_method

    ##
    # The unparsed URI of the request

    attr_reader :unparsed_uri

    ##
    # The HTTP version of the request

    attr_reader :http_version

    # :section: Request-URI

    ##
    # The parsed URI of the request

    attr_reader :request_uri

    ##
    # The request path

    attr_reader :path

    ##
    # The script name (CGI variable)

    attr_accessor :script_name

    ##
    # The path info (CGI variable)

    attr_accessor :path_info

    ##
    # The query from the URI of the request

    attr_accessor :query_string

    # :section: Header and entity body

    ##
    # The raw header of the request

    attr_reader :raw_header

    ##
    # The parsed header of the request

    attr_reader :header

    ##
    # The parsed request cookies

    attr_reader :cookies

    ##
    # The Accept header value

    attr_reader :accept

    ##
    # The Accept-Charset header value

    attr_reader :accept_charset

    ##
    # The Accept-Encoding header value

    attr_reader :accept_encoding

    ##
    # The Accept-Language header value

    attr_reader :accept_language

    # :section:

    ##
    # The remote user (CGI variable)

    attr_accessor :user

    ##
    # The socket address of the server

    attr_reader :addr

    ##
    # The socket address of the client

    attr_reader :peeraddr

    ##
    # Hash of request attributes

    attr_reader :attributes

    ##
    # Is this a keep-alive connection?

    attr_reader :keep_alive

    ##
    # The local time this request was received

    attr_reader :request_time

    ##
    # Creates a new HTTP request.  WEBrick::Config::HTTP is the default
    # configuration.

    def initialize(config)
      @config = config
      @buffer_size = @config[:InputBufferSize]
      @logger = config[:Logger]

      @request_line = @request_method =
        @unparsed_uri = @http_version = nil

      @request_uri = @host = @port = @path = nil
      @script_name = @path_info = nil
      @query_string = nil
      @query = nil

      @raw_header = Array.new
      @header = nil
      @cookies = []
      @accept = []
      @accept_charset = []
      @accept_encoding = []
      @accept_language = []
      @body = +""

      # Single decoded copy of the entity body.  Both #body and #body_reader
      # consume from this buffer, so the socket is read exactly once even when
      # a hook peeks at a prefix before handing the request to someone else.
      @body_data = +"".b
      @body_pos = 0
      @body_complete = false
      @body_error = nil
      @body_mode = nil
      @chunk_remaining = nil
      @body_chunked = false

      @addr = []
      @peeraddr = []
      @attributes = {}
      @user = nil
      @keep_alive = false
      @request_time = nil

      @remaining_size = nil
      @socket = nil

      @forwarded_proto = @forwarded_host = @forwarded_port =
        @forwarded_server = @forwarded_for = nil
    end

    ##
    # Parses a request from +socket+.  This is called internally by
    # WEBrick::HTTPServer.

    def parse(socket=nil)
      @socket = socket
      begin
        @peeraddr = socket.respond_to?(:peeraddr) ? socket.peeraddr : []
        @addr = socket.respond_to?(:addr) ? socket.addr : []
      rescue Errno::ENOTCONN
        raise HTTPStatus::EOFError
      end

      read_request_line(socket)
      if @http_version.major > 0
        read_header(socket)
        @header['cookie'].each{|cookie|
          @cookies += Cookie::parse(cookie)
        }
        @accept = HTTPUtils.parse_qvalues(self['accept'])
        @accept_charset = HTTPUtils.parse_qvalues(self['accept-charset'])
        @accept_encoding = HTTPUtils.parse_qvalues(self['accept-encoding'])
        @accept_language = HTTPUtils.parse_qvalues(self['accept-language'])
      end

      setup_forwarded_info

      if @request_method == "CONNECT" || @unparsed_uri == "*"
        # For CONNECT and OPTIONS * requests, we still need to extract
        # host and port from the Host header for CGI meta variables.
        @host, @port = extract_host_port
        return
      end

      begin
        @request_uri = parse_uri(@unparsed_uri)
        @path = HTTPUtils::unescape(@request_uri.path)
        @path = HTTPUtils::normalize_path(@path)
        @host = @request_uri.host
        @port = @request_uri.port
        @query_string = @request_uri.query
        @script_name = ""
        @path_info = @path.dup
      rescue
        raise HTTPStatus::BadRequest, "bad URI '#{@unparsed_uri}'."
      end

      if /\Aclose\z/io =~ self["connection"]
        @keep_alive = false
      elsif /\Akeep-alive\z/io =~ self["connection"]
        @keep_alive = true
      elsif @http_version < "1.1"
        @keep_alive = false
      else
        @keep_alive = true
      end
    end

    ##
    # Generate HTTP/1.1 100 continue response if the client expects it,
    # otherwise does nothing.

    def continue # :nodoc:
      if self['expect'] == '100-continue' && @config[:HTTPVersion] >= "1.1"
        @socket << "HTTP/#{@config[:HTTPVersion]} 100 continue#{CRLF}#{CRLF}"
        @header.delete('expect')
      end
    end

    ##
    # Returns the request body.
    #
    # Without arguments the body is read from the current position of the
    # body stream.  If a request hook already consumed a prefix through
    # #body_reader or #readpartial, only the remainder is returned, so the
    # hook's bytes and this call's bytes together reconstruct the entity
    # body exactly once.
    #
    # Pass <tt>from: :start</tt> when the caller explicitly wants the whole
    # entity body from byte 0, replaying whatever a previous consumer already
    # took.  This is the handoff used by forwarders that must reproduce the
    # full body even though a hook inspected a prefix.
    #
    # When a block is given the chunks are yielded as they become available
    # and nil is returned.

    def body(from: :current, &block) # :yields: body_chunk
      rewind_body if from == :start
      if block
        while chunk = body_chunk
          block.call(chunk)
        end
        nil
      else
        while chunk = body_chunk
          @body << chunk
        end
        @body.empty? ? nil : @body
      end
    end

    ##
    # Prepares the HTTPRequest object for use as the
    # source for IO.copy_stream.
    #
    # Like #body, the reader starts at the current position unless
    # <tt>from: :start</tt> is given, in which case the whole entity body is
    # replayed from the beginning.

    def body_reader(from: :current)
      rewind_body if from == :start
      self
    end

    # for IO.copy_stream.
    def readpartial(size, buf = ''.b) # :nodoc
      return buf.clear if size <= 0
      chunk = body_chunk(size) or raise EOFError, 'end of file reached'
      buf.replace(chunk)
      buf
    end

    ##
    # Moves the body stream back to the beginning of the entity body so the
    # next #body or #body_reader call sees the whole request body again.
    # Bytes already decoded from the socket are replayed from memory.

    def rewind_body
      @body_pos = 0
      @body = +""
      self
    end

    ##
    # Number of entity body bytes decoded from the socket so far.

    def body_bytes_read
      @body_data.bytesize
    end

    ##
    # Number of entity body bytes handed to #body, #body_reader or
    # #readpartial so far.

    def body_bytes_consumed
      @body_pos
    end

    ##
    # Total size of the entity body, or nil while it is still incomplete.
    # A body that failed to decode has no reliable length and also returns nil.

    def body_length
      @body_complete && !@body_error ? @body_data.bytesize : nil
    end

    ##
    # True once the whole entity body (including chunked trailers) has been
    # decoded from the socket.

    def body_complete?
      @body_complete
    end

    ##
    # True when the request body arrived with chunked transfer-encoding.
    # Unlike the deleted +transfer-encoding+ header after decoding, this
    # stays set so a forwarder can choose how to re-frame the upstream body.

    def chunked_body?
      @body_chunked
    end

    ##
    # Request query as a Hash

    def query
      unless @query
        parse_query()
      end
      @query
    end

    ##
    # The content-length header

    def content_length
      if length = self['content-length']
        Integer(length, 10)
      end
    end

    ##
    # The content-type header

    def content_type
      return self['content-type']
    end

    ##
    # Retrieves +header_name+

    def [](header_name)
      if @header
        value = @header[header_name.downcase]
        value.empty? ? nil : value.join
      end
    end

    ##
    # Iterates over the request headers

    def each
      if @header
        @header.each{|k, v|
          value = @header[k]
          yield(k, value.empty? ? nil : value.join)
        }
      end
    end

    ##
    # The host this request is for

    def host
      return @forwarded_host || @host
    end

    ##
    # The port this request is for

    def port
      return @forwarded_port || @port
    end

    ##
    # The server name this request is for

    def server_name
      return @forwarded_server || @config[:ServerName]
    end

    ##
    # The client's IP address

    def remote_ip
      return self["client-ip"] || @forwarded_for || @peeraddr[3]
    end

    ##
    # Is this an SSL request?

    def ssl?
      return @request_uri.scheme == "https"
    end

    ##
    # Should the connection this request was made on be kept alive?

    def keep_alive?
      @keep_alive
    end

    def to_s # :nodoc:
      ret = @request_line.dup
      @raw_header.each{|line| ret << line }
      ret << CRLF
      # Render the full entity body without disturbing the offset (or the
      # accumulated #body buffer) a caller may still be reading from.
      pos, saved = @body_pos, @body
      if data = body(from: :start)
        ret << data
      end
      @body_pos, @body = pos, saved
      ret
    end

    ##
    # Consumes any remaining body and updates keep-alive status

    def fixup() # :nodoc:
      begin
        body{|chunk| }   # read remaining body from the current offset
      rescue HTTPStatus::Error => ex
        @logger.error("HTTPRequest#fixup: #{ex.class} occurred.")
        @keep_alive = false
      rescue => ex
        @logger.error(ex)
        @keep_alive = false
      end
      # A body that could not be fully decoded must not silently shift the
      # framing of the next request on this connection.
      @keep_alive = false if @body_error
    end

    # This method provides the metavariables defined by the revision 3
    # of "The WWW Common Gateway Interface Version 1.1"
    # To browse the current document of CGI Version 1.1, see below:
    # https://www.rfc-editor.org/rfc/rfc3875

    def meta_vars
      meta = Hash.new

      cl = self["Content-Length"]
      ct = self["Content-Type"]
      meta["CONTENT_LENGTH"]    = cl if cl.to_i > 0
      meta["CONTENT_TYPE"]      = ct.dup if ct
      meta["GATEWAY_INTERFACE"] = "CGI/1.1"
      meta["PATH_INFO"]         = @path_info ? @path_info.dup : ""
     #meta["PATH_TRANSLATED"]   = nil      # no plan to be provided
      meta["QUERY_STRING"]      = @query_string ? @query_string.dup : ""
      meta["REMOTE_ADDR"]       = @peeraddr[3]
      meta["REMOTE_HOST"]       = @peeraddr[2]
     #meta["REMOTE_IDENT"]      = nil      # no plan to be provided
      meta["REMOTE_USER"]       = @user
      meta["REQUEST_METHOD"]    = @request_method.dup
      meta["REQUEST_URI"]       = @request_uri.to_s
      meta["SCRIPT_NAME"]       = @script_name.dup
      meta["SERVER_NAME"]       = @host
      meta["SERVER_PORT"]       = @port.to_s
      meta["SERVER_PROTOCOL"]   = "HTTP/" + @config[:HTTPVersion].to_s
      meta["SERVER_SOFTWARE"]   = @config[:ServerSoftware].dup

      self.each{|key, val|
        next if /^content-type$/i =~ key
        next if /^content-length$/i =~ key
        name = "HTTP_" + key
        name.gsub!(/-/o, "_")
        name.upcase!
        meta[name] = val
      }

      meta
    end

    private

    # :stopdoc:

    MAX_URI_LENGTH = 2083 # :nodoc:

    # same as Mongrel, Thin and Puma
    MAX_HEADER_LENGTH = (112 * 1024) # :nodoc:

    def read_request_line(socket)
      @request_line = read_line(socket, MAX_URI_LENGTH) if socket
      raise HTTPStatus::EOFError unless @request_line

      @request_bytes = @request_line.bytesize
      if @request_bytes >= MAX_URI_LENGTH and @request_line[-1, 1] != LF
        raise HTTPStatus::RequestURITooLarge
      end

      @request_time = Time.now
      if /^(\S+) (\S++)(?: HTTP\/(\d+\.\d+))?\r\n/mo =~ @request_line
        @request_method = $1
        @unparsed_uri   = $2
        @http_version   = HTTPVersion.new($3 ? $3 : "0.9")
      else
        rl = @request_line.sub(/\x0d?\x0a\z/o, '')
        raise HTTPStatus::BadRequest, "bad Request-Line '#{rl}'."
      end
    end

    DISALLOWED_TRAILERS = %w[content-encoding content-type content-range
      trailer authorization set-cookie transfer-encoding content-length
      host cache-control max-forwards te].freeze
    private_constant :DISALLOWED_TRAILERS

    def read_header(socket)
      if socket
        end_of_headers = false

        raw = if @header
          trailer_lines = []
        else
          @raw_header
        end

        while line = read_line(socket)
          if line == CRLF
            end_of_headers = true
            break
          end
          if (@request_bytes += line.bytesize) > MAX_HEADER_LENGTH
            raise HTTPStatus::RequestEntityTooLarge, 'headers too large'
          end
          if line.include?("\x00")
            raise HTTPStatus::BadRequest, 'null byte in header'
          end
          raw << line
        end

        # Allow if @header already set to support chunked trailers
        raise HTTPStatus::EOFError unless end_of_headers || trailer_lines
      end

      if trailer_lines
        if requested_trailers = self["trailer"]
          requested_trailers = requested_trailers.downcase.split(/[,\s]+/)
          requested_trailers -= DISALLOWED_TRAILERS
          unless requested_trailers.empty?
            trailers = HTTPUtils::parse_header(trailer_lines.join)
            requested_trailers.each do |key|
              if trailers.key?(key)
                @header[key] = trailers[key]
              end
            end
          end
        end
      else
        @header = HTTPUtils::parse_header(@raw_header.join)

        if (content_length = @header['content-length']) && content_length.length != 0
          if content_length.length > 1
            raise HTTPStatus::BadRequest, "multiple content-length request headers"
          elsif !/\A\d+\z/.match?(content_length[0])
            raise HTTPStatus::BadRequest, "invalid content-length request header"
          end
        end
      end
    end

    # Extracts host and port from request headers (Host, X-Forwarded-Host, etc.)
    # or falls back to socket address or server config.
    # Returns [host, port] where port is an Integer or nil.
    def extract_host_port
      if @forwarded_host
        host, port = @forwarded_host, @forwarded_port
      elsif self["host"]
        host, port = parse_host_request_line(self["host"])
        port = port.to_i if port
      elsif @addr.size > 0
        host, port = @addr[2], @addr[1]
      else
        host, port = @config[:ServerName], @config[:Port]
      end

      [host, port]
    end

    def parse_uri(str, scheme="http")
      if @config[:Escape8bitURI]
        str = HTTPUtils::escape8bit(str)
      end
      str.sub!(%r{\A/+}, '/')
      uri = URI::parse(str)
      return uri if uri.absolute?

      host, port = extract_host_port
      uri.scheme = @forwarded_proto || scheme
      uri.host = host
      uri.port = port
      return URI::parse(uri.to_s)
    end

    host_pattern = URI::RFC2396_Parser.new.pattern.fetch(:HOST)
    HOST_PATTERN = /\A(#{host_pattern})(?::(\d+))?\z/n
    def parse_host_request_line(host)
      host.scan(HOST_PATTERN)[0]
    end

    def read_body(socket, block)
      return unless socket
      @socket = socket
      while chunk = body_chunk
        block.call(chunk)
      end
      return @body
    end

    # Returns the next +size+ bytes of the entity body starting at the current
    # offset, or nil at the end of the body.
    #
    # The entity body is decoded from the socket only once; every requested
    # range is served from @body_data, so a later #body (or a request on the
    # same keep-alive connection after #fixup) never re-reads socket bytes
    # that belong to this request.  Decoding errors are kept in @body_error so
    # that consumers which stop early still see the failure when they reach
    # the end of what could be decoded.
    def body_chunk(size = @buffer_size)
      bytes = read_body_bytes(size)
      bytes unless bytes.nil? || bytes.empty?
    end

    def read_body_bytes(size)
      need = @body_pos + size
      while @body_data.bytesize < need && !@body_complete
        break if @body_error
        begin
          break unless fill_body_buffer(@socket)
        rescue StandardError => ex
          # Serve the bytes that were decoded before the failure; the error is
          # re-raised once the consumer reaches the end of them.
          @body_error ||= ex
          @body_complete = true
          break
        end
      end
      available = @body_data.bytesize - @body_pos
      if available <= 0
        raise @body_error if @body_error
        return nil
      end
      chunk = @body_data.byteslice(@body_pos, [size, available].min)
      @body_pos += chunk.bytesize
      chunk
    end

    # Reads one more piece of the entity body from +socket+ into @body_data and
    # returns true when the buffer grew.  Chunked framing is decoded
    # incrementally so a hook can stop in the middle of a chunk and the next
    # consumer continues from the same offset.
    def fill_body_buffer(socket)
      before = @body_data.bytesize
      unless @body_mode
        if socket.nil?
          @body_mode = :none
          @body_complete = true
          return false
        end
        if tc = self['transfer-encoding']
          if self['content-length']
            raise HTTPStatus::BadRequest, "request with both transfer-encoding and content-length, possible request smuggling"
          end
          unless /\Achunked\z/io =~ tc
            raise HTTPStatus::NotImplemented, "Transfer-Encoding: #{tc}."
          end
          @body_mode = :chunked
          @chunk_remaining = nil
          @body_chunked = true
        elsif self['content-length'] || @remaining_size
          @body_mode = :length
          @remaining_size ||= self['content-length'].to_i
        elsif BODY_CONTAINABLE_METHODS.member?(@request_method)
          raise HTTPStatus::LengthRequired
        else
          @body_mode = :none
          @body_complete = true
          return false
        end
      end

      begin
        case @body_mode
        when :none
          @body_complete = true
        when :length
          if @remaining_size <= 0
            @body_complete = true
            return false
          end
          sz = [@buffer_size, @remaining_size].min
          buf = read_data(socket, sz)
          if buf.nil? || buf.empty?
            # A nil read without EOF means the peer sent nothing more yet;
            # leave the request readable instead of silently truncating.
            @body_complete = true if socket_eof?(socket)
            if @remaining_size > 0 && @body_complete
              raise HTTPStatus::BadRequest, "invalid body size."
            end
            return false
          end
          @remaining_size -= buf.bytesize
          @body_data << buf
        when :chunked
          fill_chunked_buffer(socket)
        end
      rescue StandardError => ex
        # Keep the failure so consumers reach it after the bytes that could be
        # decoded, and so #fixup can refuse to reuse the connection.
        @body_error ||= ex
        @body_complete = true
        raise ex
      end

      @body_data.bytesize > before
    end

    def socket_eof?(socket)
      return true unless socket.respond_to?(:eof?)
      socket.eof?
    rescue IOError, SystemCallError
      true
    end

    def fill_chunked_buffer(socket)
      if @chunk_remaining.nil?
        chunk_size, = read_chunk_size(socket)
        if chunk_size == 0
          read_header(socket)                # trailer + CRLF
          @header.delete("transfer-encoding")
          @remaining_size = 0
          @body_complete = true
          return
        end
        @chunk_remaining = chunk_size
      end

      sz = [@chunk_remaining, @buffer_size].min
      data = read_data(socket, sz)           # read chunk-data
      if data.nil? || data.bytesize != sz
        raise HTTPStatus::BadRequest, "bad chunk data size."
      end
      @body_data << data
      @chunk_remaining -= sz

      if @chunk_remaining <= 0
        line = read_line(socket)             # skip CRLF
        unless line == "\r\n"
          raise HTTPStatus::BadRequest, "extra data after chunk '#{line}'."
        end
        @chunk_remaining = nil
      end
    end

    def read_chunk_size(socket)
      line = read_line(socket)
      if /\A([0-9a-fA-F]+)(?:;(\S+(?:=\S+)?))?\r\n\z/ =~ line
        chunk_size = $1.hex
        chunk_ext = $2
        [ chunk_size, chunk_ext ]
      else
        raise HTTPStatus::BadRequest, "bad chunk '#{line}'."
      end
    end

    def _read_data(io, method, *arg)
      begin
        WEBrick::Utils.timeout(@config[:RequestTimeout]){
          return io.__send__(method, *arg)
        }
      rescue Errno::ECONNRESET
        return nil
      rescue Timeout::Error
        raise HTTPStatus::RequestTimeout
      end
    end

    def read_line(io, size=4096)
      _read_data(io, :gets, LF, size)
    end

    def read_data(io, size)
      _read_data(io, :read, size)
    end

    def parse_query()
      begin
        if @request_method == "GET" || @request_method == "HEAD"
          @query = HTTPUtils::parse_query(@query_string)
        elsif self['content-type'] =~ /^application\/x-www-form-urlencoded/
          # Form parsing needs the complete entity body, even if a hook has
          # already peeked at a prefix through the incremental reader.
          @query = HTTPUtils::parse_query(body(from: :start))
        elsif self['content-type'] =~ /^multipart\/form-data; boundary=(.+)/
          boundary = HTTPUtils::dequote($1)
          @query = HTTPUtils::parse_form_data(body(from: :start), boundary)
        else
          @query = Hash.new
        end
      rescue => ex
        raise HTTPStatus::BadRequest, ex.message
      end
    end

    PrivateNetworkRegexp = /
      ^unknown$|
      ^((::ffff:)?127.0.0.1|::1)$|
      ^(::ffff:)?(10|172\.(1[6-9]|2[0-9]|3[01])|192\.168)\.
    /ixo

    # It's said that all X-Forwarded-* headers will contain more than one
    # (comma-separated) value if the original request already contained one of
    # these headers. Since we could use these values as Host header, we choose
    # the initial(first) value. (apr_table_mergen() adds new value after the
    # existing value with ", " prefix)
    def setup_forwarded_info
      if @forwarded_server = self["x-forwarded-server"]
        @forwarded_server = @forwarded_server.split(",", 2).first
      end
      if @forwarded_proto = self["x-forwarded-proto"]
        @forwarded_proto = @forwarded_proto.split(",", 2).first
      end
      if host_port = self["x-forwarded-host"]
        host_port = host_port.split(",", 2).first
        if host_port =~ /\A(\[[0-9a-fA-F:]+\])(?::(\d+))?\z/
          @forwarded_host = $1
          tmp = $2
        else
          @forwarded_host, tmp = host_port.split(":", 2)
        end
        @forwarded_port = (tmp || (@forwarded_proto == "https" ? 443 : 80)).to_i
      end
      if addrs = self["x-forwarded-for"]
        addrs = addrs.split(",").collect(&:strip)
        addrs.reject!{|ip| PrivateNetworkRegexp =~ ip }
        @forwarded_for = addrs.first
      end
    end

    # :startdoc:
  end
end
