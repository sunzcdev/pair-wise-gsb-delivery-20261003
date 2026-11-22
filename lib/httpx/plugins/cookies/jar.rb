# frozen_string_literal: true

module HTTPX
  module Plugins::Cookies
    # The Cookie Jar
    #
    # It stores and manages cookies for a session, such as i.e. evicting when expired, access methods, or
    # initialization from parsing `Set-Cookie` HTTP header values.
    #
    # It closely follows the [CookieStore API](https://developer.mozilla.org/en-US/docs/Web/API/CookieStore),
    # by implementing the same methods, with a few specific conveniences for this non-browser manipulation use-case.
    #
    class Jar
      using URIExtensions

      include Enumerable

      def initialize_dup(orig)
        super
        @mtx = orig.instance_variable_get(:@mtx).dup
        @current_partition = orig.instance_variable_get(:@current_partition).dup
        @cookies = orig.instance_variable_get(:@cookies).map(&:dup)
      end

      # initializes the cookie store, either empty, or with whatever is passed as +cookies+, which
      # can be an array of HTTPX::Plugins::Cookies::Cookie objects or hashes-or-tuples of cookie attributes.
      #
      # +partition_key+ optionally isolates the cookies managed by this jar in a named partition,
      # so that multiple logical stores (i.e. one per upstream) can coexist in the same jar, even
      # when they share domains and cookie names.
      def initialize(cookies = nil, partition_key: nil)
        @mtx = Thread::Mutex.new
        # the current partition is kept in a mutable cell, so that it can be switched even when
        # the jar itself is frozen (i.e. when it is part of frozen session options), just like
        # the cookie store and the mutex already are.
        @current_partition = [partition_key]
        @cookies = []

        cookies.each do |elem|
          @cookies << [partition_key, to_cookie(elem)]
        end if cookies
      end

      # the key of the partition cookies are stored under and looked up from.
      #
      # when nil (the default), the jar behaves as a single, unpartitioned store.
      #
      # it can be changed at any time (i.e. to associate the jar with a different
      # upstream before performing requests), and all operations (updates, lookups,
      # expiry eviction, deletions) only apply to the partition it points to.
      def partition_key
        @current_partition[0]
      end

      # associates the jar with the partition identified by +partition_key+.
      def partition_key=(partition_key)
        @current_partition[0] = partition_key
      end

      # parses the `Set-Cookie` header value as +set_cookie+ and does the corresponding updates.
      def parse(set_cookie)
        SetCookieParser.call(set_cookie) do |name, value, attrs|
          set(Cookie.new(name, value, attrs))
        end
      end

      # returns the first HTTPX::Plugins::Cookie::Cookie instance in the store which matches either the name
      # (when String) or all attributes (when a Hash or array of tuples) passed to +name_or_options+
      def get(name_or_options)
        each.find { |ck| ck.match?(name_or_options) }
      end

      # returns all HTTPX::Plugins::Cookie::Cookie instances in the store which match either the name
      # (when String) or all attributes (when a Hash or array of tuples) passed to +name_or_options+
      def get_all(name_or_options)
        each.select { |ck| ck.match?(name_or_options) } # rubocop:disable Style/SelectByRegexp
      end

      # when +name+ is a HTTPX::Plugins::Cookie::Cookie, it stores it internally; when +name+ is a String,
      # it creates a cookie with it and the value-or-attributes passed to +value_or_options+.

      # optionally, +name+ can also be the attributes hash-or-array as long it contains a <tt>:name</tt> field).
      def set(name, value_or_options = nil)
        cookie = case name
                 when Cookie
                   raise ArgumentError, "there should not be a second argument" if value_or_options

                   name
                 when Array, Hash
                   raise ArgumentError, "there should not be a second argument" if value_or_options

                   Cookie.new(name)
                 else
                   raise ArgumentError, "the second argument is required" unless value_or_options

                   Cookie.new(name, value_or_options)
        end

        store(partition_key, cookie)
      end

      # @deprecated
      def add(cookie, path = nil)
        warn "DEPRECATION WARNING: calling `##{__method__}` is deprecated. Use `#set` instead."
        c = cookie.dup
        c.path = path if path && c.path == "/"
        set(c)
      end

      # deletes all cookies  in the store which match either the name (when String) or all attributes (when a Hash
      # or array of tuples) passed to +name_or_options+.
      #
      # alternatively, of +name_or_options+ is an instance of HTTPX::Plugins::Cookies::Cookiem, it deletes it from the store.
      def delete(name_or_options)
        synchronize do
          case name_or_options
          when Cookie
            @cookies.delete_if { |(partition, ck)| partition == partition_key && ck == name_or_options }
          else
            @cookies.delete_if { |(partition, ck)| partition == partition_key && ck.match?(name_or_options) }
          end
        end
      end

      # deletes all cookies stored under the current partition key.
      def clear
        synchronize { @cookies.delete_if { |(partition, _)| partition == partition_key } }
      end

      # returns the list of valid cookies which matdh the domain and path from the URI object passed to +uri+.
      def [](uri)
        each(uri).sort
      end

      # enumerates over the cookies stored under the current partition key. if +uri+ is passed,
      # it'll filter out expired cookies and only yield cookies which match its domain and path.
      def each(uri = nil, &blk)
        return enum_for(__method__, uri) unless blk

        unless uri
          return synchronize do
            @cookies.each do |(partition, cookie)|
              yield cookie if partition == partition_key
            end
          end
        end

        now = Time.now
        tpath = uri.path

        synchronize do
          @cookies.delete_if do |(partition, cookie)|
            if partition != partition_key
              false
            elsif cookie.expired?(now)
              true
            else
              yield cookie if cookie.valid_for_uri?(uri) && Cookie.path_match?(cookie.path, tpath)
              false
            end
          end
        end
      end

      def merge(other)
        jar_dup = dup

        if other.is_a?(Jar)
          # preserve the partitions the cookies were stored under, and follow
          # the partition the other jar is currently associated with
          other.each_stored do |partition, elem|
            jar_dup.send(:store, partition, elem)
          end

          jar_dup.partition_key = other.partition_key
        else
          other.each do |elem|
            jar_dup.set(to_cookie(elem))
          end
        end

        jar_dup
      end

      protected

      # enumerates over all stored (partition key, cookie) pairs, across all partitions.
      def each_stored(&blk)
        synchronize { @cookies.each(&blk) }
      end

      private

      def to_cookie(elem)
        case elem
        when Cookie
          elem
        when Array
          Cookie.new(*elem)
        else
          Cookie.new(elem)
        end
      end

      # stores +cookie+ under +partition_key+.
      #
      # If the user agent receives a new cookie with the same cookie-name, domain-value, and path-value
      # as a cookie that it has already stored under the same partition, the existing cookie is evicted
      # and replaced with the new cookie.
      def store(partition_key, cookie)
        synchronize do
          @cookies.delete_if do |(partition, ck)|
            partition == partition_key &&
              ck.name == cookie.name && ck.domain == cookie.domain && ck.path == cookie.path
          end

          @cookies << [partition_key, cookie]
        end
      end

      def synchronize(&block)
        return yield if @mtx.owned?

        @mtx.synchronize(&block)
      end
    end
  end
end
