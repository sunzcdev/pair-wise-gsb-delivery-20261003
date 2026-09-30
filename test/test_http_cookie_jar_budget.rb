# frozen_string_literal: false
require File.expand_path('helper', File.dirname(__FILE__))

module TestHTTPCookieJar
  # Tests for the write-time capacity budget of HashStore
  # (:max_cookies_per_site and :max_total_bytes).
  #
  # All timestamps are fixed Time objects instead of the wall clock,
  # so that expiry and access order are fully deterministic.  The
  # passage of time is simulated by moving the expires attribute of a
  # stored cookie into the past.
  class TestHashStoreBudget < Test::Unit::TestCase
    FAR_FUTURE = Time.at(4_000_000_000).freeze
    PAST       = Time.at(1).freeze

    def make_cookie(name, value, domain, accessed_at:, created_at: nil, expires: FAR_FUTURE)
      HTTP::Cookie.new(
        :name => name, :value => value, :domain => domain,
        :path => '/', :for_domain => true, :expires => expires,
        :origin => "http://#{domain}/"
      ).tap { |cookie|
        cookie.created_at = created_at || accessed_at
        cookie.accessed_at = accessed_at
      }
    end

    def new_jar(options)
      HTTP::CookieJar.new({ :store => :hash }.merge(options))
    end

    def names_in(jar)
      jar.to_a.map(&:name).sort
    end

    def test_no_limits_by_default
      jar = new_jar({})
      assert_nil jar.store.bytes_remaining
      jar.add(make_cookie('c1', 'v', 'a.example.com', :accessed_at => Time.at(100)))
      assert_equal 3, jar.store.total_bytes
    end

    def test_per_site_limit_groups_by_domain_name_and_evicts_lru_within_site
      jar = new_jar(:max_cookies_per_site => 2)

      # s1 and s2 live on different hostnames but share the site
      # example.com per their DomainName registered domain.
      jar.add(make_cookie('s1', '1', 'a.example.com', :accessed_at => Time.at(100)))
      jar.add(make_cookie('s2', '2', 'b.example.com', :accessed_at => Time.at(200)))
      jar.add(make_cookie('o1', '3', 'c.other.org',   :accessed_at => Time.at(50)))
      assert_equal %w[o1 s1 s2], names_in(jar)

      jar.add(make_cookie('s3', '4', 'a.example.com', :accessed_at => Time.at(300)))

      # s1 is the least recently accessed cookie of the site
      # example.com and gets evicted; the unrelated site other.org is
      # left alone even though o1 is the oldest overall.
      assert_equal %w[o1 s2 s3], names_in(jar)
    end

    def test_total_bytes_limit_evicts_lru_globally
      jar = new_jar(:max_total_bytes => 30)
      store = jar.store

      # Each cookie is name (2 bytes) + value (8 bytes) = 10 bytes.
      jar.add(make_cookie('c1', 'v' * 8, 'd1.example.com', :accessed_at => Time.at(100)))
      jar.add(make_cookie('c2', 'v' * 8, 'd2.example.com', :accessed_at => Time.at(200)))
      jar.add(make_cookie('c3', 'v' * 8, 'd3.example.com', :accessed_at => Time.at(300)))
      assert_equal 30, store.total_bytes
      assert_equal 0, store.bytes_remaining

      jar.add(make_cookie('c4', 'v' * 8, 'd4.example.com', :accessed_at => Time.at(400)))
      assert_equal %w[c2 c3 c4], names_in(jar)
      assert_equal 30, store.total_bytes
      assert_equal 0, store.bytes_remaining

      # A 15 byte cookie needs two 10 byte victims, evicted in access
      # order across unrelated domains.
      jar.add(make_cookie('c5', 'v' * 13, 'd5.example.com', :accessed_at => Time.at(500)))
      assert_equal %w[c4 c5], names_in(jar)
      assert_equal 25, store.total_bytes
      assert_equal 5, store.bytes_remaining
    end

    def test_expired_cookies_are_reclaimed_before_lru_eviction
      jar = new_jar(:max_total_bytes => 30)
      store = jar.store

      c1 = make_cookie('c1', 'v' * 8, 'd1.example.com', :accessed_at => Time.at(300))
      jar.add(c1)
      jar.add(make_cookie('c2', 'v' * 8, 'd2.example.com', :accessed_at => Time.at(100)))
      jar.add(make_cookie('c3', 'v' * 8, 'd3.example.com', :accessed_at => Time.at(200)))
      assert_equal 30, store.total_bytes

      # Simulate the clock advancing past c1's lifetime.
      c1.expires = PAST

      jar.add(make_cookie('c4', 'v' * 8, 'd4.example.com', :accessed_at => Time.at(400)))

      # The expired c1 is reclaimed first, so c2 survives even though
      # it is the least recently accessed live cookie.
      assert_equal %w[c2 c3 c4], names_in(jar)
      assert_equal 30, store.total_bytes
    end

    def test_replacement_is_not_counted_twice
      jar = new_jar(:max_total_bytes => 25)
      store = jar.store

      jar.add(make_cookie('aa', 'x' * 8, 'a.example.com', :accessed_at => Time.at(100)))
      jar.add(make_cookie('bb', 'y' * 8, 'b.example.com', :accessed_at => Time.at(200)))
      assert_equal 20, store.total_bytes

      # Replacing aa (10 bytes) with a 15 byte value must net to 25,
      # not 35, so bb must not be evicted.
      jar.add(make_cookie('aa', 'z' * 13, 'a.example.com', :accessed_at => Time.at(300)))
      assert_equal %w[aa bb], names_in(jar)
      assert_equal 25, store.total_bytes
      assert_equal 0, store.bytes_remaining
      assert_equal 'z' * 13, jar.to_a.find { |c| c.name == 'aa' }.value
    end

    def test_oversized_cookie_is_rejected_keeping_old_value
      jar = new_jar(:max_total_bytes => 20)
      store = jar.store

      jar.add(make_cookie('aa', 'x' * 8, 'a.example.com', :accessed_at => Time.at(100)))
      assert_equal 10, store.total_bytes

      # A single cookie larger than the whole budget never fits.
      jar.add(make_cookie('bb', 'y' * 20, 'b.example.com', :accessed_at => Time.at(200)))
      assert_equal %w[aa], names_in(jar)
      assert_equal 10, store.total_bytes

      # Replacing aa with an oversized value keeps the old value.
      jar.add(make_cookie('aa', 'z' * 19, 'a.example.com', :accessed_at => Time.at(300)))
      assert_equal %w[aa], names_in(jar)
      assert_equal 'x' * 8, jar.to_a.find { |c| c.name == 'aa' }.value
      assert_equal 10, store.total_bytes
    end

    def test_add_all_commits_batch_and_evicts_lru
      jar = new_jar(:max_total_bytes => 20)
      store = jar.store

      jar.add(make_cookie('e1', 'v' * 8, 'a.example.com', :accessed_at => Time.at(100)))

      batch = [
        make_cookie('b1', 'v' * 8, 'b.example.com', :accessed_at => Time.at(200)),
        make_cookie('b2', 'v' * 8, 'c.example.com', :accessed_at => Time.at(300)),
      ]
      assert_equal true, store.add_all(batch)

      # e1 is the least recently accessed and makes room for the batch.
      assert_equal %w[b1 b2], names_in(jar)
      assert_equal 20, store.total_bytes
    end

    def test_add_all_rejects_oversized_batch_without_committing
      jar = new_jar(:max_total_bytes => 20)
      store = jar.store

      jar.add(make_cookie('e1', 'v' * 8, 'a.example.com', :accessed_at => Time.at(100)))

      # The batch alone (30 bytes) exceeds the budget.
      batch = [
        make_cookie('b1', 'v' * 13, 'b.example.com', :accessed_at => Time.at(200)),
        make_cookie('b2', 'v' * 13, 'c.example.com', :accessed_at => Time.at(300)),
      ]
      assert_equal false, store.add_all(batch)
      assert_equal %w[e1], names_in(jar)
      assert_equal 10, store.total_bytes

      # A batch containing one cookie that can never fit is rejected
      # as a whole.
      batch = [
        make_cookie('b3', 'v' * 8,  'b.example.com', :accessed_at => Time.at(200)),
        make_cookie('b4', 'v' * 30, 'c.example.com', :accessed_at => Time.at(300)),
      ]
      assert_equal false, store.add_all(batch)
      assert_equal %w[e1], names_in(jar)
      assert_equal 10, store.total_bytes
    end

    def test_add_all_rejects_batch_exceeding_per_site_limit
      jar = new_jar(:max_cookies_per_site => 2)
      store = jar.store

      batch = [
        make_cookie('b1', '1', 'a.example.com', :accessed_at => Time.at(100)),
        make_cookie('b2', '2', 'b.example.com', :accessed_at => Time.at(200)),
        make_cookie('b3', '3', 'c.example.com', :accessed_at => Time.at(300)),
      ]
      assert_equal false, store.add_all(batch)
      assert_equal [], names_in(jar)
      assert_equal 0, store.total_bytes

      assert_equal true, store.add_all(batch.first(2))
      assert_equal %w[b1 b2], names_in(jar)
    end

    def test_add_all_accounts_replacements_within_and_against_store
      jar = new_jar(:max_total_bytes => 20)
      store = jar.store

      jar.add(make_cookie('aa', 'x' * 8, 'a.example.com', :accessed_at => Time.at(100)))

      # aa is replaced by a 15 byte value and bb adds 5 bytes; the
      # batch itself is 20 bytes and replaces 10 stored bytes.
      batch = [
        make_cookie('aa', 'z' * 13, 'a.example.com', :accessed_at => Time.at(200)),
        make_cookie('aa', 'w' * 13, 'a.example.com', :accessed_at => Time.at(300)),
        make_cookie('bb', 'v' * 3,  'b.example.com', :accessed_at => Time.at(400)),
      ]
      assert_equal true, store.add_all(batch)

      # The last aa in the batch wins and the old aa is not counted.
      assert_equal %w[aa bb], names_in(jar)
      assert_equal 'w' * 13, jar.to_a.find { |c| c.name == 'aa' }.value
      assert_equal 20, store.total_bytes
    end

    def test_eviction_is_independent_of_domain_traversal_order
      build = ->(order) {
        jar = new_jar(:max_total_bytes => 24)
        specs = {
          # d1 and d2 tie on both access and creation time, so the
          # deterministic identity tie-break decides between them.
          'd1' => [Time.at(100), Time.at(50)],
          'd2' => [Time.at(100), Time.at(50)],
          'd3' => [Time.at(200), Time.at(60)],
        }
        order.each { |name|
          accessed_at, created_at = specs[name]
          jar.add(make_cookie(name, 'v' * 6, "#{name}.example.com",
                              :accessed_at => accessed_at, :created_at => created_at))
        }
        jar
      }

      jar_a = build.call(%w[d1 d2 d3])
      jar_b = build.call(%w[d3 d2 d1])

      [jar_a, jar_b].each { |jar|
        jar.add(make_cookie('n1', 'v' * 6, 'n1.example.com', :accessed_at => Time.at(300)))
      }

      # Reordering the traversal of unrelated domains does not change
      # who gets evicted: d1 loses the tie against d2 in both jars.
      assert_equal %w[d2 d3 n1], names_in(jar_a)
      assert_equal %w[d2 d3 n1], names_in(jar_b)
      assert_equal 24, jar_a.store.total_bytes
      assert_equal 24, jar_b.store.total_bytes
    end
  end
end
