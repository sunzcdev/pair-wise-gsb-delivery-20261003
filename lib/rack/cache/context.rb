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
      return validate_with_head(entry) if @request.head? && head_freshening?

      # send no head requests because we want content
      convert_head_to_get!

      # Add our cached validators to the environment.
      # We keep the etags from the client to handle the case when the client
      # has a different private valid entry which is not cached here.
      cached_etags, request_etags = apply_cached_validators!(entry)

      response = forward

      if response.status == 304
        record :valid

        # Check if the response validated which is not cached here
        etag = response.headers['etag']
        return response if etag && request_etags.include?(etag) && !cached_etags.include?(etag)

        entry = merge_not_modified(entry, response)

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

    # Revalidate a cached GET representation with a conditional HEAD request.
    # The backend always sees HEAD (it is never rewritten to GET) and its
    # response is only used to update the cached metadata. The stored body is
    # never replaced since a HEAD response carries no entity.
    #
    # See http://tools.ietf.org/html/rfc9111#section-4.3.4.
    def validate_with_head(entry)
      cached_etags, request_etags = apply_cached_validators!(entry)

      response = forward

      store_entry = false
      if response.status == 304
        record :valid

        # Check if the response validated which is not cached here
        etag = response.headers['etag']
        return response if etag && request_etags.include?(etag) && !cached_etags.include?(etag)

        entry = merge_not_modified(entry, response)

        response.body.close if response.body.respond_to?(:close)

        response = entry
        store_entry = true
      elsif response.status == 200
        case head_representation_status(entry, response)
        when :compatible
          record :freshen

          entry = merge_head_representation(entry, response)

          response.body.close if response.body.respond_to?(:close)

          response = entry
          store_entry = true
        else
          # The validators disagree (:conflict) or there is not enough
          # information to prove the entity is the same (:insufficient).
          # Do not treat the HEAD response as a validated GET entity: the
          # stale entry is left untouched and a later GET will re-fetch
          # a complete representation.
          record :invalid
        end
      else
        record :invalid
      end

      store(response) if store_entry && response.cacheable?

      response
    end

    # The cache missed or a reload is required. Forward the request to the
    # backend and determine whether the response should be stored. This allows
    # conditional / validation requests through to the backend but performs no
    # caching of the response when the backend returns a 304.
    def fetch
      # send no head requests because we want content
      convert_head_to_get! unless head_freshening?

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

      # A HEAD response carries no body; never turn one into an empty cached
      # GET representation when the request stayed HEAD.
      store(response) if response.cacheable? && !(@request.head? && head_freshening?)

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

    # Add the cached entry's validators to the request environment. Returns
    # the list of cached etags and the etags sent by the client.
    def apply_cached_validators!(entry)
      # add our cached last-modified validator to the environment
      @env['HTTP_IF_MODIFIED_SINCE'] = entry.last_modified

      cached_etags = entry.etag.to_s.split(/\s*,\s*/)
      request_etags = @request.env['HTTP_IF_NONE_MATCH'].to_s.split(/\s*,\s*/)
      etags = (cached_etags + request_etags).uniq
      @env['HTTP_IF_NONE_MATCH'] = etags.empty? ? nil : etags.join(', ')

      [cached_etags, request_etags]
    end

    # Merge the metadata of a 304 Not Modified response into a copy of the
    # stored entry, leaving its entity (body and body headers) untouched.
    def merge_not_modified(entry, response)
      entry = entry.dup
      entry.headers.delete('date')
      %w[Date expires cache-control etag last-modified].each do |name|
        next unless value = response.headers[name]
        entry.headers[name] = value
      end
      entry
    end

    # Metadata that may be copied from a successful HEAD response onto the
    # stored GET representation because it does not describe the entity body.
    HEAD_METADATA_HEADERS = %w[
      date
      expires
      cache-control
      etag
      last-modified
    ].freeze

    # Body-related headers that may only be adopted when they agree with the
    # representation already cached (they must describe the retained body).
    HEAD_ENTITY_HEADERS = %w[
      content-encoding
      content-language
      content-md5
      content-type
    ].freeze

    # Determine whether the HEAD response represents the same entity as the
    # stored GET entry:
    #
    # * :compatible when strong validators (strong etag or last-modified)
    #   agree and the content-length, when known on both sides, matches.
    # * :conflict when a validator or the content-length disagrees.
    # * :insufficient when there is no validator on either representation or
    #   the validator cannot be compared strongly.
    def head_representation_status(entry, response)
      entry_etag = strong_etag(entry.etag)
      response_etag = strong_etag(response.etag)
      if entry_etag && response_etag
        return :conflict unless entry_etag == response_etag
      elsif entry.last_modified && response.last_modified
        return :conflict unless same_httpdate?(entry.last_modified, response.last_modified)
      else
        return :insufficient
      end

      entry_length = entry.headers['content-length']
      response_length = response.headers['content-length']
      if entry_length && response_length && entry_length != response_length
        return :conflict
      end

      :compatible
    end

    # Copy updatable metadata from a compatible HEAD response onto the stored
    # entry. The original body and its content-length are always preserved.
    def merge_head_representation(entry, response)
      entry = entry.dup
      entry.headers.delete('date')

      HEAD_METADATA_HEADERS.each do |name|
        next unless value = response.headers[name]
        entry.headers[name] = value
      end

      HEAD_ENTITY_HEADERS.each do |name|
        next unless value = response.headers[name]
        current = entry.headers[name]
        entry.headers[name] = value if current.nil? || current == value
      end

      entry
    end

    # Return the etag when it is a strong validator (no "W/" prefix), nil
    # otherwise. Weak validators only prove semantic equivalence and cannot
    # establish that the entity bytes are unchanged.
    def strong_etag(etag)
      return nil unless etag
      value = etag.strip
      value.start_with?('W/') ? nil : value
    end

    # Compare two HTTP-date values by parsing them; unparseable values are
    # only considered equal when their literal values match.
    def same_httpdate?(first, second)
      Time.httpdate(first) == Time.httpdate(second)
    rescue ArgumentError
      first == second
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

    # Rewrite HEAD requests to GET because the content is needed, unless
    # explicit HEAD freshening is enabled (the backend then keeps seeing
    # HEAD).
    def convert_head_to_get!
      if @env['REQUEST_METHOD'] == 'HEAD' && !head_freshening?
        @env['REQUEST_METHOD'] = 'GET'
        @env['rack.methodoverride.original_method'] = 'HEAD'
      end
    end
  end
end
