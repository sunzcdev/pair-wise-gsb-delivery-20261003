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
        :max_cookies_per_site => nil,
        :max_total_bytes => nil,
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
    # : Maximum number of cookies allowed per site, enforced at write
    # time.  Cookies are grouped into sites by the registered domain
    # taken from each cookie's DomainName object (e.g. cookies for
    # `www.example.com` and `app.example.com` share the site
    # `example.com`), which is computed locally without any network
    # access.  When a new cookie would exceed the limit, expired
    # cookies are removed first and then the least recently accessed
    # cookies of the site are evicted.  Disabled if nil. (default:
    # nil)
    #
    # :max_total_bytes
    # : Maximum total size in bytes of the cookies in the store,
    # enforced at write time, where the size of a cookie is the sum of
    # the byte sizes of its name and value.  When a new cookie would
    # exceed the limit, expired cookies are removed first and then the
    # least recently accessed cookies are evicted until the new cookie
    # fits.  A single cookie larger than the limit is rejected and any
    # old value it would replace is kept.  Disabled if nil. (default:
    # nil)
    def initialize(options = nil)
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
      @total_bytes = 0
    end

    # The copy constructor.  This store class supports cloning.
    def initialize_copy(other)
      @jar = Marshal.load(Marshal.dump(other.instance_variable_get(:@jar)))
    end

    # Returns the total size in bytes of the cookies currently stored,
    # where the size of a cookie is the sum of the byte sizes of its
    # name and value.
    attr_reader :total_bytes

    # Returns how many more bytes of cookies can be stored under the
    # `:max_total_bytes` limit, or nil if no limit is set.
    def bytes_remaining
      @max_total_bytes && @max_total_bytes - @total_bytes
    end

    def add(cookie)
      if budget_limited?
        add_with_budget(cookie)
      else
        path_cookies = ((@jar[cookie.domain] ||= {})[cookie.path] ||= {})
        if old = path_cookies[cookie.name]
          @total_bytes -= cookie_size(old)
        end
        path_cookies[cookie.name] = cookie
        @total_bytes += cookie_size(cookie)
        cleanup if (@gc_index += 1) >= @gc_threshold
      end
      self
    end

    # Adds multiple cookies as a single atomic operation with regard
    # to the capacity budget.
    #
    # Before anything is stored, the whole change is checked against
    # the configured limits: the batch itself, where cookies sharing
    # the same name, domain and path replace each other, must fit
    # within `:max_total_bytes` and `:max_cookies_per_site`.  If it
    # does not fit, nothing is stored and false is returned.
    # Otherwise expired cookies are removed, the least recently
    # accessed cookies are evicted as needed, the batch is committed
    # and true is returned.
    def add_all(cookies)
      cookies = cookies.to_a
      return true if cookies.empty?

      unless budget_limited?
        cookies.each { |cookie| add(cookie) }
        return true
      end

      synchronize {
        # De-duplicate the batch by identity (name, domain, path); the
        # last cookie wins.
        batch = {}
        keys = []
        cookies.each { |cookie|
          key = [cookie.domain, cookie.path, cookie.name]
          keys << key unless batch.key?(key)
          batch[key] = cookie
        }

        batch_bytes = 0
        batch.each_value { |cookie|
          size = cookie_size(cookie)
          # A single cookie that can never fit rejects the whole batch.
          return false if @max_total_bytes && size > @max_total_bytes
          batch_bytes += size
        }

        # The batch alone must fit in the byte budget; room for it can
        # always be made by evicting old cookies.
        return false if @max_total_bytes && batch_bytes > @max_total_bytes

        if @max_cookies_per_site
          counts = Hash.new(0)
          batch.each_value { |cookie|
            counts[site_key(cookie)] += 1
          }
          counts.each_value { |count|
            return false if count > @max_cookies_per_site
          }
        end

        keys.each { |key| add_with_budget(batch[key]) }
        true
      }
    end

    def delete(cookie)
      path_cookies = ((@jar[cookie.domain] ||= {})[cookie.path] ||= {})
      if old = path_cookies.delete(cookie.name)
        @total_bytes -= cookie_size(old)
      end
      self
    end

    def each(uri = nil) # :yield: cookie
      now = Time.now
      if uri
        tpath = uri.path
        @jar.each { |domain, paths|
          paths.each { |path, hash|
            next unless HTTP::Cookie.path_match?(path, tpath)
            hash.delete_if { |name, cookie|
              if cookie.expired?(now)
                @total_bytes -= cookie_size(cookie)
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
      else
        synchronize {
          @jar.each { |domain, paths|
            paths.each { |path, hash|
              hash.delete_if { |name, cookie|
                if cookie.expired?(now)
                  @total_bytes -= cookie_size(cookie)
                  true
                else
                  yield cookie
                  false
                end
              }
            }
          }
        }
      end
      self
    end

    def clear
      @jar.clear
      @total_bytes = 0
      self
    end

    def cleanup(session = false)
      now = Time.now
      all_cookies = []

      synchronize {
        break if @gc_index == 0

        @jar.each { |domain, paths|
          domain_cookies = []

          paths.each { |path, hash|
            hash.delete_if { |name, cookie|
              if cookie.expired?(now) || (session && cookie.session?)
                @total_bytes -= cookie_size(cookie)
                true
              else
                domain_cookies << cookie
                false
              end
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

        prune_empty

        @gc_index = 0
      }
      self
    end

    private

    # The size of a cookie in bytes for capacity accounting.
    def cookie_size(cookie)
      cookie.name.bytesize + cookie.value.bytesize
    end

    def budget_limited?
      !!(@max_cookies_per_site || @max_total_bytes)
    end

    # Groups cookies into sites using the registered domain of each
    # cookie's DomainName object, falling back to the bare hostname
    # for public suffixes and IP addresses.  This is computed locally
    # and never touches the network.
    def site_key(cookie)
      domain_name = cookie.domain_name
      (domain_name && domain_name.domain) || cookie.domain
    end

    # Orders cookies from least to most recently accessed.  Ties are
    # broken by creation time and then by identity, so the result is a
    # total order that does not depend on hash traversal order.
    def lru_order(cookies)
      cookies.sort_by { |cookie|
        [cookie.accessed_at, cookie.created_at, cookie.dot_domain, cookie.path, cookie.name]
      }
    end

    def all_cookies
      cookies = []
      @jar.each_value { |paths|
        paths.each_value { |hash|
          hash.each_value { |cookie| cookies << cookie }
        }
      }
      cookies
    end

    def site_cookies(site)
      all_cookies.select { |cookie| site_key(cookie) == site }
    end

    def sweep_expired(now)
      @jar.each_value { |paths|
        paths.each_value { |hash|
          hash.delete_if { |name, cookie|
            if cookie.expired?(now)
              @total_bytes -= cookie_size(cookie)
              true
            else
              false
            end
          }
        }
      }
      prune_empty
    end

    def prune_empty
      @jar.delete_if { |domain, paths|
        paths.delete_if { |path, hash|
          hash.empty?
        }
        paths.empty?
      }
    end

    def add_with_budget(cookie)
      size = cookie_size(cookie)
      # A single cookie that can never fit is rejected, keeping any
      # old value it would have replaced.
      if @max_total_bytes && size > @max_total_bytes
        return self
      end

      synchronize {
        # Expired entries are reclaimed first.
        sweep_expired(Time.now)

        # A replaced cookie must not be counted twice; take the old
        # value off the books before making room.
        delete(cookie)

        if @max_cookies_per_site
          siblings = site_cookies(site_key(cookie))
          if (debt = siblings.size + 1 - @max_cookies_per_site) > 0
            lru_order(siblings).slice(0, debt).each { |victim|
              delete(victim)
            }
          end
        end

        if @max_total_bytes && @total_bytes + size > @max_total_bytes
          lru_order(all_cookies).each { |victim|
            break if @total_bytes + size <= @max_total_bytes
            delete(victim)
          }
        end

        ((@jar[cookie.domain] ||= {})[cookie.path] ||= {})[cookie.name] = cookie
        @total_bytes += size
        cleanup if (@gc_index += 1) >= @gc_threshold
      }
      self
    end
  end
end
