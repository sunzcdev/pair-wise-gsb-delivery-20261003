require_relative 'spec_helper'
require 'minitest/mock'

class ReleaseRecursorCacheTest < Minitest::Test
  def setup
    @klass = Dnsruby::Recursor
    @original = %i[@@zones_cache @@zone_expirations @@authority_cache @@hints].to_h do |name|
      [name, @klass.class_variable_get(name)]
    end
    @root = {'ns.root.' => @klass::AddressCache.new}
    @root.fetch('ns.root.').push(['192.0.2.53', 300])
    @klass.class_variable_set(:@@zones_cache, {'.' => @root})
    @klass.class_variable_set(:@@hints, @root)
    @klass.class_variable_set(:@@zone_expirations, {})
    @klass.class_variable_set(:@@authority_cache, {})
    @recursor = @klass.allocate
    @now = Time.utc(2026, 11, 14, 12)
  end

  def teardown
    @original.each { |name, value| @klass.class_variable_set(name, value) }
  end

  def test_delegation_expires_before_longer_lived_glue
    addresses = @klass::AddressCache.new
    Time.stub(:now, @now) do
      addresses.push(['192.0.2.53', 300])
      @klass.cache_zone('example.', {'ns.example.' => addresses}, 30)
      assert_equal('example.', @recursor.get_closest_known_zone_for('WWW.EXAMPLE.'))
    end
    Time.stub(:now, @now + 30) do
      assert_equal(['192.0.2.53'], addresses.values)
      assert_equal('.', @recursor.get_closest_known_zone_for('www.example.'))
    end
  end

  def test_working_authority_map_is_separate_from_shared_mapping
    @klass.cache_zone('example.', {'ns.example.' => @klass::AddressCache.new}, 30)
    _zone, working = @recursor.get_closest_known_zone_authorities_for('www.example.')
    working.delete('ns.example.')
    assert_equal(['ns.example.'], @klass.class_variable_get(:@@zones_cache).fetch('example.').keys)
  end

  def test_empty_working_map_cannot_evict_a_newer_delegation
    replacement = {'ns.example.' => @klass::AddressCache.new}
    replacement.fetch('ns.example.').push(['192.0.2.54', 60])
    working = {}
    klass = @klass
    working.define_singleton_method(:values) do
      klass.cache_zone('example.', replacement, 60)
      []
    end
    @klass.cache_zone('example.', {}, 30)
    published = @klass.class_variable_get(:@@zones_cache).fetch('example.')
    published.define_singleton_method(:dup) { working }

    zone, authorities = @recursor.get_closest_known_zone_authorities_for('www.example.')
    assert_equal('example.', zone)
    assert_equal(replacement.keys, authorities.keys)
    assert_equal(['192.0.2.54'], authorities.fetch('ns.example.').values)
    latest = @klass.class_variable_get(:@@zones_cache).fetch('example.')
    refute_same(replacement, latest)
    refute_same(latest, authorities)
    assert_same(latest, @klass.class_variable_get(:@@zone_expirations)['example.'].first)
  end

  def test_zero_ttl_delegation_is_not_retained
    @klass.cache_zone('example.', {'ns.example.' => @klass::AddressCache.new}, 0)
    assert_equal('.', @recursor.get_closest_known_zone_for('www.example.'))
  end

  def test_address_cache_length_excludes_expired_addresses
    addresses = @klass::AddressCache.new
    Time.stub(:now, @now) { addresses.push(['192.0.2.53', 5]) }
    Time.stub(:now, @now + 5) { assert_equal(0, addresses.length) }
  end

  def test_fresh_zero_ttl_delegation_invalidates_an_older_mapping
    @klass.cache_zone('example.', {'ns.example.' => @klass::AddressCache.new}, 60)
    @klass.cache_zone('example.', {'other.example.' => @klass::AddressCache.new}, 0)
    assert_equal('.', @recursor.get_closest_known_zone_for('www.example.'))
  end

  def test_top_level_recursor_caches_nil_failure_and_honors_do_caching
    resolver = Dnsruby::Resolver.new(nameserver: ['192.0.2.53'])
    recursor = @klass.new(resolver)
    count = 0
    recursor.define_singleton_method(:_dorecursion) { |*_args| count += 1; nil }
    2.times { assert_nil(recursor.query('missing.example.', 'A', 'IN', true)) }
    assert_equal(1, count)
    resolver.do_caching = false
    assert_nil(recursor.query('missing.example.', 'A', 'IN', true))
    assert_equal(2, count)
  ensure
    resolver&.close
  end

  def test_increasing_recursion_limits_does_not_reuse_a_smaller_limits_failure
    resolver = Dnsruby::Resolver.new(nameserver: ['192.0.2.53'])
    recursor = @klass.new(resolver)
    count = 0
    recursor.define_singleton_method(:_dorecursion) do |*_args|
      count += 1
      2.times { Dnsruby::Recursor.send(:current_budget).step! }
      nil
    end
    recursor.recursion_limits = {max_steps: 1}
    2.times { assert_raises(Dnsruby::RecursionLimitError) { recursor.query('example.', 'A', 'IN', true) } }
    assert_equal(2, count, 'Shared recursion budget exhaustion must not cache a question failure')
    recursor.recursion_limits = {max_steps: 2}
    assert_nil(recursor.query('example.', 'A', 'IN', true))
    assert_equal(3, count)
  ensure
    resolver&.close
  end

  def test_cached_completion_honors_cancellation_and_deadlines
    resolver = Dnsruby::Resolver.new(nameserver: ['192.0.2.53'])
    recursor = @klass.new(resolver)
    count = 0
    recursor.define_singleton_method(:_dorecursion) { |*_args| count += 1; nil }
    recursor.query('missing.example.', 'A', 'IN', true)
    control = Dnsruby::QueryControl.new
    control.cancel
    Dnsruby::QueryControl.stub(:current, control) do
      assert_raises(Dnsruby::QueryControl::Cancelled) do
        recursor.query('missing.example.', 'A', 'IN', true)
      end
    end
    expired = Dnsruby::QueryControl.new(deadline: Dnsruby::TransportClock.now - 1)
    Dnsruby::QueryControl.stub(:current, expired) do
      assert_raises(Dnsruby::QueryControl::DeadlineExceeded) do
        recursor.query('missing.example.', 'A', 'IN', true)
      end
    end
    assert_equal(1, count)
  ensure
    resolver&.close
  end

  def test_top_level_recursor_caches_negative_answers_and_scopes_validation_policy
    resolver = Dnsruby::Resolver.new(nameserver: ['192.0.2.53'])
    recursor = @klass.new(resolver)
    response = Dnsruby::Message.new('missing.example.', 'A')
    response.header.rcode = Dnsruby::RCode::NXDOMAIN
    response.add_authority(Dnsruby::RR.create('example. 60 IN SOA ns.example. hostmaster.example. 1 60 60 60 30'))
    count = 0
    recursor.define_singleton_method(:_dorecursion) { |*_args| count += 1; response }
    refute(recursor.query('missing.example.', 'A', 'IN', true).cached)
    assert(recursor.query('missing.example.', 'A', 'IN', true).cached)
    assert_equal(1, count)
    recursor.dnssec = true
    recursor.query('missing.example.', 'A', 'IN', true)
    assert_equal(2, count)
  ensure
    resolver&.close
  end

  def test_top_level_nil_failure_does_not_attempt_dnssec_validation
    resolver = Dnsruby::Resolver.new(nameserver: ['192.0.2.53'])
    recursor = @klass.new(resolver)
    count = 0
    recursor.define_singleton_method(:_dorecursion) { |*_args| count += 1; nil }
    Dnsruby::Dnssec.stub(:validate, ->(*) { flunk('There is no response to validate') }) do
      2.times { assert_nil(recursor.query('missing.example.')) }
    end
    assert_equal(1, count)
  ensure
    resolver&.close
  end

  def test_top_level_name_error_exception_keeps_its_exception_convention
    resolver = Dnsruby::Resolver.new(nameserver: ['192.0.2.53'])
    recursor = @klass.new(resolver)
    response = Dnsruby::Message.new('missing.example.', 'A')
    response.header.rcode = Dnsruby::RCode::NXDOMAIN
    response.add_authority(Dnsruby::RR.create('example. 60 IN SOA ns.example. hostmaster.example. 1 60 60 60 30'))
    count = 0
    recursor.define_singleton_method(:_dorecursion) do |*_args|
      count += 1
      error = Dnsruby::NXDomain.new('missing')
      error.response = response
      raise error
    end
    2.times do
      error = assert_raises(Dnsruby::NXDomain) { recursor.query('missing.example.', 'A', 'IN', true) }
      assert_equal(Dnsruby::RCode::NXDOMAIN, error.response.rcode)
    end
    assert_equal(1, count)
  ensure
    resolver&.close
  end

  def test_outer_maps_and_address_lists_have_capacity_bounds
    # Exercise bounded insertion with a small temporary capacity, not a load test.
    old_limit = @klass::CACHE_LIMIT
    @klass.send(:remove_const, :CACHE_LIMIT)
    @klass.const_set(:CACHE_LIMIT, 3)
    4.times do |index|
      addresses = @klass::AddressCache.new
      @klass.cache_authority("ns#{index}.example.", addresses)
      @klass.cache_zone("zone#{index}.example.", {"ns#{index}.example." => addresses}, 30)
    end
    assert_equal(3, @klass.class_variable_get(:@@authority_cache).length)
    zones = @klass.class_variable_get(:@@zones_cache)
    assert_equal(3, zones.length)
    assert_same(@root, zones['.'])
  ensure
    @klass.send(:remove_const, :CACHE_LIMIT)
    @klass.const_set(:CACHE_LIMIT, old_limit)
  end
end
