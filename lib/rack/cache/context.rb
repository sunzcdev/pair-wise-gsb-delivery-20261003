require 'rack/cache/options'
require 'rack/cache/request'
require 'rack/cache/response'
require 'rack/cache/storage'

module Rack::Cache
  # Implements Rack's middleware interface and provides the context for all
  # cache logic, including the core logic engine.
  class Context
    include Rack::Cache::Options

    # Array of trace Symbols
    attr_reader :trace

    # The Rack application object immediately downstream.
    attr_reader :backend

    def initialize(backend, options={})
      @backend = backend
      @trace = []
      @env = nil
      @options = options

      initialize_options options
      yield self if block_given?

      @private_header_keys =
        private_headers.map { |name| "HTTP_#{name.upcase.tr('-', '_')}" }
    end

    # The configured MetaStore instance. Changing the rack-cache.metastore
    # value effects the result of this method immediately.
    def metastore
      uri = options['rack-cache.metastore']
      storage.resolve_metastore_uri(uri, @options)
    end

    # The configured EntityStore instance. Changing the rack-cache.entitystore
    # value effects the result of this method immediately.
    def entitystore
      uri = options['rack-cache.entitystore']
      storage.resolve_entitystore_uri(uri, @options)
    end

    # The Rack call interface. The receiver acts as a prototype and runs
    # each request in a dup object unless the +rack.run_once+ variable is
    # set in the environment.
    def call(env)
      if env['rack.run_once'] && !env['rack.multithread']
        call! env
      else
        clone.call! env
      end
    end

    # The real Rack call interface. The caching logic is performed within
    # the context of the receiver.
    def call!(env)
      @trace = []
      @default_options.each { |k,v| env[k] ||= v }
      @env = env
      @request = Request.new(@env.dup.freeze)

      response =
        if @request.get? || @request.head?
          if !@env['HTTP_EXPECT'] && !@env['rack-cache.force-pass']
            lookup
          else
            pass
          end
        else
          if @request.options?
            pass
          else
            invalidate
          end
        end

      # log trace and set x-rack-cache tracing header
      trace = @trace.join(', ')
      response.headers['x-rack-cache'] = trace

      # write log message to rack.errors
      if verbose?
        message = "cache: [%s %s] %s\n" %
          [@request.request_method, @request.fullpath, trace]
        log_info(message)
      end

      # tidy up response a bit
      if (@request.get? || @request.head?) && not_modified?(response)
        response.not_modified!
      end

      if @request.head?
        response.body.close if response.body.respond_to?(:close)
        response.body = []
      end
      response.to_a
    end

  private

    # Record that an event took place.
    def record(event)
      @trace << event
    end

    # Does the request include authorization or other sensitive information
    # that should cause the response to be considered private by default?
    # Private responses are not stored in the cache.
    def private_request?
      @private_header_keys.any? { |key| @env.key?(key) }
    end

    # Determine if the #response validators (etag, last-modified) matches
    # a conditional value specified in #request.
    def not_modified?(response)
      last_modified = @request.env['HTTP_IF_MODIFIED_SINCE']
      if etags = @request.env['HTTP_IF_NONE_MATCH']
        etags = etags.split(/\s*,\s*/)
        (etags.include?(response.etag) || etags.include?('*')) && (!last_modified || response.last_modified == last_modified)
      elsif last_modified
        response.last_modified == last_modified
      end
    end

    # Whether the cache entry is "fresh enough" to satisfy the request.
    def fresh_enough?(entry)
      if entry.fresh?
        if allow_revalidate? && max_age = @request.cache_control.max_age
          max_age > 0 && max_age >= entry.age
        else
          true
        end
      end
    end

    # Delegate the request to the backend and create the response.
    def forward
      Response.new(*backend.call(@env))
    end

    # The request is sent to the backend, and the backend's response is sent
    # to the client, but is not entered into the cache.
    def pass
      record :pass
      forward
    end

    # Invalidate POST, PUT, DELETE and all methods not understood by this cache
    # See RFC2616 13.10
    def invalidate
      metastore.invalidate(@request, entitystore)
    rescue => e
      log_error(e)
      pass
    else
      record :invalidate
      pass
    end

    # Try to serve the response from cache. When a matching cache entry is
    # found and is fresh, use it as the response without forwarding any request
    # to the backend. When a matching cache entry is found but is stale, attempt
    # to #validate the entry with the backend using conditional GET.
    # If validation raises an exception and fault tolerant caching is enabled,
    # serve the stale cache entry.
    # When no matching cache entry is found, trigger miss processing.
    def lookup
      if @request.no_cache? && allow_reload?
        record :reload
        fetch
      else
        begin
          entry = metastore.lookup(@request, entitystore)
        rescue => e
          log_error(e)
          return pass
        end
        if entry
          if fresh_enough?(entry)
            record :fresh
            entry.headers['age'] = entry.age.to_s
            range_response(entry) || entry
          else
            record :stale
            if fault_tolerant?
              validate_with_stale_cache_failover(entry)
            else
              validate(entry)
            end
          end
        else
          record :miss
          fetch
        end
      end
    end

    # Attempt to satisfy a Range request from a fresh, complete cache
    # entry. Returns a 206 or 416 response when the request includes a
    # Range header that can be handled locally, or nil when the full
    # entry should be served instead (no Range header, unsplittable
    # entry, or a failed If-Range precondition). HEAD requests ignore
    # the Range header field per RFC 7233.
    def range_response(entry)
      return unless @request.get?
      return unless entry.status == 200
      header = @request.env['HTTP_RANGE']
      return unless header
      return unless size = entry.headers['content-length']
      return unless if_range_satisfied?(entry)

      size = size.to_i
      ranges = parse_byte_ranges(header, size)
      return if ranges.nil?
      return range_not_satisfiable_response(entry, size) if ranges.empty?

      if ranges.length == 1
        single_range_response(entry, ranges.first, size)
      else
        multipart_range_response(entry, ranges, size)
      end
    end

    # Determine whether the If-Range precondition holds for the cache
    # entry. If-Range values are compared using the strong comparison
    # function: entity tags never match weakly and a Last-Modified
    # value must match exactly.
    def if_range_satisfied?(entry)
      value = @request.env['HTTP_IF_RANGE']
      return true if value.nil? || value.strip.empty?
      value = value.strip
      if value.start_with?('"', 'W/"', 'w/"')
        return false unless value.start_with?('"')
        etag = entry.etag
        !etag.nil? && !etag.start_with?('W/', 'w/') && etag == value
      else
        !entry.last_modified.nil? && entry.last_modified == value
      end
    end

    # Parse a byte-range-spec header value into an Array of [first, last]
    # byte positions (inclusive) against a representation of +size+
    # bytes. Unsatisfiable ranges are dropped. Returns nil when the
    # header cannot be understood, in which case it must be ignored.
    def parse_byte_ranges(header, size)
      return unless header =~ /\Abytes=(.+)\z/im
      ranges = []
      $1.split(/\s*,\s*/).each do |spec|
        case spec
        when /\A(\d+)-(\d*)\z/
          first = $1.to_i
          last = $2.empty? ? size - 1 : $2.to_i
          next if first >= size
          last = size - 1 if last >= size
          ranges << [first, last] if first <= last
        when /\A-(\d+)\z/
          length = $1.to_i
          next if length == 0
          first = size - length
          first = 0 if first < 0
          ranges << [first, size - 1] if first <= size - 1
        else
          return
        end
      end
      ranges
    end

    # Extract the byte +ranges+ (an Array of [first, last] pairs) from
    # the response +body+ as it is stored in the entity store, without
    # joining the entire body into a single string. Returns an Array of
    # chunk Arrays, one per range, in the same order as +ranges+.
    def slice_ranges(body, ranges)
      pieces = ranges.map { [] }
      offset = 0
      body.each do |chunk|
        chunk_start = offset
        offset += chunk.bytesize
        ranges.each_with_index do |(first, last), index|
          next if last < chunk_start || first >= offset
          from = first - chunk_start
          from = 0 if from < 0
          to = last - chunk_start
          to = chunk.bytesize - 1 if to >= chunk.bytesize
          pieces[index] << chunk.byteslice(from..to)
        end
      end
      body.close if body.respond_to?(:close)
      pieces
    end

    # Build a 206 Partial Content response for a single byte range.
    def single_range_response(entry, range, size)
      first, last = range
      response = entry.dup
      response.status = 206
      response.body = slice_ranges(entry.body, [range]).first
      response.headers['content-range'] = "bytes #{first}-#{last}/#{size}"
      response.headers['content-length'] = (last - first + 1).to_s
      response.headers['accept-ranges'] = 'bytes'
      response
    end

    # Build a 206 Partial Content response carrying each byte range as a
    # part of a multipart/byteranges body. The boundary, per-part
    # content-range values, and the overall content-length are derived
    # from the same part list so they stay consistent.
    def multipart_range_response(entry, ranges, size)
      boundary = "rack-cache-#{rand(1_000_000_000)}"
      content_type = entry.headers['content-type']
      slices = slice_ranges(entry.body, ranges)

      body = []
      ranges.each_with_index do |(first, last), index|
        body << "--#{boundary}\r\n"
        body << "content-type: #{content_type}\r\n" if content_type
        body << "content-range: bytes #{first}-#{last}/#{size}\r\n"
        body << "\r\n"
        body.concat(slices[index])
        body << "\r\n"
      end
      body << "--#{boundary}--\r\n"

      length = 0
      body.each { |part| length += part.bytesize }

      response = entry.dup
      response.status = 206
      response.body = body
      response.headers['content-type'] = "multipart/byteranges; boundary=#{boundary}"
      response.headers['content-length'] = length.to_s
      response.headers['accept-ranges'] = 'bytes'
      response
    end

    # Build a 416 Range Not Satisfiable response describing the actual
    # size of the stored representation.
    def range_not_satisfiable_response(entry, size)
      entry.body.close if entry.body.respond_to?(:close)
      response = entry.dup
      response.status = 416
      response.body = []
      response.headers['content-range'] = "bytes */#{size}"
      response.headers['content-length'] = '0'
      response.headers['accept-ranges'] = 'bytes'
      response
    end

    # Returns stale cache on exception.
    def validate_with_stale_cache_failover(entry)
      validate(entry)
    rescue => e
      record :connnection_failed
      age = entry.age.to_s
      entry.headers['age'] = age
      record "Fail-over to stale cache data with age #{age} due to #{e.class.name}: #{e}"
      entry
    end

    # Validate that the cache entry is fresh. The original request is used
    # as a template for a conditional GET request with the backend.
    def validate(entry)
      # send no head requests because we want content
      convert_head_to_get!

      # add our cached last-modified validator to the environment
      @env['HTTP_IF_MODIFIED_SINCE'] = entry.last_modified

      # Add our cached etag validator to the environment.
      # We keep the etags from the client to handle the case when the client
      # has a different private valid entry which is not cached here.
      cached_etags = entry.etag.to_s.split(/\s*,\s*/)
      request_etags = @request.env['HTTP_IF_NONE_MATCH'].to_s.split(/\s*,\s*/)
      etags = (cached_etags + request_etags).uniq
      @env['HTTP_IF_NONE_MATCH'] = etags.empty? ? nil : etags.join(', ')

      response = forward

      if response.status == 304
        record :valid

        # Check if the response validated which is not cached here
        etag = response.headers['etag']
        return response if etag && request_etags.include?(etag) && !cached_etags.include?(etag)

        entry = entry.dup
        entry.headers.delete('date')
        %w[Date expires cache-control etag last-modified].each do |name|
          next unless value = response.headers[name]
          entry.headers[name] = value
        end

        # even though it's empty, be sure to close the response body from upstream
        # because middleware use close to signal end of response
        response.body.close if response.body.respond_to?(:close)

        response = entry
      else
        record :invalid
      end

      store(response) if response.cacheable?

      response
    end

    # The cache missed or a reload is required. Forward the request to the
    # backend and determine whether the response should be stored. This allows
    # conditional / validation requests through to the backend but performs no
    # caching of the response when the backend returns a 304.
    def fetch
      # send no head requests because we want content
      convert_head_to_get!

      response = forward

      # Mark the response as explicitly private if any of the private
      # request headers are present and the response was not explicitly
      # declared public.
      if private_request? && !response.cache_control.public?
        response.private = true
      elsif default_ttl > 0 && response.ttl.nil? && !response.cache_control.must_revalidate?
        # assign a default TTL for the cache entry if none was specified in
        # the response; the must-revalidate cache control directive disables
        # default ttl assigment.
        response.ttl = default_ttl
      end

      store(response) if response.cacheable?

      response
    end

    # Write the response to the cache.
    def store(response)
      strip_ignore_headers(response)
      metastore.store(@request, response, entitystore)
      response.headers['age'] = response.age.to_s
    rescue => e
      log_error(e)
      nil
    else
      record :store
    end

    # Remove all ignored response headers before writing to the cache.
    def strip_ignore_headers(response)
      stripped_values = ignore_headers.map { |name| response.headers.delete(name) }
      record :ignore if stripped_values.any?
    end

    def log_error(exception)
      message = "cache error: #{exception.message}\n#{exception.backtrace.join("\n")}\n"
      log(:error, message)
    end

    def log_info(message)
      log(:info, message)
    end

    def log(level, message)
      if @env['rack.logger']
        @env['rack.logger'].send(level, message)
      else
        @env['rack.errors'].write(message)
      end
    end

    # send no head requests because we want content
    def convert_head_to_get!
      if @env['REQUEST_METHOD'] == 'HEAD'
        @env['REQUEST_METHOD'] = 'GET'
        @env['rack.methodoverride.original_method'] = 'HEAD'
      end
    end
  end
end
