require_relative 'spec_helper'
require 'minitest/mock'

# Functional policy and cache contracts; all discovery uses local doubles.
class ReviewValidationPrimingTest < Minitest::Test
  def setup
    @klass = Dnsruby::Recursor
    @saved = %i[@@hints @@stale_hints @@hints_generation @@nameservers
      @@zones_cache @@zone_expirations @@authority_cache].to_h do |field|
      [field, @klass.class_variable_get(field)]
    end
    %i[@@hints @@zones_cache @@zone_expirations @@authority_cache].each do |field|
      @klass.class_variable_set(field, {})
    end
    @klass.class_variable_set(:@@stale_hints, nil)
  end

  def teardown
    @saved.each { |field, value| @klass.class_variable_set(field, value) }
  end

  def test_required_verification_errors_survive_provisional_classification
    verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
    message = Dnsruby::Message.new('ordinary.example.')
    expected = Dnsruby::VerifyError.new('Verification service did not complete')
    provisional = ->(_anchor, packet, _name) { packet.security_level = Dnsruby::Message::SecurityLevel.INSECURE; nil }
    # Model the verification service's result independently of wire records.
    message.stub(:rrsets, [:present]) do
      verifier.stub(:find_closest_anchor_for, nil) do
        verifier.stub(:try_to_follow_from_anchor, provisional) do
          verifier.stub(:verify_result, ->(*) { raise expected }) do
            verifier.stub(:discover_signer_keys, false) do
              actual = assert_raises(Dnsruby::VerifyError) { verifier.validate(message, nil) }
              assert_same(expected, actual)
            end
          end
        end
      end
    end
  end

  def test_unsigned_alias_classification_visits_each_required_name
    verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
    message = Dnsruby::Message.new('alias.example.')
    message.add_answer(Dnsruby::RR.create('alias.example. 60 IN CNAME target.other.'))
    message.add_answer(Dnsruby::RR.create('target.other. 60 IN A 192.0.2.1'))
    names = []
    verifier.stub(:discover_insecure_name, ->(name, _klass) { names << name.to_s(true); true }) do
      assert(verifier.validate(message, nil))
    end
    assert_equal(%w[alias.example. target.other.], names.sort)
    assert_equal(Dnsruby::Message::SecurityLevel.INSECURE, message.security_level)
  end

  def test_escaped_dots_are_not_ancestor_boundaries
    @klass.class_variable_set(:@@zones_cache, {'.' => {}, 'example.' => {}, 'b.example.' => {}})
    recursor = @klass.allocate
    assert_equal('example.', recursor.get_closest_known_zone_for('a\\.b.example.'))
    assert_equal('example.', recursor.get_closest_known_zone_for('a\\046b.example.'))
    assert_equal('example.', recursor.get_closest_known_zone_for('www.example'))
    relative = Dnsruby::Name.create('a\\.b.example')
    assert_equal('example.', recursor.get_closest_known_zone_for(relative))
    refute(relative.absolute?)
    assert_equal('.', recursor.get_closest_known_zone_for('other'))
  end

  def test_descendant_deadline_survives_parent_eviction_without_extending_it
    clock = Time.utc(2026, 11, 15)
    Time.stub(:now, -> { clock }) do
      root = {'ns.root.' => addresses(300)}
      @klass.class_variable_set(:@@hints, root)
      @klass.class_variable_set(:@@zones_cache, {'.' => root})
      @klass.class_variable_set(:@@zone_expirations, {'.' => [root, clock + 90]})
      @klass.cache_zone('example', {'ns.example.' => addresses(300)}, 20, parent: '.')
      @klass.cache_zone(Dnsruby::Name.create('child.example'), {'ns.child.example.' => addresses(300)}, 60, parent: 'example')
      @klass.class_variable_get(:@@zones_cache).delete('example.')
      @klass.class_variable_get(:@@zone_expirations).delete('example.')
      recursor = @klass.allocate
      clock += 19
      assert_equal('child.example.', recursor.get_closest_known_zone_for('www.child.example.'))
      clock += 1
      assert_equal('.', recursor.get_closest_known_zone_for('www.child.example.'))
    end
  end

  def test_partial_priming_fills_missing_root_names
    packet = root_packet(ns_ttl: 60, address_ttl: 300)
    packet.add_answer(Dnsruby::RR.create('. 60 IN NS b.root.example.'))
    resolver, lookups = priming_resolver(packet)
    @klass.set_hints([], resolver)
    assert_equal([['b.root.example.', 'A'], ['b.root.example.', 'AAAA']], lookups)
    assert_equal(%w[a.root.example b.root.example], @klass.class_variable_get(:@@hints).keys.sort)
  end

  def test_root_ns_expiration_is_independent_of_address_expiration
    clock = Time.utc(2026, 11, 15)
    Time.stub(:now, -> { clock }) do
      resolver, = priming_resolver(root_packet(ns_ttl: 5, address_ttl: 300))
      @klass.set_hints([], resolver)
      assert(@klass.send(:hints_ready?))
      clock += 5
      refute(@klass.send(:hints_ready?))
      assert_equal(['192.0.2.53'], @klass.class_variable_get(:@@hints).values.first.values)
    end
  end

  def test_zero_ttl_priming_addresses_belong_to_the_current_lookup
    resolver, = priming_resolver(root_packet(ns_ttl: 0, address_ttl: 0))
    @klass.send(:with_limits, resolver) do
      @klass.set_hints([], resolver)
      refute(@klass.send(:hints_ready?))
      generation = @klass.class_variable_get(:@@hints_generation)
      assert_equal(generation, @klass.send(:ensure_hints, resolver))
      zone, authorities = @klass.allocate.get_closest_known_zone_authorities_for('www.example.')
      assert_equal('.', zone)
      assert_equal(['192.0.2.53'], authorities.values.first.current_values)
      assert_empty(@klass.class_variable_get(:@@hints).values.first.values)
    end
    refute(@klass.send(:hints_ready?))
  end

  def test_supported_name_classification_is_memoized_within_one_operation
    verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
    anchor = Struct.new(:klass).new(Dnsruby::Classes.IN)
    calls = 0
    verifier.stub(:find_closest_anchor_for, anchor) do
      verifier.stub(:follow_chain, ->(*) { calls += 1; anchor }) do
        verifier.send(:with_operational_validation, Dnsruby::Message.new('example.')) do
          2.times { refute(verifier.send(:discover_insecure_name, Dnsruby::Name.create('example.'), Dnsruby::Classes.IN)) }
        end
      end
    end
    assert_equal(1, calls)
  end

  def test_root_ns_deadline_starts_before_address_discovery
    clock = Time.utc(2026, 11, 15)
    Time.stub(:now, -> { clock }) do
      packet = root_packet(ns_ttl: 5, address_ttl: 300)
      packet.add_answer(Dnsruby::RR.create('. 5 IN NS b.root.example.'))
      resolver, = priming_resolver(packet) { clock += 3 }
      @klass.set_hints([], resolver)
      refute(@klass.send(:hints_ready?))
      assert_equal(Time.utc(2026, 11, 15) + 5, @klass.class_variable_get(:@@zone_expirations)['.'][1])
    end
  end

  private

  def addresses(ttl)
    @klass::AddressCache.new.tap { |cache| cache.push(['192.0.2.53', ttl]) }
  end

  def root_packet(ns_ttl:, address_ttl:)
    packet = Dnsruby::Message.new('.', 'NS')
    packet.add_answer(Dnsruby::RR.create(". #{ns_ttl} IN NS a.root.example."))
    packet.add_additional(Dnsruby::RR.create("a.root.example. #{address_ttl} IN A 192.0.2.53"))
    packet
  end

  def priming_resolver(packet, &before_send)
    lookups = []
    resolver = Struct.new(:query_timeout, :recurse).new(2, false)
    resolver.define_singleton_method(:single_resolvers) { [Object.new] }
    resolver.define_singleton_method(:query_no_validation_or_recursion) { |*| packet }
    resolver.define_singleton_method(:send_async) do |query, queue|
      before_send&.call
      name, type = query.question.first.qname.to_s(true), query.question.first.qtype.to_s
      lookups << [name, type]
      response = Dnsruby::Message.new(name, type)
      value = type == 'A' ? '192.0.2.54' : '2001:db8::54'
      response.add_answer(Dnsruby::RR.create("#{name} 60 IN #{type} #{value}"))
      queue << [lookups.length, response, nil]
      lookups.length
    end
    [resolver, lookups]
  end
end
