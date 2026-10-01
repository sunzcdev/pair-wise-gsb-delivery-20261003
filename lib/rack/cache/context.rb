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

    # Forward an unsafe request (POST, PUT, DELETE, ...) to the backend and
    # invalidate affected cache entries after a non-error response is
    # received. The request target is always invalidated; in addition,
    # same-origin URIs referenced by the Location and Content-Location
    # response headers are invalidated, per RFC 9111 section 4.4.
    #
    # Entries are marked stale rather than deleted, so they may still be
    # revalidated according to the normal caching rules. A failed write
    # (a 4xx/5xx response, or one that raises while forwarding) does not
    # invalidate anything.
    def invalidate
      response = pass

      if response.status < 400
        begin
          invalidate_locations(response)
        rescue => e
          log_error(e)
        else
          record :invalidate
        end
      end

      response
    end

    # Invalidate the request target along with every same-origin URI that
    # can be parsed from the response's Location and Content-Location
    # headers. Relative references are resolved against this request;
    # cross-origin, malformed, and duplicate URIs do not widen the set.
    def invalidate_locations(response)
      store = metastore
      requests = [@request]
      invalidated_keys = [store.cache_key(@request)]

      %w[location content-location].each do |header|
        next unless value = response.headers[header]
        next unless request_uri = parse_same_origin_uri(value)
        target = request_for_uri(request_uri)
        key = store.cache_key(target)
        next if invalidated_keys.include?(key)
        invalidated_keys << key
        requests << target
      end

      requests.each do |target|
        store.invalidate(target, entitystore)
      end
    end

    # Parse +value+ as a URI reference resolved against the effective
    # request URI (including its path and query), per RFC 3986 section 5.
    # Returns nil when the reference is malformed or does not identify the
    # same origin (scheme, host, and port) as the request.
    def parse_same_origin_uri(value)
      return nil if value.to_s.empty?
      base = URI::Generic.build(
        :scheme  => @request.scheme,
        :host    => @request.host,
        :port    => @request.port,
        :path    => @request.script_name + @request.path_info,
        :query   => @request.query_string.empty? ? nil : @request.query_string
      )
      uri = base + value.to_s
      return nil unless uri.absolute? && uri.host
      return nil unless uri.scheme == @request.scheme
      return nil unless host_without_brackets(uri.host) ==
                        host_without_brackets(@request.host)
      return nil unless uri.port == @request.port
      uri
    rescue URI::Error, ArgumentError
      nil
    end

    # Normalize a host for comparison: IPv6 hosts are bracketed in URIs
    # (e.g. "[::1]") but reported without brackets by Rack::Request.
    def host_without_brackets(host)
      host.to_s.downcase.delete_prefix('[').delete_suffix(']')
    end

    # Build a Request object for the absolute, same-origin +uri+ so that
    # the configured cache key generator produces the key under which the
    # referenced response is cached. The cache key itself carries the
    # script name, path, and query of the target; any mount prefix is
    # therefore derived from the resolved URI rather than copied from the
    # triggering request (whose Location may live outside the app).
    def request_for_uri(uri)
      env = @env.select { |key, _| key.start_with?('rack-cache.') }
      env.update(
        'rack.url_scheme' => uri.scheme,
        'HTTP_HOST'       => host_header(uri),
        'SCRIPT_NAME'     => '',
        'PATH_INFO'       => uri.path.to_s.empty? ? '/' : uri.path,
        'QUERY_STRING'    => uri.query.to_s,
        'REQUEST_METHOD'  => 'GET'
      )
      Request.new(env)
    end

    def host_header(uri)
      default_port = uri.scheme == 'https' ? 443 : 80
      if uri.port == default_port
        uri.host
      else
        "#{uri.host}:#{uri.port}"
      end
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
            entry
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
