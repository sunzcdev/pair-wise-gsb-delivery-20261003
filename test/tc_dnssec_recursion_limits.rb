require_relative 'spec_helper'
require 'minitest/mock'

# Discovery and completion boundaries use ordinary query metadata and resolver
# doubles. No sockets or DNS traffic are needed to check terminal limit errors.
class DnssecRecursionLimitsTest < Minitest::Test
  def setup
    @verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
    @zone = Dnsruby::Name.create('example.')
    @child = Dnsruby::Name.create('child.example.')
    @anchor = metadata_double(name: @zone, klass: Dnsruby::Classes.IN, type: Dnsruby::Types.DNSKEY)
    @limit = Dnsruby::RecursionLimitError.new('Recursive lookup work limit exceeded')
    @queries = []
    @saved_recursor = Dnsruby::Dnssec.do_validation_with_recursor?
    Dnsruby::Dnssec.do_validation_with_recursor(false)
  end

  def teardown
    Dnsruby::Dnssec.do_validation_with_recursor(@saved_recursor)
  end

  def test_ds_discovery_preserves_the_limit_without_caching_a_question_failure
    resolver = exhausted_resolver
    @verifier.stub(:get_nameservers_for, ->(*) { flunk('DS exhaustion must stop child discovery') }) do
      error = assert_raises(Dnsruby::RecursionLimitError) do
        @verifier.get_anchor_for(@child, @zone, @anchor, resolver)
      end
      assert_same(@limit, error)
      assert_uncached_limit(error)
    end
    assert_equal([[@child, Dnsruby::Types.DS]], @queries)
  end

  def test_dnskey_discovery_preserves_the_limit_without_trying_another_key
    resolver = exhausted_resolver
    @verifier.stub(:get_nameservers_for, resolver) do
      @verifier.stub(:verify, ->(*) { flunk('DNSKEY exhaustion must stop key fallback') }) do
        error = assert_raises(Dnsruby::RecursionLimitError) do
          @verifier.get_anchor_for(@zone, @zone, @anchor, resolver)
        end
        assert_same(@limit, error)
        assert_uncached_limit(error)
      end
    end
    assert_equal([[@zone, Dnsruby::Types.DNSKEY]], @queries)
  end

  def test_dlv_registry_key_discovery_preserves_the_limit
    verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::DLV)
    verifier.stub(:get_dlv_resolver, exhausted_resolver) do
      verifier.stub(:verify, ->(*) { flunk('An exhausted DLV lookup must not verify keys') }) do
        error = assert_raises(Dnsruby::RecursionLimitError) { verifier.get_dlv_key(@anchor) }
        assert_same(@limit, error)
        assert_empty(verifier.trusted_keys)
      end
    end
    assert_equal([[Dnsruby::Name.create('dlv.isc.org.'), Dnsruby::Types.DNSKEY]], @queries)
  end

  def test_dlv_zone_key_discovery_preserves_the_limit
    verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::DLV)
    verifier.stub(:get_nameservers_for, exhausted_resolver) do
      verifier.stub(:verify, ->(*) { flunk('An exhausted zone lookup must not verify keys') }) do
        error = assert_raises(Dnsruby::RecursionLimitError) do
          verifier.get_zone_key_from_dlv_rrset(metadata_double(type: Dnsruby::Types.DLV), @child)
        end
        assert_same(@limit, error)
        assert_empty(verifier.trusted_keys)
      end
    end
    assert_equal([[@child, Dnsruby::Types.DNSKEY]], @queries)
  end

  def test_dlv_record_discovery_stops_before_trying_the_next_ancestor
    verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::DLV)
    verifier.stub(:get_dlv_resolver, exhausted_resolver) do
      verifier.stub(:get_zone_key_from_dlv_rrset, ->(*) { flunk('Exhaustion must stop DLV discovery') }) do
        error = assert_raises(Dnsruby::RecursionLimitError) { verifier.find_closest_dlv_anchor_for(@child) }
        assert_same(@limit, error)
      end
    end
    assert_equal([[Dnsruby::Name.create('child.example.dlv.isc.org.'), Dnsruby::Types.DLV]], @queries)
  end

  def test_exhausted_discovery_stops_validation_policy_fallbacks
    previous_policy = Dnsruby::Dnssec.validation_policy
    query = Dnsruby::Message.new(@child, 'A')
    query.header.cd = true
    resolver = exhausted_resolver
    discover = ->(*) { @verifier.get_anchor_for(@child, @zone, @anchor, resolver) }
    fallback = ->(*) { flunk('Aggregate exhaustion must not try another validation policy') }
    policies = Dnsruby::Dnssec::ValidationPolicy
    [[policies::LOCAL_ANCHORS_THEN_ROOT, :validate_with_anchors, :validate_with_root],
      [policies::ROOT_THEN_LOCAL_ANCHORS, :validate_with_root, :validate_with_anchors]].each do |policy, first, second|
      @queries.clear
      Dnsruby::Dnssec.validation_policy = policy
      Dnsruby::Dnssec.stub(first, discover) do
        Dnsruby::Dnssec.stub(second, fallback) do
          Dnsruby::Dnssec.stub(:validate_with_dlv, fallback) do
            error = assert_raises(Dnsruby::RecursionLimitError) do
              Dnsruby::Dnssec.validate_with_query(query, positive_response)
            end
            assert_same(@limit, error)
          end
        end
      end
      assert_equal([[@child, Dnsruby::Types.DS]], @queries)
    end
  ensure
    Dnsruby::Dnssec.validation_policy = previous_policy
  end

  def test_async_validator_completion_preserves_the_limit_and_avoids_both_caches
    worker = nil
    query = Dnsruby::Message.new(@child, 'A')
    query.do_validation = true
    response = positive_response
    completions, registrations, removals, positive_cache_calls = [], [], [], []
    cache = Dnsruby::Cache::ResolutionCache.new
    selector = Object.new
    selector.define_singleton_method(:register_validation) { |*args| registrations << args; true }
    selector.define_singleton_method(:unregister_validation) { |*args| removals << args }
    selector.define_singleton_method(:push_validation_response_to_select) do |*args|
      completions << args
      cache.add(args[4], :test_scope, args[2], args[3])
    end
    resolver = metadata_double(dnssec: true)
    validator = Dnsruby::ValidatorThread.new(:client, Queue.new, response, nil, query, selector, resolver)
    Dnsruby::Dnssec.stub(:validate_with_query, ->(*) { raise @limit }) do
      Dnsruby::PacketSender.stub(:cache, ->(*args) { positive_cache_calls << args }) do
        worker = Thread.new { validator.do_validate }
        assert(worker.join(2), 'Validator completion did not finish')
      end
    end
    assert_equal(1, completions.length)
    assert_same(@limit, completions.first[3])
    assert_same(@limit, response.security_error)
    assert_equal(Dnsruby::Message::SecurityLevel.BOGUS, response.security_level)
    assert_empty(positive_cache_calls)
    assert_nil(cache.find(query, :test_scope))
    assert_equal(1, registrations.length)
    assert_equal(registrations, removals)
    assert_nil(worker[:dnsruby_validation_budget])
    assert_nil(worker[:dnsruby_query_control])
  ensure
    worker&.kill if worker&.alive?
    worker&.join
  end

  private

  def exhausted_resolver
    queries, error = @queries, @limit
    Object.new.tap do |resolver|
      resolver.define_singleton_method(:dnssec=) { |_| }
      resolver.define_singleton_method(:query_no_validation_or_recursion) do |name, type|
        queries << [Dnsruby::Name.create(name).tap { |owner| owner.absolute = true }, type]
        raise error
      end
    end
  end

  def positive_response
    Dnsruby::Message.new(@child, 'A').tap do |message|
      message.header.qr = true
      message.add_answer(Dnsruby::RR.create('child.example. 60 IN A 192.0.2.1'))
    end
  end

  def assert_uncached_limit(error)
    query = Dnsruby::Message.new(@child, 'A')
    [nil, positive_response].each do |response|
      cache = Dnsruby::Cache::ResolutionCache.new
      cache.add(query, :test_scope, response, error)
      assert_nil(cache.find(query, :test_scope))
    end
    assert_nil(Dnsruby::ValidationBudget.current)
    assert_nil(Dnsruby::QueryControl.current)
  end

  def metadata_double(**attributes)
    Object.new.tap do |object|
      attributes.each { |name, value| object.define_singleton_method(name) { value } }
    end
  end
end
