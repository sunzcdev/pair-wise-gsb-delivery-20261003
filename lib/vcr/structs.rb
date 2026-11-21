require 'delegate'
require 'time'

module VCR
  # @private
  module Normalizers
    # @private
    module Body
      def self.included(klass)
        klass.extend ClassMethods
      end

      # @private
      module ClassMethods
        def body_from(hash_or_string)
          return hash_or_string unless hash_or_string.is_a?(Hash)
          hash = hash_or_string

          if hash.has_key?('base64_string')
            base64_decoded = hash['base64_string'].unpack1('m')
            force_encode_string(base64_decoded, hash['encoding'])
          else
            try_encode_string(hash['string'], hash['encoding'])
          end
        end

        def force_encode_string(string, encoding)
          return string unless encoding
          string.force_encoding(encoding)
        end

        def try_encode_string(string, encoding_name)
          return string if encoding_name.nil? || string.nil?

          encoding = Encoding.find(encoding_name)
          return string if string.encoding == encoding

          # ASCII-8BIT just means binary, so encoding to it is nonsensical
          # and yet "\u00f6".encode("ASCII-8BIT") raises an error.
          # Instead, we'll force encode it (essentially just tagging it as binary)
          return string.dup.force_encoding(encoding) if encoding == Encoding::BINARY

          string.encode(encoding)
        rescue EncodingError => e
          struct_type = name.split('::').last.downcase
          warn "VCR: got `#{e.class.name}: #{e.message}` while trying to encode the #{string.encoding.name} " +
                "#{struct_type} body to the original body encoding (#{encoding}). Consider using the " +
                "`:preserve_exact_body_bytes` option to work around this."
          return string
        end
      end

      def initialize(*args)
        super

        if body && !body.is_a?(String)
          raise ArgumentError, "#{self.class} initialized with an invalid (non-String) body of class #{body.class}: #{body.inspect}."
        end

        # Ensure that the body is a raw string, in case the string instance
        # has been subclassed or extended with additional instance variables
        # or attributes, so that it is serialized to YAML as a raw string.
        # This is needed for rest-client.  See this ticket for more info:
        # http://github.com/myronmarston/vcr/issues/4
        self.body = String.new(body.to_s)
      end

    private

      def serializable_body
        # Ensure it's just a string, and not a string with some
        # extra state, as such strings serialize to YAML with
        # all the extra state.
        body = String.new(self.body.to_s)

        if VCR.configuration.preserve_exact_body_bytes_for?(self)
          base64_encoded = [body].pack('m')
          base_body_hash(body).merge('base64_string' => base64_encoded)
        else
          base_body_hash(body).merge('string' => body)
        end
      end

      def base_body_hash(body)
        { 'encoding' => body.encoding.name }
      end
    end

    # @private
    module Header
      def initialize(*args)
        super
        normalize_headers
      end

    private

      def normalize_headers
        new_headers = {}
        @normalized_header_keys = Hash.new {|h,k| k }

        headers.each do |k, v|
          val_array = case v
            when Array then v
            when nil then []
            else [v]
          end

          new_headers[String.new(k)] = convert_to_raw_strings(val_array)
          @normalized_header_keys[k.downcase] = k
        end if headers

        self.headers = new_headers
      end

      def header_key(key)
        key = @normalized_header_keys[key.downcase]
        key if headers.has_key? key
      end

      def get_header(key)
        key = header_key(key)
        headers[key] if key
      end

      def edit_header(key, value = nil)
        if key = header_key(key)
          value ||= yield headers[key]
          headers[key] = Array(value)
        end
      end

      def delete_header(key)
        if key = header_key(key)
          @normalized_header_keys.delete key.downcase
          headers.delete key
        end
      end

      def convert_to_raw_strings(array)
        # Ensure the values are raw strings.
        # Apparently for Paperclip uploads to S3, headers
        # get serialized with some extra stuff which leads
        # to a seg fault. See this issue for more info:
        # https://github.com/myronmarston/vcr/issues#issue/39
        array.map do |v|
          case v
            when String; String.new(v)
            when Array; convert_to_raw_strings(v)
            else v
          end
        end
      end
    end
  end

  # The request of an {HTTPInteraction}.
  #
  # @attr [Symbol] method the HTTP method (i.e. :head, :options, :get, :post, :put, :patch or :delete)
  # @attr [String] uri the request URI
  # @attr [String, nil] body the request body
  # @attr [Hash{String => Array<String>}] headers the request headers
  class Request < Struct.new(:method, :uri, :body, :headers)
    include Normalizers::Header
    include Normalizers::Body

    def initialize(*args)
      skip_port_stripping = false
      if args.last == :skip_port_stripping
        skip_port_stripping = true
        args.pop
      end

      super(*args)
      self.method = self.method.to_s.downcase.to_sym if self.method
      self.uri = without_standard_port(self.uri) unless skip_port_stripping
    end

    # Builds a serializable hash from the request data.
    #
    # @return [Hash] hash that represents this request and can be easily
    #  serialized.
    # @see Request.from_hash
    def to_hash
      {
        'method'  => method.to_s,
        'uri'     => uri,
        'body'    => serializable_body,
        'headers' => headers
      }
    end

    # Constructs a new instance from a hash.
    #
    # @param [Hash] hash the hash to use to construct the instance.
    # @return [Request] the request
    def self.from_hash(hash)
      method = hash['method']
      method &&= method.to_sym
      new method,
          hash['uri'],
          body_from(hash['body']),
          hash['headers'],
          :skip_port_stripping
    end

    # Parses the URI using the configured `uri_parser`.
    #
    # @return [#schema, #host, #port, #path, #query] A parsed URI object.
    def parsed_uri
      VCR.configuration.uri_parser.parse(uri)
    end

    @@object_method = Object.instance_method(:method)
    def method(*args)
      return super if args.empty?
      @@object_method.bind(self).call(*args)
    end

    # Decorates a {Request} with its current type.
    class Typed < DelegateClass(self)
      # @return [Symbol] One of `:ignored`, `:stubbed`, `:recordable` or `:unhandled`.
      attr_reader :type

      # @param [Request] request the request
      # @param [Symbol] type the type. Should be one of `:ignored`, `:stubbed`, `:recordable` or `:unhandled`.
      def initialize(request, type)
        @type = type
        super(request)
      end

      # @return [Boolean] whether or not this request is being ignored
      def ignored?
        type == :ignored
      end

      # @return [Boolean] whether or not this request is being stubbed by VCR
      # @see #externally_stubbed?
      # @see #stubbed?
      def stubbed_by_vcr?
        type == :stubbed_by_vcr
      end

      # @return [Boolean] whether or not this request is being stubbed by an
      #  external library (such as WebMock).
      # @see #stubbed_by_vcr?
      # @see #stubbed?
      def externally_stubbed?
        type == :externally_stubbed
      end

      # @return [Boolean] whether or not this request will be recorded.
      def recordable?
        type == :recordable
      end

      # @return [Boolean] whether or not VCR knows how to handle this request.
      def unhandled?
        type == :unhandled
      end

      # @return [Boolean] whether or not this request will be made for real.
      # @note VCR allows `:ignored` and `:recordable` requests to be made for real.
      def real?
        ignored? || recordable?
      end

      # @return [Boolean] whether or not this request will be stubbed.
      #  It may be stubbed by an external library or by VCR.
      # @see #stubbed_by_vcr?
      # @see #externally_stubbed?
      def stubbed?
        stubbed_by_vcr? || externally_stubbed?
      end

      undef method
    end

    # Provides fiber-awareness for the {VCR::Configuration#around_http_request} hook.
    class FiberAware < DelegateClass(Typed)
      # Yields the fiber so the request can proceed.
      #
      # @return [VCR::Response] the response from the request
      def proceed
        Fiber.yield
      end

      # Builds a proc that allows the request to proceed when called.
      # This allows you to treat the request as a proc and pass it on
      # to a method that yields (at which point the request will proceed).
      #
      # @return [Proc] the proc
      def to_proc
        proc { proceed }
      end

      undef method
    end

  private

    def without_standard_port(uri)
      return uri if uri.nil?
      u = parsed_uri
      return uri unless [['http', 80], ['https', 443]].include?([u.scheme, u.port])
      u.port = nil
      u.to_s
    end
  end

  # The response of an {HTTPInteraction}.
  #
  # @attr [ResponseStatus] status the status of the response
  # @attr [Hash{String => Array<String>}] headers the response headers
  # @attr [String] body the response body
  # @attr [nil, String] http_version the HTTP version
  # @attr [Hash] adapter_metadata Additional metadata used by a specific VCR adapter.
  class Response < Struct.new(:status, :headers, :body, :http_version, :adapter_metadata)
    include Normalizers::Header
    include Normalizers::Body

    def initialize(*args)
      super(*args)
      self.adapter_metadata ||= {}
    end

    # Builds a serializable hash from the response data.
    #
    # @return [Hash] hash that represents this response
    #  and can be easily serialized.
    # @see Response.from_hash
    def to_hash
      {
        'status'       => status.to_hash,
        'headers'      => headers,
        'body'         => serializable_body
      }.tap do |hash|
        hash['http_version']     = http_version if http_version
        hash['adapter_metadata'] = adapter_metadata unless adapter_metadata.empty?
      end
    end

    # Constructs a new instance from a hash.
    #
    # @param [Hash] hash the hash to use to construct the instance.
    # @return [Response] the response
    def self.from_hash(hash)
      new ResponseStatus.from_hash(hash.fetch('status', {})),
          hash['headers'],
          body_from(hash['body']),
          hash['http_version'],
          hash['adapter_metadata']
    end

    # Updates the Content-Length response header so that it is
    # accurate for the response body.
    def update_content_length_header
      edit_header('Content-Length') { body ? body.bytesize.to_s : '0' }
    end

    # The type of encoding.
    #
    # @return [String] encoding type
    def content_encoding
      enc = get_header('Content-Encoding') and enc.first
    end

    # Checks if the type of encoding is one of "gzip" or "deflate".
    # An empty body is never compressed, whatever the header says: a HEAD
    # response carries the Content-Encoding of the GET it describes.
    def compressed?
      !body.to_s.empty? && %w[ gzip deflate ].include?(content_encoding)
    end

    # Checks if VCR decompressed the response body
    def vcr_decompressed?
      adapter_metadata['vcr_decompressed']
    end

    # Decodes the compressed body and deletes evidence that it was ever compressed.
    #
    # @return self
    # @raise [VCR::Errors::UnknownContentEncodingError] if the content encoding
    #  is not a known encoding.
    def decompress
      return self if body.to_s.empty?

      self.class.decompress(body, content_encoding) { |new_body|
        self.body = new_body
        update_content_length_header
        adapter_metadata['vcr_decompressed'] = content_encoding
        delete_header('Content-Encoding')
      }
      return self
    end

    # Recompresses the decompressed body according to adapter metadata.
    #
    # @raise [VCR::Errors::UnknownContentEncodingError] if the content encoding
    #  stored in the adapter metadata is unknown
    def recompress
      type = adapter_metadata['vcr_decompressed']
      new_body = begin
        case type
        when 'gzip'
          body_str = String.new
          writer = Zlib::GzipWriter.new(StringIO.new(body_str), encoding: 'ASCII-8BIT')
          writer.write(body)
          writer.close
          body_str
        when 'deflate'
          Zlib::Deflate.inflate(body)
        when 'identity', NilClass
          nil
        else
          raise Errors::UnknownContentEncodingError, "unknown content encoding: #{type}"
        end
      end
      if new_body
        self.body = new_body
        update_content_length_header
        headers['Content-Encoding'] = type
      end
    end

    begin
      require 'zlib'
      require 'stringio'
      HAVE_ZLIB = true
    rescue LoadError
      HAVE_ZLIB = false
    end

    # Decode string compressed with gzip or deflate
    #
    # @raise [VCR::Errors::UnknownContentEncodingError] if the content encoding
    #  is not a known encoding.
    def self.decompress(body, type)
      unless HAVE_ZLIB
        warn "VCR: cannot decompress response; Zlib not available"
        return
      end

      case type
      when 'gzip'
        yield Zlib::GzipReader.new(StringIO.new(body), encoding: 'ASCII-8BIT').read
      when 'deflate'
        yield Zlib::Inflate.inflate(body)
      when 'identity', NilClass
        return
      else
        raise Errors::UnknownContentEncodingError, "unknown content encoding: #{type}"
      end
    end
  end

  # The response status of an {HTTPInteraction}.
  #
  # @attr [Integer] code the HTTP status code
  # @attr [String] message the HTTP status message (e.g. "OK" for a status of 200)
  class ResponseStatus < Struct.new(:code, :message)
    # Builds a serializable hash from the response status data.
    #
    # @return [Hash] hash that represents this response status
    #  and can be easily serialized.
    # @see ResponseStatus.from_hash
    def to_hash
      {
        'code' => code, 'message' => message
      }
    end

    # Constructs a new instance from a hash.
    #
    # @param [Hash] hash the hash to use to construct the instance.
    # @return [ResponseStatus] the response status
    def self.from_hash(hash)
      new hash['code'], hash['message']
    end
  end

  # Represents a single interaction over HTTP, containing a request and a response.
  #
  # @attr [Request] request the request
  # @attr [Response] response the response
  # @attr [Time] recorded_at when this HTTP interaction was recorded
  class HTTPInteraction < Struct.new(:request, :response, :recorded_at)
    def initialize(*args)
      super
      self.recorded_at ||= Time.now
    end

    # Builds a serializable hash from the HTTP interaction data.
    #
    # @return [Hash] hash that represents this HTTP interaction
    #  and can be easily serialized.
    # @see HTTPInteraction.from_hash
    def to_hash
      {
        'request'     => request.to_hash,
        'response'    => response.to_hash,
        'recorded_at' => recorded_at.httpdate
      }
    end

    # Constructs a new instance from a hash.
    #
    # @param [Hash] hash the hash to use to construct the instance.
    # @return [HTTPInteraction] the HTTP interaction
    def self.from_hash(hash)
      new Request.from_hash(hash.fetch('request', {})),
          Response.from_hash(hash.fetch('response', {})),
          Time.httpdate(hash.fetch('recorded_at'))
    end

    # @return [HookAware] an instance with additional capabilities
    #  suitable for use in `before_record` and `before_playback` hooks.
    def hook_aware
      HookAware.new(self)
    end

    # Decorates an {HTTPInteraction} with additional methods useful
    # for a `before_record` or `before_playback` hook.
    class HookAware < DelegateClass(HTTPInteraction)
      def initialize(http_interaction)
        @ignored = false
        super
      end

      # Flags the HTTP interaction so that VCR ignores it. This is useful in
      # a {VCR::Configuration#before_record} or {VCR::Configuration#before_playback}
      # hook so that VCR does not record or play it back.
      # @see #ignored?
      def ignore!
        @ignored = true
      end

      # @return [Boolean] whether or not this HTTP interaction should be ignored.
      # @see #ignore!
      def ignored?
        !!@ignored
      end

      # The locations within an HTTP interaction in which a scoped
      # placeholder substitution may be performed.
      URI_SCOPES = [:uri, :scheme, :host, :port, :path, :query].freeze
      BODY_SCOPES = [:body, :request_body, :response_body].freeze
      HEADER_SCOPE = :headers

      # Replaces a string in any part of the HTTP interaction (headers, request body,
      # response body, etc) with the given replacement text.
      #
      # When a `scope` is given, the replacement is restricted to the registered
      # locations only, so an identical string appearing anywhere else in the
      # interaction is left untouched. The same scope must be used when recording
      # and playing back so that only the registered range is restored.
      #
      # @example Replace a value only in the request URI query string
      #   interaction.filter!('secret', '<SECRET>', scope: :query)
      #
      # @example Replace a value only in a specific request header
      #   interaction.filter!('secret', '<SECRET>', scope: { request: { headers: 'X-Api-Key' } })
      #
      # @param [#to_s] text the text to replace
      # @param [#to_s] replacement_text the text to put in its place
      # @param [Symbol, Hash, Array] scope the location(s) in which to replace.
      #  Valid symbols are `:uri`, `:scheme`, `:host`, `:port`, `:path`, `:query`,
      #  `:headers`, `:body`, `:request_body` and `:response_body`. A hash restricts
      #  the substitution to `:request` and/or `:response`, e.g.
      #  `{ headers: 'X-Secret' }`, `{ request: :query }` or
      #  `{ response: { headers: ['X-A', 'X-B'] } }`. Arrays combine several scopes.
      #  When omitted, every part of the interaction is filtered (the default behavior).
      # @return [VCR::HTTPInteraction::HookAware] self
      def filter!(text, replacement_text, scope: nil)
        text, replacement_text = text.to_s, replacement_text.to_s
        return self if [text, replacement_text].any? { |t| t.empty? }
        return filter_object!(self, text, replacement_text) if scope.nil?

        scopes = scope.is_a?(Array) ? scope : [scope]
        unknown = scopes.reject { |s| valid_scope?(s) }
        raise ArgumentError, "Unknown placeholder filter scope: #{unknown.inspect}" if unknown.any?

        scopes.each { |s| filter_scope!(s, text, replacement_text) }
        self
      end

    private

      def valid_scope?(scope)
        case scope
        when Symbol
          URI_SCOPES.include?(scope) || BODY_SCOPES.include?(scope) || scope == HEADER_SCOPE
        when Hash
          scope.keys.all? do |side|
            case side
            when :headers then true
            when :request, :response
              values = wrap_scope(scope[side])
              values.all? do |s|
                s.is_a?(Symbol) ? valid_scope?(s) : s.is_a?(Hash) && s.keys == [:headers]
              end
            else false
            end
          end
        else false
        end
      end

      # Unlike `Array()`, this never flattens a hash into key/value pairs.
      def wrap_scope(scope)
        scope.is_a?(Array) ? scope : [scope]
      end

      def filter_scope!(scope, text, replacement_text)
        case scope
        when Symbol then filter_symbol_scope!(scope, text, replacement_text)
        when Hash then filter_hash_scope!(scope, text, replacement_text)
        end
      end

      def filter_symbol_scope!(scope, text, replacement_text)
        if URI_SCOPES.include?(scope)
          filter_uri_scope!(:request, scope, text, replacement_text)
        elsif scope == HEADER_SCOPE
          filter_headers!(request, text, replacement_text)
          filter_headers!(response, text, replacement_text)
        elsif scope == :body
          filter_body!(request, text, replacement_text)
          filter_body!(response, text, replacement_text)
        elsif scope == :request_body
          filter_body!(request, text, replacement_text)
        elsif scope == :response_body
          filter_body!(response, text, replacement_text)
        end
      end

      def filter_hash_scope!(scope, text, replacement_text)
        filter_header_names!(scope[:headers], text, replacement_text) if scope.key?(:headers)

        [:request, :response].each do |side|
          next unless scope.key?(side)
          wrap_scope(scope[side]).each do |side_scope|
            case side_scope
            when Symbol then filter_side_symbol_scope!(side, side_scope, text, replacement_text)
            when Hash then filter_header_names!(side_scope[:headers], text, replacement_text, side)
            end
          end
        end
      end

      def filter_side_symbol_scope!(side, scope, text, replacement_text)
        message = public_send(side)
        if URI_SCOPES.include?(scope)
          filter_uri_scope!(side, scope, text, replacement_text)
        elsif scope == HEADER_SCOPE
          filter_headers!(message, text, replacement_text)
        elsif scope == :body || scope == :"#{side}_body"
          filter_body!(message, text, replacement_text)
        end
      end

      def filter_header_names!(names, text, replacement_text, side = nil)
        messages = side ? [public_send(side)] : [request, response]
        Array(names).each do |name|
          messages.each { |message| filter_named_header!(message, name, text, replacement_text) }
        end
      end

      def filter_named_header!(message, name, text, replacement_text)
        actual_key = message.headers.keys.find { |key| key.downcase == name.to_s.downcase }
        return unless actual_key

        values = message.headers[actual_key]
        message.headers[actual_key] = values.map { |v| v.is_a?(String) ? v.gsub(text, replacement_text) : v }
      end

      def filter_headers!(message, text, replacement_text)
        message.headers.each_key do |name|
          filter_named_header!(message, name, text, replacement_text)
        end
      end

      def filter_body!(message, text, replacement_text)
        return unless message.body.is_a?(String) && message.body.include?(text)
        message.body = message.body.gsub(text, replacement_text)
      end

      # Matches the scheme and authority sections of an absolute URI, e.g.
      # the `https://user:pass@example.com:8080` part of
      # `https://user:pass@example.com:8080/path?query`.
      URI_AUTHORITY_PATTERN = /\A([^:]*:\/\/)([^\/?#]*)/

      def filter_uri_scope!(side, scope, text, replacement_text)
        message = public_send(side)
        return unless message.respond_to?(:parsed_uri) && message.uri.is_a?(String)
        return unless message.uri.include?(text)

        message.uri =
          case scope
          when :uri then message.uri.gsub(text, replacement_text)
          when :scheme then replace_uri_scheme(message.uri, text, replacement_text)
          when :host then replace_uri_host(message, text, replacement_text)
          when :port then replace_uri_port(message, text, replacement_text)
          when :path then replace_uri_path(message.uri, text, replacement_text)
          when :query then replace_uri_query(message.uri, text, replacement_text)
          end
      end

      # The replacements are plain string substitutions (matching the un-scoped
      # `filter!` behavior): URI component setters would percent-encode or reject
      # placeholder strings such as `<HOST>`.
      def replace_uri_scheme(uri, text, replacement_text)
        prefix = uri.slice(/\A[^:]+(?=:\/\/)/).to_s
        return uri unless prefix.include?(text)
        uri.sub(/\A[^:]+/) { |scheme| scheme.gsub(text, replacement_text) }
      end

      def replace_uri_host(message, text, replacement_text)
        host = message.parsed_uri.host
        return message.uri if host.nil? || !host.include?(text)
        message.uri.sub(/(\/\/[^\/?#]*@)?#{Regexp.escape(host)}/) do
          "#{$1}#{host.gsub(text, replacement_text)}"
        end
      end

      def replace_uri_port(message, text, replacement_text)
        port = message.parsed_uri.port
        return message.uri if port.nil?

        port_text = port.to_s
        return message.uri unless port_text.include?(text)

        new_port = port_text.gsub(text, replacement_text)
        message.uri.sub(URI_AUTHORITY_PATTERN) do |authority|
          if authority.end_with?(":#{port_text}")
            authority[0...-port_text.length] + new_port
          else
            authority
          end
        end
      end

      def replace_uri_path(uri, text, replacement_text)
        match = URI_AUTHORITY_PATTERN.match(uri)
        after_authority = match ? match.end(0) : 0
        section_start = uri.index('?', after_authority) || uri.index('#', after_authority) || uri.length
        path = uri[after_authority...section_start].to_s
        return uri unless path.include?(text)
        uri[0...after_authority] + path.gsub(text, replacement_text) + uri[section_start..-1].to_s
      end

      def replace_uri_query(uri, text, replacement_text)
        match = URI_AUTHORITY_PATTERN.match(uri)
        after_authority = match ? match.end(0) : 0
        query_start = uri.index('?', after_authority)
        return uri if query_start.nil?

        fragment_start = uri.index('#', query_start) || uri.length
        query = uri[(query_start + 1)...fragment_start]
        return uri unless query.include?(text)
        uri[0..query_start] + query.gsub(text, replacement_text) + uri[fragment_start..-1].to_s
      end

      # Strings are replaced rather than mutated with gsub!, and only when they
      # contain the text: cassette strings may be frozen (or chilled literals on
      # Ruby 3.4+), and copying every string of every interaction for each filter
      # dominates cassette loading for suites declaring many filters.
      def filter_object!(object, text, replacement_text)
        if object.respond_to?(:gsub)
          object = object.gsub(text, replacement_text) if object.include?(text)
        elsif ::Hash === object
          filter_hash!(object, text, replacement_text)
        elsif object.respond_to?(:each_pair) && object.respond_to?(:[]=)
          object.each_pair { |name, value| object[name] = filter_object!(value, text, replacement_text) }
        elsif ::Array === object
          object.map! { |value| filter_object!(value, text, replacement_text) }
        end

        object
      end

      def filter_hash!(hash, text, replacement_text)
        hash.transform_values! { |value| filter_object!(value, text, replacement_text) }

        hash.keys.each do |key|
          new_key = filter_object!(key, text, replacement_text)
          hash[new_key] = hash.delete(key) unless key.equal?(new_key)
        end
      end
    end
  end
end
