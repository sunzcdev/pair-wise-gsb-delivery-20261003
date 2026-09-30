# :markup: markdown

class HTTP::CookieJar
  # A store class that uses a hash-based cookie store.
  #
  # In this store, cookies that share the same name, domain and path
  # will overwrite each other regardless of the `for_domain` flag
  # value.  This store is built after the storage model described in
  # RFC 6265 5.3 where there is no mention of how the host-only-flag
  # affects in storing cookies.  On the other hand, in MozillaStore
  # two cookies with the same name, domain and path coexist as long as
  # they differ in the `for_domain` flag value, which means they need
  # to be expired individually.
  class HashStore < AbstractStore
    def default_options
      {
        :gc_threshold => HTTP::Cookie::MAX_COOKIES_TOTAL / 20,
        # Maximum number of cookies kept per site at a write.  nil
        # means no write-time cap is enforced.
        :max_cookies_per_site => nil,
        # Maximum total number of bytes kept in the store.  nil means
        # no write-time cap is enforced.
        :max_total_bytes => nil,
        # Clock object that responds to #now; defaults to Time.
        :clock => Time,
      }
    end

    # :call-seq:
    #   new(**options)
    #
    # Generates a hash based cookie store.
    #
    # Available option keywords are as below:
    #
    # :gc_threshold
    # : GC threshold; A GC happens when this many times cookies have
    # been stored (default: `HTTP::Cookie::MAX_COOKIES_TOTAL / 20`)
    #
    # :max_cookies_per_site
    # : Maximum number of cookies that are kept for a single site at
    # a write.  A site is the registrable domain derived from a
    # cookie's DomainName (e.g. `a.example.org` and `example.org` are
    # the same site); an IP literal is a site of its own.  Expired
    # cookies are purged first, and the least recently accessed
    # cookies of the site are evicted afterwards.  nil (default)
    # disables this write-time cap.
    #
    # :max_total_bytes
    # : Maximum total number of bytes of cookie name/value pairs kept
    # in the store.  On a write expired cookies are purged first,
    # followed by least recently accessed cookies of any site until
    # the budget is met.  A cookie that alone exceeds the budget is
    # rejected, leaving a replaced cookie untouched.  nil (default)
    # disables this write-time cap.
    #
    # :clock
    # : A clock object responding to #now that provides the current
    # time used for expiration and last-access bookkeeping.  It
    # defaults to `Time` and is primarily useful for testing.
    def initialize(options = nil)
      if options
        options = options.dup
        if options.key?(:cookies_per_site)
          options[:max_cookies_per_site] ||= options[:cookies_per_site]
        end
        if options.key?(:max_bytes)
          options[:max_total_bytes] ||= options[:max_bytes]
        end
      end
      super

      @jar = {
      # hostname => {
      #   path => {
      #     name => cookie,
      #     ...
      #   },
      #   ...
      # },
      # ...
      }

      @gc_index = 0

      # registrable domain => {
      #   :count => number of live cookies of the site,
      #   :bytes => total bytes of live cookies of the site,
      # }
      @sites = Hash.new { |sites, site|
        sites[site] = { :count => 0, :bytes => 0 }
      }
      @total_bytes = 0
    end

    # The copy constructor.  This store class supports cloning.
    def initialize_copy(other)
      @jar = Marshal.load(Marshal.dump(other.instance_variable_get(:@jar)))
      @sites = Hash.new { |sites, site|
        sites[site] = { :count => 0, :bytes => 0 }
      }
      other.instance_variable_get(:@sites).each { |site, stats|
        @sites[site] = Marshal.load(Marshal.dump(stats))
      }
      @total_bytes = other.instance_variable_get(:@total_bytes)
    end

    # Maximum number of cookies allowed per site, or nil when
    # uncapped.
    attr_reader :max_cookies_per_site

    # Maximum total bytes allowed in the store, or nil when uncapped.
    attr_reader :max_total_bytes

    # Number of bytes of cookie name/value pairs currently stored.
    attr_reader :total_bytes

    # Number of cookies currently stored per registrable domain.
    def cookies_per_site
      @sites.each_with_object({}) { |(site, stats), hash|
        hash[site] = stats[:count]
      }
    end

    # Number of bytes of the name/value pair of a cookie.
    def self.cookie_bytes(cookie)
      cookie.name.bytesize + cookie.value.bytesize
    end

    def add(cookie)
      add_all([cookie])
      self
    end

    # Atomically adds a collection of cookies.  Expired cookies are
    # purged first; then the per-site and total-byte budgets are
    # enforced by least-recently-used eviction.  The whole change is
    # rejected without any mutation when it cannot fit the budgets,
    # in which case false is returned.  Returns true on commit.
    def add_all(cookies)
      cookies = Array.try_convert(cookies) or
        raise TypeError, "#{cookies.class} is not an Array"

      synchronize {
        now = @clock.now

        purge_expired(now)

        # The last cookie in the collection wins when more than one
        # target the same identity.
        incoming = {}
        cookies.each { |cookie|
          incoming[store_key(cookie)] = cookie
        }

        projected_bytes = 0
        incoming_count = Hash.new(0)
        incoming.each_value { |cookie|
          size = self.class.cookie_bytes(cookie)
          site = site_of(cookie)
          projected_bytes += size
          incoming_count[site] += 1

          # A single cookie that alone exceeds the total budget can
          # never be accepted.
          if @max_total_bytes && size > @max_total_bytes
            return false
          end
        }

        # A site that cannot hold all its incoming cookies even after
        # its existing cookies are all evicted rejects the batch.
        if @max_cookies_per_site
          incoming_count.each_value { |count|
            return false if count > @max_cookies_per_site
          }
        end

        survivors = stored_keys
        incoming.each_key { |key|
          survivors.delete(key)
        }

        evict = []

        if @max_cookies_per_site
          site_counts = Hash.new(0)
          survivors.each { |key|
            site_counts[key[0]] += 1
          }
          incoming_count.each { |site, count|
            site_counts[site] += count
          }

          site_counts.each { |site, count|
            debt = count - @max_cookies_per_site
            next if debt <= 0
            candidates = survivors.select { |key|
              key[0] == site
            }
            lru_candidates(candidates, now).first(debt).each { |key|
              survivors.delete(key)
              evict << key
            }
          }
        end

        retained_bytes = survivors.inject(0) { |sum, key|
          sum + self.class.cookie_bytes(cookie_at(key))
        }

        if @max_total_bytes && retained_bytes + projected_bytes > @max_total_bytes
          ordered = lru_candidates(survivors.to_a, now)
          ordered.each { |key|
            break if retained_bytes + projected_bytes <= @max_total_bytes
            cookie = cookie_at(key)
            retained_bytes -= self.class.cookie_bytes(cookie)
            survivors.delete(key)
            evict << key
          }
          if retained_bytes + projected_bytes > @max_total_bytes
            return false
          end
        end

        evict.each { |key|
          erase_key(key)
        }

        committed = 0
        incoming.each { |key, cookie|
          erase_key(key)
          _, domain, path, name = key
          ((@jar[domain] ||= {})[path] ||= {})[name] = cookie
          size = self.class.cookie_bytes(cookie)
          stats = @sites[key[0]]
          stats[:count] += 1
          stats[:bytes] += size
          @total_bytes += size
          committed += 1
        }

        prune

        if (@gc_index += committed) >= @gc_threshold
          cleanup
        end

        true
      }
    end

    def delete(cookie)
      synchronize {
        erase_key(store_key(cookie))
        prune
      }
      self
    end

    def each(uri = nil) # :yield: cookie
      now = @clock.now
      if uri
        tpath = uri.path
        synchronize {
          @jar.each { |domain, paths|
            paths.each { |path, hash|
              next unless HTTP::Cookie.path_match?(path, tpath)
              hash.delete_if { |name, cookie|
                if cookie.expired?(now)
                  record_removed(cookie)
                  true
                else
                  if cookie.valid_for_uri?(uri)
                    cookie.accessed_at = now
                    yield cookie
                  end
                  false
                end
              }
            }
          }
          prune
        }
      else
        synchronize {
          @jar.each { |domain, paths|
            paths.each { |path, hash|
              hash.delete_if { |name, cookie|
                if cookie.expired?(now)
                  record_removed(cookie)
                  true
                else
                  yield cookie
                  false
                end
              }
            }
          }
          prune
        }
      end
      self
    end

    def clear
      @jar.clear
      @sites.clear
      @total_bytes = 0
      self
    end

    def cleanup(session = false)
      now = @clock.now
      all_cookies = []

      synchronize {
        break if @gc_index == 0

        purge_expired(now, session)

        @jar.each { |domain, paths|
          domain_cookies = []

          paths.each { |path, hash|
            hash.each_value { |cookie|
              domain_cookies << cookie
            }
          }

          if (debt = domain_cookies.size - HTTP::Cookie::MAX_COOKIES_PER_DOMAIN) > 0
            domain_cookies.sort_by!(&:created_at)
            domain_cookies.slice!(0, debt).each { |cookie|
              delete(cookie)
            }
          end

          all_cookies.concat(domain_cookies)
        }

        if (debt = all_cookies.size - HTTP::Cookie::MAX_COOKIES_TOTAL) > 0
          all_cookies.sort_by!(&:created_at)
          all_cookies.slice!(0, debt).each { |cookie|
            delete(cookie)
          }
        end

        prune

        @gc_index = 0
      }
      self
    end

    private

    # Returns the registrable domain that identifies the site of a
    # cookie, using the DomainName information already attached to
    # the cookie without performing any network lookup.
    def site_of(cookie)
      domain_name = cookie.domain_name
      if domain_name
        domain_name.domain || domain_name.hostname
      else
        cookie.domain
      end
    end

    def store_key(cookie)
      [site_of(cookie), cookie.domain, cookie.path, cookie.name]
    end

    def stored_keys
      keys = []
      @jar.each { |domain, paths|
        paths.each { |path, hash|
          hash.each { |name, cookie|
            keys << [site_of(cookie), domain, path, name]
          }
        }
      }
      keys
    end

    def cookie_at(key)
      _, domain, path, name = key
      @jar.fetch(domain).fetch(path).fetch(name)
    end

    def erase_key(key)
      _, domain, path, name = key
      paths = @jar[domain] or return
      hash = paths[path] or return
      cookie = hash.delete(name) or return
      record_removed(cookie)
    end

    def record_removed(cookie)
      size = self.class.cookie_bytes(cookie)
      site = site_of(cookie)
      stats = @sites[site]
      stats[:count] -= 1
      stats[:bytes] -= size
      @sites.delete(site) if stats[:count] <= 0
      @total_bytes -= size
    end

    # Sorts store keys in a stable least-recently-accessed order.
    # Ties are broken deterministically by creation time and the
    # identity tuple so that the traversal order of unrelated domains
    # never affects the result.
    def lru_candidates(keys, now)
      now = now.to_f
      keys.map { |key|
        cookie = cookie_at(key)
        _, domain, path, name = key
        accessed_at = cookie.accessed_at
        created_at = cookie.created_at
        [
          accessed_at ? accessed_at.to_f : now,
          created_at ? created_at.to_f : now,
          domain,
          path,
          name,
          key,
        ]
      }.sort_by { |accessed_at, created_at, domain, path, name, key|
        [accessed_at, created_at, domain, path, name]
      }.map { |_, _, _, _, _, key|
        key
      }
    end

    def purge_expired(now, session = false)
      @jar.each { |domain, paths|
        paths.each { |path, hash|
          hash.delete_if { |name, cookie|
            if cookie.expired?(now) || (session && cookie.session?)
              record_removed(cookie)
              true
            else
              false
            end
          }
        }
      }
      prune
    end

    def prune
      @jar.delete_if { |domain, paths|
        paths.delete_if { |path, hash|
          hash.empty?
        }
        paths.empty?
      }
    end
  end
end
