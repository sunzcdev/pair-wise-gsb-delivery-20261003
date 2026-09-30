require 'helper'

# Tests for the write-time capacity budgets of HashStore:
# configurable per-site cookie count and total byte caps, stable
# least-recently-accessed eviction, site grouping by DomainName,
# replacement accounting and atomic batch loads.  The clock and the
# access order are fully controlled by explicit Time values, and
# each expectation names the exact cookies evicted and the bytes
# left.
class TestHashStoreQuota < Test::Unit::TestCase
  class FakeClock
    attr_accessor :now
    def initialize(time)
      @now = Time.at(time)
    end
  end

  GC_THRESHOLD = 10_000

  def setup
    @clock = FakeClock.new(100)
  end

  def new_store(options = {})
    HTTP::CookieJar::HashStore.new({
        :clock => @clock,
        :gc_threshold => GC_THRESHOLD,
      }.update(options))
  end

  def make_cookie(name, value, host, created_at, accessed_at = nil)
    cookie = HTTP::Cookie.new(name, value,
      :domain => host,
      :path => '/',
      :for_domain => true,
      :origin => "http://#{host}/")
    cookie.created_at = Time.at(created_at)
    cookie.accessed_at = Time.at(accessed_at || created_at)
    cookie
  end

  def bytes(cookie)
    HTTP::CookieJar::HashStore.cookie_bytes(cookie)
  end

  def names(store)
    store.map(&:name).sort
  end

  # Advances the fake clock and simulates sending the cookie to its
  # origin URI, which updates its last-access time.
  def touch(store, cookie, time)
    @clock.now = Time.at(time)
    found = false
    store.each(URI("http://#{cookie.domain}/")) { |candidate|
      if candidate.name == cookie.name
        found = true
      end
    }
    assert(found, "cookie #{cookie.name} was expected to be in the store")
  end

  # Builds a set of cookies spread over many unrelated sites.  The
  # block receives the insertion index and returns the host name;
  # this lets the same logical set be inserted in different domain
  # traversal orders.
  def build_population(store, hosts)
    hosts.each_with_index { |host, index|
      # The logical index is derived from the host name, so a cookie
      # keeps the same creation/access times no matter in which order
      # the domains are inserted.
      logical = host[/(\d+)\./, 1].to_i
      time = logical
      cookie = make_cookie("n#{logical}", "v#{logical}", host, time, time)
      assert(store.add(cookie))
    }
  end

  def test_per_site_cap_evicts_least_recently_accessed
    store = new_store(:max_cookies_per_site => 2)
    oldest = make_cookie('old', '1', 'a.example.org', 1, 5)
    middle = make_cookie('mid', '22', 'b.example.org', 2, 10)
    newest = make_cookie('new', '333', 'shop.example.org', 3, 3)

    store.add(oldest)
    store.add(middle)

    @clock.now = 60
    store.add(newest)

    # middle was accessed most recently; oldest (accessed_at 5) is
    # the site LRU and is evicted by the incoming cookie.
    assert_equal %w[mid new], names(store)
    assert_equal({ 'example.org' => 2 }, store.cookies_per_site)
    assert_equal bytes(middle) + bytes(newest), store.total_bytes
  end

  def test_per_site_cap_groups_hosts_by_registrable_domain
    store = new_store(:max_cookies_per_site => 2)
    one = make_cookie('one', 'a', 'a.example.org', 1, 1)
    two = make_cookie('two', 'bb', 'b.example.org', 2, 2)
    unrelated = make_cookie('other', 'ccc', 'other.example.net', 3, 3)

    store.add(one)
    store.add(two)
    store.add(unrelated)

    # The new cookie is a third cookie of the example.org site while
    # the unrelated site must stay untouched.
    incoming = make_cookie('three', 'dddd', 'a.example.org', 4, 40)
    store.add(incoming)

    # one is the least recently accessed cookie of the site and is
    # evicted; two and the incoming cookie remain.
    assert_equal %w[other three two], names(store)
    assert_equal({
        'example.org' => 2,
        'example.net' => 1,
      }, store.cookies_per_site)
    assert_equal bytes(two) + bytes(unrelated) + bytes(incoming),
                 store.total_bytes
  end

  def test_lru_order_is_stable_on_equal_access_times
    store = new_store(:max_cookies_per_site => 2)
    first  = make_cookie('first',  '1', 'a.example.org', 1, 10)
    second = make_cookie('second', '2', 'b.example.org', 2, 10)
    third  = make_cookie('third',  '3', 'c.example.org', 3, 10)
    store.add(first)
    store.add(second)

    @clock.now = 20
    store.add(third)

    # Equal access times fall back to creation time: the oldest goes.
    assert_equal %w[second third], names(store)
    assert_equal bytes(second) + bytes(third), store.total_bytes
  end

  def test_total_bytes_evicts_across_sites
    store = new_store(:max_total_bytes => 15)
    old = make_cookie('old', 'aaaa', 'a.example.org', 1, 1) # 7 bytes
    mid = make_cookie('mid', 'bbb', 'other.example.net', 2, 2) # 6 bytes

    store.add(old)
    store.add(mid)
    assert_equal 13, store.total_bytes

    touch(store, old, 50)

    @clock.now = 60
    fresh = make_cookie('new', 'c', 'third.example.com', 3, 60) # 4 bytes
    store.add(fresh)

    # The globally least recently accessed cookie (mid) is evicted.
    assert_equal %w[new old], names(store)
    assert_equal bytes(old) + bytes(fresh), store.total_bytes
  end

  def test_expired_cookies_are_purged_before_lru_eviction
    store = new_store(:max_cookies_per_site => 2)
    fresh = make_cookie('fresh', '1', 'a.example.org', 1, 1)
    stale = make_cookie('stale', '2', 'b.example.org', 2, 50)
    stale.expires = Time.at(90)

    store.add(fresh)
    store.add(stale)

    @clock.now = Time.at(200)
    incoming = make_cookie('incoming', '3', 'a.example.org', 3, 200)
    store.add(incoming)

    # The expired cookie frees the slot even though it was accessed
    # more recently than the fresh cookie.
    assert_equal %w[fresh incoming], names(store)
    assert_equal({ 'example.org' => 2 }, store.cookies_per_site)
    assert_equal bytes(fresh) + bytes(incoming), store.total_bytes
  end

  def test_replacement_is_not_double_counted
    store = new_store(:max_cookies_per_site => 2,
                      :max_total_bytes => 20)
    original = make_cookie('k', 'aa', 'a.example.org', 1, 1) # 3 bytes
    other = make_cookie('o', 'bb', 'b.example.org', 2, 2) # 3 bytes
    store.add(original)
    store.add(other)

    @clock.now = 30
    updated = make_cookie('k', 'cccccccc', 'a.example.org', 3, 30) # 9 bytes
    store.add(updated)

    assert_equal 2, store.cookies_per_site.fetch('example.org')
    assert_equal bytes(updated) + bytes(other), store.total_bytes
    assert_equal 'cccccccc', store.to_a.find { |c| c.name == 'k' }.value
  end

  def test_oversized_replacement_keeps_the_old_value
    store = new_store(:max_total_bytes => 10)
    original = make_cookie('k', 'aa', 'a.example.org', 1, 1) # 3 bytes
    store.add(original)

    @clock.now = 10
    oversized = make_cookie('k', 'x' * 20, 'a.example.org', 2, 10)
    assert_equal(false, store.add_all([oversized]))

    surviving = store.to_a
    assert_equal ['k'], surviving.map(&:name)
    assert_equal 'aa', surviving.first.value
    assert_equal bytes(original), store.total_bytes
  end

  def test_batch_is_rejected_as_a_whole_when_a_site_overflows
    store = new_store(:max_cookies_per_site => 2)
    existing = make_cookie('existing', '1', 'a.example.org', 1, 1)
    store.add(existing)

    batch = [
      make_cookie('one', '2', 'x.example.org', 2, 2),
      make_cookie('two', '3', 'y.example.org', 3, 3),
      make_cookie('three', '4', 'z.example.org', 4, 4),
    ]
    assert_equal(false, store.add_all(batch))

    assert_equal ['existing'], names(store)
    assert_equal({ 'example.org' => 1 }, store.cookies_per_site)
    assert_equal bytes(existing), store.total_bytes
  end

  def test_batch_is_committed_when_it_fits_after_eviction
    store = new_store(:max_total_bytes => 14)
    old = make_cookie('old', 'aa', 'a.example.org', 1, 1) # 5 bytes
    mid = make_cookie('mid', 'bb', 'b.example.org', 2, 2) # 5 bytes
    store.add(old)
    store.add(mid)

    # 10 bytes are stored.  Touching old makes mid the global LRU;
    # at a 14-byte budget mid (5) and then old (5) must both go to
    # make room for the two protected incoming cookies.
    touch(store, old, 50)

    @clock.now = 60
    batch = [
      make_cookie('new1', 'c', 'c.example.org', 3, 60), # 5 bytes
      make_cookie('new2', 'd', 'd.example.org', 4, 60), # 5 bytes
    ]
    assert_equal(true, store.add_all(batch))

    # mid then old are evicted; both incoming cookies survive.
    assert_equal %w[new1 new2], names(store)
    assert_equal batch.sum { |c| bytes(c) },
                 store.total_bytes
  end

  def test_batch_replacement_accounts_for_the_replaced_bytes
    store = new_store(:max_total_bytes => 10)
    original = make_cookie('k', 'aa', 'a.example.org', 1, 1) # 3 bytes
    store.add(original)

    @clock.now = 20
    updated = make_cookie('k', 'bbbb', 'a.example.org', 2, 20) # 5 bytes
    other = make_cookie('o', 'cc', 'b.example.org', 3, 20) # 3 bytes

    # Without replacement accounting the batch would look like 11
    # bytes; replacing k counts only the new 5 bytes plus 3.
    assert_equal(true, store.add_all([updated, other]))
    assert_equal 8, store.total_bytes
    assert_equal 'bbbb', store.to_a.find { |c| c.name == 'k' }.value
  end

  def test_domain_traversal_order_does_not_change_eviction
    hosts = (1..8).map { |i| "site#{i}.example.net" }

    store_a = new_store(:max_total_bytes => 15)
    build_population(store_a, hosts)

    store_b = new_store(:max_total_bytes => 15)
    build_population(store_b, hosts.reverse)

    # The stores hold the same logical cookies with the same access
    # times; a further oversized write triggers global LRU eviction
    # in each.
    incoming_a = make_cookie('boom', 'z' * 5, 'incoming.example.com',
                             100, 100)
    incoming_b = make_cookie('boom', 'z' * 5, 'incoming.example.com',
                             100, 100)
    @clock.now = 100
    store_a.add(incoming_a)
    store_b.add(incoming_b)

    survivors_a = store_a.map { |c|
      [c.name, c.domain, c.value]
    }.sort
    survivors_b = store_b.map { |c|
      [c.name, c.domain, c.value]
    }.sort

    assert_equal(survivors_a, survivors_b)
    assert_equal(store_a.total_bytes, store_b.total_bytes)
    # The incoming cookie is protected from eviction.
    assert(survivors_a.include?(['boom', 'incoming.example.com',
                                'z' * 5]))
    assert(store_a.total_bytes <= 15)
  end
end
