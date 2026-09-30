require 'rack/cache/key'
require 'rack/cache/storage'
require 'socket'
require 'timeout'

module Rack::Cache

  # Configuration options and utility methods for option access. Rack::Cache
  # uses the Rack Environment to store option values. All options documented
  # below are stored in the Rack Environment as "rack-cache.<option>", where
  # <option> is the option name.
  module Options
    extend self

    def self.option_accessor(key)
      name = option_name(key)
      define_method(key) { || options[name] }
      define_method("#{key}=") { |value| options[name] = value }
      define_method("#{key}?") { || !! options[name] }
    end

    def option_name(key)
      case key
      when Symbol ; "rack-cache.#{key}"
      when String ; key
      else raise ArgumentError
      end
    end
    module_function :option_name

    # Enable verbose trace logging. This option is currently enabled by
    # default but is likely to be disabled in a future release.
    option_accessor :verbose

    # The storage resolver. Defaults to the Rack::Cache.storage singleton instance
    # of Rack::Cache::Storage. This object is responsible for resolving metastore
    # and entitystore URIs to an implementation instances.
    option_accessor :storage

    # A URI specifying the meta-store implementation that should be used to store
    # request/response meta information. The following URIs schemes are
    # supported:
    #
    # * heap:/
    # * file:/absolute/path or file:relative/path
    # * memcached://localhost:11211[/namespace]
    #
    # If no meta store is specified the 'heap:/' store is assumed. This
    # implementation has significant draw-backs so explicit configuration is
    # recommended.
    option_accessor :metastore

    # A custom cache key generator, which can be anything that responds to :call.
    # By default, this is the Rack::Cache::Key class, but you can implement your
    # own generator. A cache key generator gets passed a request and generates the
    # appropriate cache key.
    #
    # In addition to setting the generator to an object, you can just pass a block
    # instead, which will act as the cache key generator:
    #
    #   set :cache_key do |request|
    #     request.fullpath.replace(/\//, '-')
    #   end
    option_accessor :cache_key

    # A URI specifying the entity-store implementation that should be used to
    # store response bodies. See the metastore option for information on
    # supported URI schemes.
    #
    # If no entity store is specified the 'heap:/' store is assumed. This
    # implementation has significant draw-backs so explicit configuration is
    # recommended.
    option_accessor :entitystore

    # The number of seconds that a cache entry should be considered
    # "fresh" when no explicit freshness information is provided in
    # a response. Explicit cache-control or expires headers
    # override this value.
    #
    # Default: 0
    option_accessor :default_ttl

    # Set of response headers that are removed before storing them in the
    # cache. These headers are only removed for cacheable responses.  For
    # example, in most cases, it makes sense to prevent cookies from being
    # stored in the cache.
    #
    # Default: ['set-cookie']
    option_accessor :ignore_headers

    # Set of request headers that trigger "private" cache-control behavior
    # on responses that don't explicitly state whether the response is
    # public or private via a cache-control directive. Applications that use
    # cookies for authorization may need to add the 'Cookie' header to this
    # list.
    #
    # Default: ['Authorization', 'Cookie']
    option_accessor :private_headers

    # Specifies whether a client can force cache reload by including a
    # cache-control "no-cache" directive in the request. Disabled by default.
    option_accessor :allow_reload

    # Specifies whether a client can force cache revalidate by including a
    # cache-control "max-age=0" directive in the request. Disabled by default.
    option_accessor :allow_revalidate

    # Specifies whether the underlying entity store's native expiration should
    # be used.
    option_accessor :use_native_ttl

    # Specifies whether to serve a request from a stale cache entry if
    # the attempt to revalidate the cache entry raises an exception.
    option_accessor :fault_tolerant

    # Specifies whether RFC 5861 stale-if-error behavior is enabled. When
    # true, a stale entry may be served only within the grace window (in
    # seconds) declared by the response's cache-control stale-if-error
    # directive and only when revalidation hits a configured fault: one of
    # the exceptions in #stale_if_error_exceptions or one of the statuses
    # in #stale_if_error_statuses. Outside the window, or for an
    # unconfigured fault (e.g. an application programming error), the
    # failure is propagated as-is. Unlike #fault_tolerant, this mode has a
    # hard deadline and never swallows arbitrary exceptions.
    option_accessor :stale_if_error

    # Exception classes (or a Proc) that count as transport faults for the
    # stale-if-error mode. When given a Proc, it is called with the
    # exception and must return truthy for the fault to be eligible.
    #
    # Default: connection/timeout style errors raised by the transport;
    # generic application errors such as RuntimeError are intentionally not
    # included.
    option_accessor :stale_if_error_exceptions

    # Response status codes (or a Proc) that count as origin faults for the
    # stale-if-error mode. When given a Proc, it is called with the status
    # code and must return truthy for the status to be eligible.
    #
    # Default: 500, 502, 503, and 504.
    option_accessor :stale_if_error_statuses

    # The underlying options Hash. During initialization (or outside of a
    # request), this is a default values Hash. During a request, this is the
    # Rack environment Hash. The default values Hash is merged in underneath
    # the Rack environment before each request is processed.
    def options
      @env || @default_options
    end

    # Set multiple options.
    def options=(hash={})
      hash.each { |key,value| write_option(key, value) }
    end

    # Set an option. When +option+ is a Symbol, it is set in the Rack
    # Environment as "rack-cache.option". When +option+ is a String, it
    # exactly as specified. The +option+ argument may also be a Hash in
    # which case each key/value pair is merged into the environment as if
    # the #set method were called on each.
    def set(option, value=self, &block)
      if block_given?
        write_option option, block
      elsif value == self
        self.options = option.to_hash
      else
        write_option option, value
      end
    end

  private
    def initialize_options(options={})
      @default_options = {
        'rack-cache.cache_key'        => Key,
        'rack-cache.verbose'          => true,
        'rack-cache.storage'          => Rack::Cache::Storage.instance,
        'rack-cache.metastore'        => 'heap:/',
        'rack-cache.entitystore'      => 'heap:/',
        'rack-cache.default_ttl'      => 0,
        'rack-cache.ignore_headers'   => ['set-cookie'],
        'rack-cache.private_headers'  => ['Authorization', 'Cookie'],
        'rack-cache.allow_reload'     => false,
        'rack-cache.allow_revalidate' => false,
        'rack-cache.use_native_ttl'   => false,
        'rack-cache.fault_tolerant'   => false,
        'rack-cache.stale_if_error'   => false,
        'rack-cache.stale_if_error_exceptions' =>
          [
            Errno::ECONNREFUSED,
            Errno::ECONNRESET,
            Errno::EHOSTUNREACH,
            Errno::ENETUNREACH,
            Errno::ETIMEDOUT,
            Errno::EPIPE,
            IOError,
            SocketError,
            Timeout::Error,
          ],
        'rack-cache.stale_if_error_statuses' => [500, 502, 503, 504],
      }
      self.options = options
    end

    def read_option(key)
      options[option_name(key)]
    end

    def write_option(key, value)
      options[option_name(key)] = value
    end
  end
end
