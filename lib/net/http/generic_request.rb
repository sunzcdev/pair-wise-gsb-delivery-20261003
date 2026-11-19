# frozen_string_literal: true
#
# \HTTPGenericRequest is the parent of the Net::HTTPRequest class.
#
# Do not use this directly; instead, use a subclass of Net::HTTPRequest.
#
# == About the Examples
#
# :include: doc/net-http/examples.rdoc
#
class Net::HTTPGenericRequest

  include Net::HTTPHeader

  def initialize(m, reqbody, resbody, uri_or_path, initheader = nil) # :nodoc:
    @method = m
    @request_has_body = reqbody
    @response_has_body = resbody

    if URI === uri_or_path then
      raise ArgumentError, "not an HTTP URI" unless URI::HTTP === uri_or_path
      hostname = uri_or_path.host
      raise ArgumentError, "no host component for URI" unless (hostname && hostname.length > 0)
      @uri = uri_or_path.dup
      @path = uri_or_path.request_uri
      raise ArgumentError, "no HTTP request path given" unless @path
    else
      @uri = nil
      raise ArgumentError, "no HTTP request path given" unless uri_or_path
      raise ArgumentError, "HTTP request path is empty" if uri_or_path.empty?
      @path = uri_or_path.dup
    end

    @decode_content = false

    if Net::HTTP::HAVE_ZLIB then
      if !initheader ||
         !initheader.keys.any? { |k|
           %w[accept-encoding range].include? k.downcase
         } then
        @decode_content = true if @response_has_body
        initheader = initheader ? initheader.dup : {}
        initheader["accept-encoding"] =
          "gzip;q=1.0,deflate;q=0.6,identity;q=0.3"
      end
    end

    initialize_http_header initheader
    self['Accept'] ||= '*/*'
    self['User-Agent'] ||= 'Ruby'
    self['Host'] ||= @uri.authority if @uri
    @body = nil
    @body_stream = nil
    @body_data = nil
  end

  # Returns the string method name for the request:
  #
  #   Net::HTTP::Get.new(uri).method  # => "GET"
  #   Net::HTTP::Post.new(uri).method # => "POST"
  #
  attr_reader :method

  # Returns the string path for the request:
  #
  #   Net::HTTP::Get.new(uri).path # => "/"
  #   Net::HTTP::Post.new('example.com').path # => "example.com"
  #
  attr_reader :path

  # Returns the URI object for the request, or +nil+ if none:
  #
  #   Net::HTTP::Get.new(uri).uri
  #   # => #<URI::HTTPS https://jsonplaceholder.typicode.com/>
  #   Net::HTTP::Get.new('example.com').uri # => nil
  #
  attr_reader :uri

  # Returns +false+ if the request's header <tt>'Accept-Encoding'</tt>
  # has been set manually or deleted
  # (indicating that the user intends to handle encoding in the response),
  # +true+ otherwise:
  #
  #   req = Net::HTTP::Get.new(uri) # => #<Net::HTTP::Get GET>
  #   req['Accept-Encoding']        # => "gzip;q=1.0,deflate;q=0.6,identity;q=0.3"
  #   req.decode_content            # => true
  #   req['Accept-Encoding'] = 'foo'
  #   req.decode_content            # => false
  #   req.delete('Accept-Encoding')
  #   req.decode_content            # => false
  #
  attr_reader :decode_content

  # Returns a string representation of the request:
  #
  #   Net::HTTP::Post.new(uri).inspect # => "#<Net::HTTP::Post POST>"
  #
  def inspect
    "\#<#{self.class} #{@method}>"
  end

  # Returns a string representation of the request with the details for pp:
  #
  #   require 'pp'
  #   post = Net::HTTP::Post.new(uri)
  #   post.inspect # => "#<Net::HTTP::Post POST>"
  #   post.pretty_inspect
  #   # => #<Net::HTTP::Post
  #         POST
  #         path="/"
  #         headers={"accept-encoding" => ["gzip;q=1.0,deflate;q=0.6,identity;q=0.3"],
  #          "accept" => ["*/*"],
  #          "user-agent" => ["Ruby"],
  #          "host" => ["www.ruby-lang.org"]}>
  #
  def pretty_print(q)
    q.object_group(self) {
      q.breakable
      q.text @method
      q.breakable
      q.text "path="; q.pp @path
      q.breakable
      q.text "headers="; q.pp to_hash
    }
  end

  ##
  # Don't automatically decode response content-encoding if the user indicates
  # they want to handle it.

  def []=(key, val) # :nodoc:
    @decode_content = false if key.downcase == 'accept-encoding'

    super key, val
  end

  # Returns whether the request may have a body:
  #
  #   Net::HTTP::Post.new(uri).request_body_permitted? # => true
  #   Net::HTTP::Get.new(uri).request_body_permitted?  # => false
  #
  def request_body_permitted?
    @request_has_body
  end

  # Returns whether the response may have a body:
  #
  #   Net::HTTP::Post.new(uri).response_body_permitted? # => true
  #   Net::HTTP::Head.new(uri).response_body_permitted? # => false
  #
  def response_body_permitted?
    @response_has_body
  end

  def body_exist? # :nodoc:
    warn "Net::HTTPRequest#body_exist? is obsolete; use response_body_permitted?", uplevel: 1 if $VERBOSE
    response_body_permitted?
  end

  # Returns the string body for the request, or +nil+ if there is none:
  #
  #   req = Net::HTTP::Post.new(uri)
  #   req.body # => nil
  #   req.body = '{"title": "foo","body": "bar","userId": 1}'
  #   req.body # => "{\"title\": \"foo\",\"body\": \"bar\",\"userId\": 1}"
  #
  attr_reader :body

  # Sets the body for the request:
  #
  #   req = Net::HTTP::Post.new(uri)
  #   req.body # => nil
  #   req.body = '{"title": "foo","body": "bar","userId": 1}'
  #   req.body # => "{\"title\": \"foo\",\"body\": \"bar\",\"userId\": 1}"
  #
  def body=(str)
    @body = str
    @body_stream = nil
    @body_data = nil
    str
  end

  # Returns the body stream object for the request, or +nil+ if there is none:
  #
  #   req = Net::HTTP::Post.new(uri)          # => #<Net::HTTP::Post POST>
  #   req.body_stream                         # => nil
  #   require 'stringio'
  #   req.body_stream = StringIO.new('xyzzy') # => #<StringIO:0x0000027d1e5affa8>
  #   req.body_stream                         # => #<StringIO:0x0000027d1e5affa8>
  #
  attr_reader :body_stream

  # Sets the body stream for the request:
  #
  #   req = Net::HTTP::Post.new(uri)          # => #<Net::HTTP::Post POST>
  #   req.body_stream                         # => nil
  #   require 'stringio'
  #   req.body_stream = StringIO.new('xyzzy') # => #<StringIO:0x0000027d1e5affa8>
  #   req.body_stream                         # => #<StringIO:0x0000027d1e5affa8>
  #
  def body_stream=(input)
    @body = nil
    @body_stream = input
    @body_data = nil
    input
  end

  def set_body_internal(str)   #:nodoc: internal use only
    raise ArgumentError, "both of body argument and HTTPRequest#body set" if str and (@body or @body_stream)
    self.body = str if str
    if @body.nil? && @body_stream.nil? && @body_data.nil? && request_body_permitted?
      self.body = ''
    end
  end

  # Returns the BodyAdmission object for the most recent transmission of this
  # request, or +nil+ if the request has not been transmitted yet.
  #
  # The admission records whether the upload body was allowed onto the wire,
  # and it explains why:
  #
  #   http.request(req) {|res| ... }
  #   admission = req.body_admission
  #   admission.state        # => :continue
  #   admission.admitted?    # => true
  #   admission.body_sent?   # => true
  #
  # See BodyAdmission for the possible states.
  attr_reader :body_admission

  # Returns whether an automatic retry may send this request's body again.
  #
  # A string body can always be sent again. A body stream, or form data that
  # may hold IO values, is consumed as it is sent, so once any of it has
  # reached the connection a retry would read the source a second time and
  # upload the wrong bytes. In that case the failure is reported based on the
  # bytes that were actually sent, rather than silently re-reading the source.
  def retriable_body? # :nodoc:
    return true if @body
    return true unless @body_stream || @body_data
    admission = @body_admission
    admission.nil? || !admission.body_sent?
  end

  # Returns whether this request announced a body on the wire, through
  # <tt>Content-Length</tt> or <tt>Transfer-Encoding</tt>. When such a request
  # is answered before the body is admitted, the connection still expects the
  # announced bytes and cannot carry another request.
  def body_declared? # :nodoc:
    !!content_length || chunked?
  end

  # Returns +true+ if the connection was left in the middle of this request's
  # body, so it cannot be reused for another request:
  #
  # - A body was announced but declined, so the peer still waits for it.
  # - Writing the body started but did not finish.
  #
  # This is derived from what actually happened, not from what was intended.
  def upload_incomplete? # :nodoc:
    return false unless body_declared?
    admission = @body_admission
    return false if admission.nil?
    admission.declined? || (admission.body_sent? && !admission.body_finished?)
  end

  # The inspectable record of a single body admission decision.
  #
  # When a request asks the server to accept the body first (through the
  # <tt>Expect: 100-continue</tt> header), the body source must not be read
  # until one of these happens:
  #
  # - The server sends an actual <tt>100 Continue</tt> response.
  # - The continue timeout expires, which explicitly adopts the wait-expiry
  #   policy and allows the body to be sent anyway.
  #
  # Informational responses other than <tt>100 Continue</tt> (for example
  # <tt>103 Early Hints</tt>) neither admit nor refuse the body; the client
  # keeps waiting. Any final response received while waiting (for example
  # <tt>417 Expectation Failed</tt>) means the server declined the upload, so
  # the body source is left untouched.
  #
  # The admission decision (#state) and the sending facts (#body_bytes,
  # #body_sent?) are recorded separately: a <tt>:continue</tt> decision stays
  # readable after the body has been written. A response that arrives after
  # the body started is handled according to those facts, not as if the body
  # had never been sent.
  class BodyAdmission
    # The admission decision (#state):
    #
    # <tt>:pending</tt> :: Created, but no decision has been made yet.
    # <tt>:not_requested</tt> :: No applicable <tt>Expect: 100-continue</tt>;
    #                            the body may be sent immediately.
    # <tt>:waiting</tt> :: Waiting for <tt>100 Continue</tt> or a final response.
    # <tt>:continue</tt> :: A real <tt>100 Continue</tt> admitted the body.
    # <tt>:timeout</tt> :: The continue timeout expired; the wait-expiry policy
    #                      admitted the body.
    # <tt>:declined</tt> :: A final response arrived before the body was
    #                       admitted; the source was not read.
    attr_reader :state

    # The response that decided the admission: the <tt>100 Continue</tt> that
    # admitted the body, or the final response that declined it. For an
    # informational response seen while waiting, this is the most recent one.
    # +nil+ while no such response has been seen.
    attr_reader :response

    # The number of informational responses other than <tt>100 Continue</tt>
    # (such as <tt>103 Early Hints</tt>) seen while waiting.
    attr_reader :informational_responses

    # The number of bytes the connection accepted for the body. For a chunked
    # upload this includes the chunk framing written around the payload.
    attr_reader :body_bytes

    # A short human-readable explanation of the admission decision, if any.
    attr_reader :reason

    def initialize # :nodoc:
      @state = :pending
      @continue_requested = false
      @response = nil
      @informational_responses = 0
      @body_bytes = 0
      @body_started = false
      @body_finished = false
      @reason = nil
    end

    # Returns whether the request asked for a <tt>100 Continue</tt> exchange.
    def continue_requested?
      @continue_requested
    end

    # Returns +true+ if the body was, or may be, transmitted.
    #
    # This is +false+ when a final response declined the upload before the
    # body started, in which case the source was never read.
    def admitted?
      case @state
      when :not_requested, :continue, :timeout
        true
      else
        false
      end
    end

    # Returns +true+ if the body has started to be written.
    def body_sent?
      @body_started
    end

    # Returns +true+ if the body was written completely.
    def body_finished?
      @body_finished
    end

    # Returns +true+ while waiting for the server's decision.
    def waiting?
      @state == :waiting
    end

    # Returns +true+ if a final response declined the upload.
    def declined?
      @state == :declined
    end

    def inspect # :nodoc:
      "\#<#{self.class} state=#{@state} admitted=#{admitted?} " \
        "body_sent=#{body_sent?} body_finished=#{body_finished?} " \
        "body_bytes=#{@body_bytes} " \
        "informational_responses=#{@informational_responses}>"
    end

    # Records that no wait was needed, so the body may be sent immediately.
    def adopt_no_wait(reason) # :nodoc:
      @state = :not_requested
      @reason = reason
      self
    end

    # Records that the request asked for a <tt>100 Continue</tt> and is now
    # waiting for the server's decision.
    def wait! # :nodoc:
      @continue_requested = true
      @state = :waiting
    end

    def admit_continue!(res) # :nodoc:
      @response = res
      @state = :continue
    end

    # Records an explicit decision to send the body after the continue timeout
    # expired without a 100 Continue.
    def adopt_timeout! # :nodoc:
      @state = :timeout
      @reason = "continue timeout expired"
      self
    end

    def observe_informational!(res) # :nodoc:
      @informational_responses += 1
      @response = res
    end

    def decline!(res) # :nodoc:
      @response = res
      @state = :declined
      @reason = "final response #{res.code} received before the body"
      self
    end

    def start_body! # :nodoc:
      @body_started = true
    end

    def finish_body!(bytes) # :nodoc:
      @body_bytes += bytes
      @body_finished = true
    end

    def abort_body!(bytes) # :nodoc:
      @body_bytes += bytes
    end
  end

  #
  # write
  #

  def exec(sock, ver, path)   #:nodoc: internal use only
    @body_admission = BodyAdmission.new
    if @body
      send_request_with_body sock, ver, path, @body
    elsif @body_stream
      send_request_with_body_stream sock, ver, path, @body_stream
    elsif @body_data
      send_request_with_body_data sock, ver, path, @body_data
    else
      write_header sock, ver, path
    end
  end

  def update_uri(addr, port, ssl) # :nodoc: internal use only
    # reflect the connection and @path to @uri
    return unless @uri

    if ssl
      scheme = 'https'
      klass = URI::HTTPS
    else
      scheme = 'http'
      klass = URI::HTTP
    end

    if host = self['host']
      host = URI.parse("//#{host}").host # Remove a port component from the existing Host header
    elsif host = @uri.host
    else
     host = addr
    end
    # convert the class of the URI
    if @uri.is_a?(klass)
      @uri.host = host
      @uri.port = port
    else
      @uri = klass.new(
        scheme, @uri.userinfo,
        host, port, nil,
        @uri.path, nil, @uri.query, nil)
    end
  end

  private

  # :stopdoc:

  class Chunker #:nodoc:
    def initialize(sock)
      @sock = sock
      @prev = nil
    end

    def write(buf)
      # avoid memcpy() of buf, buf can huge and eat memory bandwidth
      rv = buf.bytesize
      @sock.write("#{rv.to_s(16)}\r\n", buf, "\r\n")
      rv
    end

    def finish
      @sock.write("0\r\n\r\n")
    end
  end

  # Wraps the socket used for a body upload so the number of bytes that
  # actually reached it can be reported, even when writing fails part way
  # through. The count is what the socket accepted, so for a chunked upload it
  # includes the chunk framing the client wrote around the payload.
  class BodyWriter #:nodoc:
    def initialize(sock)
      @sock = sock
      @bytes = 0
    end

    attr_reader :bytes

    def write(*strs)
      written = @sock.write(*strs)
      @bytes += written if Integer === written
      written
    end

    def <<(str)
      write(str)
      self
    end
  end

  def send_request_with_body(sock, ver, path, body)
    self.content_length = body.bytesize
    delete 'Transfer-Encoding'
    write_header sock, ver, path
    return unless wait_for_continue sock, ver
    writer = BodyWriter.new(sock)
    send_upload(sock, "request body (#{body.bytesize} bytes)", writer) {
      writer.write body
    }
  end

  def send_request_with_body_stream(sock, ver, path, f)
    unless content_length() or chunked?
      raise ArgumentError,
          "Content-Length not given and Transfer-Encoding is not `chunked'"
    end
    write_header sock, ver, path
    # The upload source is not touched until the server has admitted the
    # body: either a real 100 Continue arrived, or the continue timeout
    # expired and the wait-expiry policy allowed sending anyway.
    return unless wait_for_continue sock, ver
    writer = BodyWriter.new(sock)
    if chunked?
      chunker = Chunker.new(writer)
      send_upload(sock, "chunked body stream", writer) {
        IO.copy_stream(f, chunker)
        chunker.finish
      }
    else
      send_upload(sock, "body stream", writer) {
        IO.copy_stream(f, writer)
      }
    end
  end

  def send_request_with_body_data(sock, ver, path, params)
    if /\Amultipart\/form-data\z/i !~ self.content_type
      self.content_type = 'application/x-www-form-urlencoded'
      return send_request_with_body(sock, ver, path, URI.encode_www_form(params))
    end

    opt = @form_option.dup
    require 'securerandom' unless defined?(SecureRandom)
    opt[:boundary] ||= SecureRandom.urlsafe_base64(40)
    self.set_content_type(self.content_type, boundary: opt[:boundary])
    if chunked?
      write_header sock, ver, path
      return unless wait_for_continue sock, ver
      writer = BodyWriter.new(sock)
      send_upload(sock, "chunked multipart body", writer) {
        encode_multipart_form_data(writer, params, opt)
      }
    else
      require 'tempfile'
      # The multipart body is spooled to a temporary file before the wait, so
      # the request header can go out with the exact Content-Length while the
      # upload source has not been sent. The file is only read into the
      # connection once the body is admitted.
      file = Tempfile.new('multipart')
      begin
        file.binmode
        encode_multipart_form_data(file, params, opt)
        file.rewind
        self.content_length = file.size
        write_header sock, ver, path
        return unless wait_for_continue sock, ver
        writer = BodyWriter.new(sock)
        send_upload(sock, "multipart body", writer) {
          IO.copy_stream(file, writer)
        }
      ensure
        file.close(true)
      end
    end
  end

  def encode_multipart_form_data(out, params, opt)
    charset = opt[:charset]
    boundary = opt[:boundary]
    require 'securerandom' unless defined?(SecureRandom)
    boundary ||= SecureRandom.urlsafe_base64(40)
    if /[\r\n]/.match?(boundary.to_s)
      raise ArgumentError, "multipart boundary cannot include CR/LF"
    end
    chunked_p = chunked?

    buf = +''
    params.each do |key, value, h={}|
      key = quote_string(key, charset)
      filename =
        h.key?(:filename) ? h[:filename] :
        value.respond_to?(:to_path) ? File.basename(value.to_path) :
        nil

      buf << "--#{boundary}\r\n"
      if filename
        filename = quote_string(filename, charset)
        type = (h[:content_type] || 'application/octet-stream').to_s
        if /[\r\n]/.match?(type)
          raise ArgumentError, "field content type cannot include CR/LF"
        end
        buf << "Content-Disposition: form-data; " \
          "name=\"#{key}\"; filename=\"#{filename}\"\r\n" \
          "Content-Type: #{type}\r\n\r\n"
        if !out.respond_to?(:write) || !value.respond_to?(:read)
          # if +out+ is not an IO or +value+ is not an IO
          buf << (value.respond_to?(:read) ? value.read : value)
        elsif value.respond_to?(:size) && chunked_p
          # if +out+ is an IO and +value+ is a File, use IO.copy_stream
          flush_buffer(out, buf, chunked_p)
          out << "%x\r\n" % value.size if chunked_p
          IO.copy_stream(value, out)
          out << "\r\n" if chunked_p
        else
          # +out+ is an IO, and +value+ is not a File but an IO
          flush_buffer(out, buf, chunked_p)
          1 while flush_buffer(out, value.read(4096), chunked_p)
        end
      else
        # non-file field:
        #   HTML5 says, "The parts of the generated multipart/form-data
        #   resource that correspond to non-file fields must not have a
        #   Content-Type header specified."
        buf << "Content-Disposition: form-data; name=\"#{key}\"\r\n\r\n"
        buf << (value.respond_to?(:read) ? value.read : value)
      end
      buf << "\r\n"
    end
    buf << "--#{boundary}--\r\n"
    flush_buffer(out, buf, chunked_p)
    out << "0\r\n\r\n" if chunked_p
  end

  def quote_string(str, charset)
    str = str.encode(charset, fallback:->(c){'&#%d;'%c.encode("UTF-8").ord}) if charset
    if /[\r\n]/.match?(str)
      raise ArgumentError, "multipart field name or filename cannot include CR/LF"
    end
    str.gsub(/[\\"]/, '\\\\\&')
  end

  def flush_buffer(out, buf, chunked_p)
    return unless buf
    out << "%x\r\n"%buf.bytesize if chunked_p
    out << buf
    out << "\r\n" if chunked_p
    buf.clear
  end

  ##
  # Decides whether the upload body may be sent, and records the decision in
  # the request's BodyAdmission.
  #
  # When the request is speaking HTTP/1.1 and carries <tt>Expect:
  # 100-continue</tt>, the server gets the first word: a real <tt>100
  # Continue</tt> admits the body, while a final response declines it and is
  # thrown to the request loop so the body source is never read.
  #
  # Other informational responses (such as <tt>103 Early Hints</tt>) are not a
  # <tt>100 Continue</tt>; they say nothing about whether the body may be sent,
  # so the client keeps waiting.
  #
  # If nothing arrives within the continue timeout, the wait-expiry policy is
  # adopted explicitly and the body is sent anyway. With no continue timeout
  # configured, the established default policy applies and the body is sent
  # without waiting.
  #
  # Returns +true+ if the body may be sent; otherwise throws +:response+ with
  # the server's final response.

  def wait_for_continue(sock, ver)
    admission = current_admission
    unless continue_expected?(ver)
      admission.adopt_no_wait("no Expect: 100-continue header")
      return true
    end
    if sock.continue_timeout.nil?
      admission.adopt_no_wait("continue_timeout is not set")
      return true
    end

    admission.wait!
    timeout = sock.continue_timeout
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + timeout
    debug_upload(sock, "waiting up to #{timeout.inspect}s for 100 Continue")
    loop do
      unless buffered_response?(sock) || wait_for_more(sock, deadline)
        admission.adopt_timeout!
        debug_upload(sock, "continue timeout expired; adopting wait-expiry policy and sending body")
        return true
      end
      res = Net::HTTPResponse.read_new(sock)

      if res.kind_of?(Net::HTTPContinue)
        admission.admit_continue!(res)
        debug_upload(sock, "received 100 Continue; body admitted")
        return true
      end
      # Any other 1xx is informational only: it does not mean the server
      # accepted the upload, so keep waiting for the real decision.
      if res.kind_of?(Net::HTTPInformation)
        admission.observe_informational!(res)
        if admission.informational_responses > Net::HTTPResponse::MAX_INFORMATIONAL_RESPONSES
          raise Net::HTTPBadResponse, 'too many informational responses'
        end
        debug_upload(sock, "received informational #{res.code}; still waiting for 100 Continue")
        next
      end
      # A final response before the body started: the server refused the
      # upload, so the source must stay untouched.
      admission.decline!(res)
      debug_upload(sock, "received final response #{res.code} before body; upload declined")
      throw :response, res
    end
  end

  def continue_expected?(ver)   # :nodoc:
    ver >= '1.1' and @header['expect'] and
      @header['expect'].include?('100-continue')
  end

  # Waits until the deadline for the underlying IO to become readable. Once the
  # deadline passes, a zero-timeout poll still picks up a response that is
  # already waiting, and otherwise returns +false+.
  def wait_for_more(sock, deadline)   # :nodoc:
    remaining = deadline - Process.clock_gettime(Process::CLOCK_MONOTONIC)
    io = sock.io
    io = io.to_io if io.respond_to?(:to_io)
    io.wait_readable(remaining > 0 ? remaining : 0) ? true : false
  end

  # Returns +true+ if the buffered reader already holds bytes from an earlier
  # read, for example when the server sent <tt>103 Early Hints</tt> and the
  # <tt>100 Continue</tt> in a single segment. In that case there is no need to
  # wait on the socket before parsing the next response.
  def buffered_response?(sock)   # :nodoc:
    if sock.respond_to?(:rbuf_size, true)
      sock.send(:rbuf_size) > 0
    else
      false
    end
  end

  # Runs the block that writes the body, recording the admission state and the
  # actual number of bytes written. Diagnostics distinguish this sending phase
  # from the waiting phase above.

  def send_upload(sock, description, writer)   # :nodoc:
    admission = current_admission
    admission.start_body!
    debug_upload(sock, "sending #{description}")
    begin
      yield
    rescue => exception
      # The source may have been read partly; record what actually went out so
      # callers can tell how much of the upload happened.
      admission.abort_body!(writer.bytes)
      debug_upload(sock, "body send failed (#{exception.class}) after #{writer.bytes} bytes; sending aborted")
      raise
    end
    admission.finish_body!(writer.bytes)
    debug_upload(sock, "sent #{writer.bytes} body bytes")
    writer.bytes
  end

  def current_admission   # :nodoc:
    @body_admission ||= BodyAdmission.new
  end

  def debug_upload(sock, message)   # :nodoc:
    output = sock.respond_to?(:debug_output) ? sock.debug_output : nil
    output << "expect-continue: #{message}\n" if output
  end

  def write_header(sock, ver, path)
    reqline = "#{@method} #{path} HTTP/#{ver}"
    if /[\r\n]/ =~ reqline
      raise ArgumentError, "A Request-Line must not contain CR or LF"
    end
    buf = +''
    buf << reqline << "\r\n"
    each_capitalized do |k,v|
      buf << "#{k}: #{v}\r\n"
    end
    buf << "\r\n"
    sock.write buf
  end

end
