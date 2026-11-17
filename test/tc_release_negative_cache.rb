require_relative 'spec_helper'
require 'minitest/mock'

class ReleaseNegativeCacheTest < Minitest::Test
  def setup
    @now = Time.utc(2026, 11, 14, 12)
  end

  def test_negative_lifetime_is_the_lesser_of_soa_ttl_and_minimum
    [[:NXDOMAIN, 80, 15], [:NOERROR, 10, 80]].each do |rcode, ttl, minimum|
      cache = Dnsruby::Cache.new
      response = negative(rcode, ttl: ttl, minimum: minimum)
      lifetime = [ttl, minimum].min
      Time.stub(:now, @now) { cache.add(response) }
      Time.stub(:now, @now + lifetime - 1) do
        found = cache.find('missing.example.', 'A')
        assert_equal(1, found.authority.first.ttl)
        assert_equal(rcode.to_s, found.rcode.to_s)
      end
      Time.stub(:now, @now + lifetime) { assert_nil(cache.find('missing.example.', 'A')) }
    end
  end

  def test_plain_name_error_is_shared_across_types_but_nodata_is_not
    [:NXDOMAIN, :NOERROR].each do |rcode|
      cache = Dnsruby::Cache.new
      cache.add(negative(rcode))
      found = cache.find('missing.example.', 'AAAA')
      if rcode == :NXDOMAIN
        assert_equal(Dnsruby::Types::AAAA, found.question.first.qtype)
      else
        assert_nil(found)
      end
      assert_nil(cache.find('missing.example.', 'A', 'CH'))
    end
  end

  def test_alias_name_error_does_not_claim_the_alias_itself_is_absent
    cache = Dnsruby::Cache.new
    response = negative(:NXDOMAIN, name: 'alias.example.')
    response.add_answer(Dnsruby::RR.create('alias.example. 90 IN CNAME missing.example.'))
    cache.add(response)
    refute_nil(cache.find('alias.example.', 'A'))
    assert_nil(cache.find('alias.example.', 'CNAME'))
  end

  def test_negative_without_enclosing_soa_or_with_zero_lifetime_is_not_cached
    cache = Dnsruby::Cache.new
    response = Dnsruby::Message.new('missing.example.', 'A')
    response.header.rcode = Dnsruby::RCode::NXDOMAIN
    assert_nil(cache.add(response))
    response.add_authority(Dnsruby::RR.create('other. 30 IN SOA ns.other. hostmaster.other. 1 60 60 60 30'))
    assert_nil(cache.add(response))
    assert_nil(cache.add(negative(:NOERROR, minimum: 0)))
    assert_equal(0, cache.length)
  end

  def test_alias_nodata_never_enters_the_global_positive_cache
    Dnsruby::PacketSender.clear_caches
    query = Dnsruby::Message.new('alias.example.', 'A')
    query.header.rd = true
    response = negative(:NOERROR, name: 'alias.example.')
    response.add_answer(Dnsruby::RR.create('alias.example. 90 IN CNAME missing.example.'))
    Dnsruby::PacketSender.cache(query, response)
    assert_equal(0, Dnsruby::PacketSender.recursive_cache_length)
  ensure
    Dnsruby::PacketSender.clear_caches
  end

  def test_resolver_caches_completed_name_errors_and_retains_client_id
    with_resolver(lambda { |query| [negative(:NXDOMAIN, name: query.question.first.qname), Dnsruby::NXDomain.new] }) do |resolver, attempts|
      first = ask(resolver)
      second = ask(resolver)
      assert_equal(1, attempts.call)
      assert_instance_of(Dnsruby::NXDomain, first[2])
      assert_instance_of(Dnsruby::NXDomain, second[2])
      assert(second[1].cached)
      refute_equal(first[0], second[0])
      assert_equal(second[0], second[1].header.id)
    end
  end

  def test_resolver_configuration_and_validation_policy_scope_negative_answers
    original_policy = Dnsruby::Dnssec.validation_policy
    with_resolver(lambda { |_query| [negative(:NOERROR), nil] }) do |resolver, attempts|
      ask(resolver)
      resolver.port = 1053
      ask(resolver)
      assert_equal(2, attempts.call)
      Dnsruby::Dnssec.validation_policy = original_policy == 1 ? 2 : 1
      ask(resolver)
      assert_equal(3, attempts.call)
    end
  ensure
    Dnsruby::Dnssec.validation_policy = original_policy
  end

  def test_opt_identity_is_binary_and_includes_payload_flags_and_options
    with_resolver(lambda { |_query| [negative(:NOERROR), nil] }) do |resolver, attempts|
      2.times do |index|
        query = Dnsruby::Message.new('missing.example.', 'A')
        query.add_additional(Dnsruby::RR::OPT.new(1232, 0,
          [Dnsruby::RR::OPT::Option.new(65001, "value#{index}")]))
        ask(resolver, query)
      end
      assert_equal(2, attempts.call)
    end
  end

  def test_raw_disabled_caching_tsig_and_update_queries_bypass_completion_cache
    with_resolver(lambda { |_query| [negative(:NOERROR), nil] }) do |resolver, attempts|
      ask(resolver)
      query = Dnsruby::Message.new('missing.example.', 'A')
      query.do_caching = false
      ask(resolver, query)
      raw = Dnsruby::Message.new('missing.example.', 'A')
      raw.send_raw = true
      ask(resolver, raw)
      assert_equal(3, attempts.call)
      engine = resolver.instance_variable_get(:@resolver_ruby)
      assert_nil(engine.send(:response_cache_scope, Dnsruby::Update.new('example.')))
      resolver.tsig = ['key.example.', 'c2VjcmV0']
      assert_nil(engine.send(:response_cache_scope, Dnsruby::Message.new('missing.example.', 'A')))
    end
  end

  def test_failed_resolution_is_cached_for_one_second_then_retried
    clock = 100.0
    Dnsruby::TransportClock.stub(:now, -> { clock }) do
      with_resolver(lambda { |_query| [nil, Dnsruby::ServFail.new('temporarily unavailable')] }) do |resolver, attempts|
        assert_instance_of(Dnsruby::ServFail, ask(resolver)[2])
        assert_instance_of(Dnsruby::ServFail, ask(resolver)[2])
        assert_equal(1, attempts.call)
        clock += 1
        ask(resolver)
        assert_equal(2, attempts.call)
      end
    end
  end

  def test_controlled_ordinary_failures_are_cached_but_cancellation_is_not
    cache = Dnsruby::Cache::ResolutionCache.new
    query = Dnsruby::Message.new('missing.example.', 'A')
    control = Dnsruby::QueryControl.new
    cache.add(query, [:scope], nil, Dnsruby::ServFail.new, control: control)
    assert_instance_of(Dnsruby::ServFail, cache.find(query, [:scope]).last)
    control.cancel
    cache.add(query, [:cancelled], nil, Dnsruby::ResolvTimeout.new, control: control)
    assert_nil(cache.find(query, [:cancelled]))
    cache.add(query, [:budget], nil, Dnsruby::ValidationLimitError.new)
    assert_instance_of(Dnsruby::ValidationLimitError, cache.find(query, [:budget]).last)
    cache.add(query, [:cancelled_validation], nil, Dnsruby::ValidationCancelled.new)
    assert_nil(cache.find(query, [:cancelled_validation]))
    cache.add(query, [:inherited_budget], nil, Dnsruby::ValidationLimitError.new,
      control: Dnsruby::QueryControl.new)
    assert_nil(cache.find(query, [:inherited_budget]))
  end

  def test_cached_validation_failure_preserves_security_state_and_ages_rr_ttl
    cache = Dnsruby::Cache::ResolutionCache.new
    query = Dnsruby::Message.new('missing.example.', 'A')
    response = Dnsruby::Message.new('missing.example.', 'A')
    response.add_answer(Dnsruby::RR.create('missing.example. 90 IN A 192.0.2.1'))
    response.security_level = Dnsruby::Message::SecurityLevel::BOGUS
    response.security_error = 'verification failed'
    Time.stub(:now, @now) { cache.add(query, [:scope], response, Dnsruby::VerifyError.new('verification failed')) }
    Time.stub(:now, @now + 1) do
      cached, error = cache.find(query, [:scope])
      assert_equal(89, cached.answer.first.ttl)
      assert_equal(Dnsruby::Message::SecurityLevel::BOGUS, cached.security_level)
      assert_equal('verification failed', cached.security_error)
      assert_instance_of(Dnsruby::VerifyError, error)
    end
  end

  def test_insecure_negative_requires_and_preserves_authenticated_proof_lifetime
    cache = Dnsruby::Cache::ResolutionCache.new
    query = Dnsruby::Message.new('missing.example.', 'A')
    query.do_validation = true
    response = negative(:NOERROR)
    response.security_level = Dnsruby::Message::SecurityLevel::INSECURE
    Time.stub(:now, @now) do
      cache.add(query, [:scope], response, nil)
      assert_nil(cache.find(query, [:scope]))
      response.instance_variable_set(:@dnsruby_authenticated_expiration, @now.to_i + 3)
      cache.add(query, [:scope], response, nil)
    end
    Time.stub(:now, @now + 2) do
      found, error = cache.find(query, [:scope])
      assert_nil(error)
      assert_equal(1, found.authority.first.ttl)
      assert_equal(@now.to_i + 3, found.instance_variable_get(:@dnsruby_authenticated_expiration))
    end
    Time.stub(:now, @now + 3) { assert_nil(cache.find(query, [:scope])) }
  end

  def test_configuration_change_during_resolution_prevents_insertion
    selected = nil
    producer = lambda do |_query|
      selected.query_timeout = 7
      [negative(:NOERROR), nil]
    end
    with_resolver(producer) do |resolver, attempts|
      selected = resolver
      ask(resolver)
      ask(resolver)
      assert_equal(2, attempts.call)
      ask(resolver)
      assert_equal(2, attempts.call)
    end
  end

  def test_explicit_clear_invalidates_resolver_completion_cache
    with_resolver(lambda { |_query| [negative(:NOERROR), nil] }) do |resolver, attempts|
      ask(resolver)
      Dnsruby::PacketSender.clear_caches
      ask(resolver)
      assert_equal(2, attempts.call)
    end
  end

  private

  def negative(rcode, ttl: 80, minimum: 15, name: 'missing.example.')
    response = Dnsruby::Message.new(name, 'A')
    response.header.rcode = Dnsruby::RCode.new(rcode.to_s)
    response.add_authority(Dnsruby::RR.create("example. #{ttl} IN SOA ns.example. hostmaster.example. 1 60 60 60 #{minimum}"))
    response
  end

  def ask(resolver, message = Dnsruby::Message.new('missing.example.', 'A'))
    queue = Queue.new
    @id = (@id || 100) + 1
    message.header.id = @id
    resolver.send_async(message, queue, @id)
    queue.pop
  end

  def with_resolver(producer)
    resolver = Dnsruby::Resolver.new(nameserver: ['192.0.2.53'])
    engine = Dnsruby::ResolverRuby.new(resolver)
    resolver.instance_variable_set(:@resolver_ruby, engine)
    selector = Object.new
    selector.define_singleton_method(:add_observer) { |*_args| }
    selector.define_singleton_method(:remove_observer) { |*_args| }
    count = 0
    engine.define_singleton_method(:tick) do |id = nil|
      next if id.nil?
      resolver.single_res_mutex.synchronize do
        query, client_queue, select_queue = @query_list.fetch(id)
        count += 1
        response, error = producer.call(query)
        send_result(client_queue, id, select_queue, response, error)
      end
    end
    Dnsruby::SelectThread.stub(:instance, selector) do
      yield resolver, -> { count }
    end
  ensure
    resolver&.close
  end
end

class ReleaseCacheCompletionFollowupsTest < Minitest::Test
  def test_dname_only_negative_uses_target_soa_and_question_specific_cache
    query = Dnsruby::Message.new('www.old.example.', 'AAAA')
    message = Dnsruby::Message.new('www.old.example.', 'AAAA')
    message.add_answer(Dnsruby::RR.create('old.example. 60 IN DNAME new.example.'))
    soa = Dnsruby::RR.create('new.example. 60 IN SOA ns.new.example. hostmaster.new.example. 1 3600 600 86400 30')
    message.add_authority(soa)
    assert_same(soa, Dnsruby::Cache.negative_soa(message))
    cache = Dnsruby::Cache::ResolutionCache.new
    cache.add(query, [:scope], message, nil)
    refute_nil(cache.find(query, [:scope]))
    assert_nil(cache.find(Dnsruby::Message.new('www.old.example.', 'DNAME'), [:scope]))
    message.add_answer(Dnsruby::RR.create('www.new.example. 60 IN AAAA 2001:db8::1'))
    assert_nil(Dnsruby::Cache.negative_soa(message))
  end

  def test_latest_success_replaces_failure_and_negative_completions
    query = Dnsruby::Message.new('host.example.', 'A')
    positive = Dnsruby::Message.new('host.example.', 'A')
    positive.add_answer(Dnsruby::RR.create('host.example. 60 IN A 192.0.2.1'))
    negative = Dnsruby::Message.new('host.example.', 'A')
    negative.header.rcode = Dnsruby::RCode.NXDOMAIN
    negative.add_authority(Dnsruby::RR.create('example. 60 IN SOA ns.example. hostmaster.example. 1 3600 600 86400 30'))
    cache = Dnsruby::Cache::ResolutionCache.new
    cache.add(query, [:scope], nil, Dnsruby::ServFail.new('temporary failure'))
    refute_nil(cache.find(query, [:scope]))
    cache.add(query, [:scope], positive, nil)
    assert_nil(cache.find(query, [:scope]))
    cache.add(query, [:scope], negative, Dnsruby::NXDomain.new('missing'))
    refute_nil(cache.find(query, [:scope]))
    cache.add(query, [:scope], positive, nil)
    assert_nil(cache.find(query, [:scope]))
    cache.add(query, [:scope], negative, Dnsruby::NXDomain.new('missing'))
    cache.add(query, [:scope], nil, Dnsruby::ServFail.new('later failure'))
    _message, error = cache.find(query, [:scope])
    assert_instance_of(Dnsruby::ServFail, error)
  end

  def test_validation_scope_includes_discovery_route_and_resolver_settings_without_lock_reentry
    previous_resolver = Dnsruby::Dnssec.default_resolver
    previous_mode = Dnsruby::Dnssec.do_validation_with_recursor?
    resolver = Dnsruby::Resolver.new(nameserver: ['127.0.0.1'])
    Dnsruby::Dnssec.default_resolver = resolver
    before = Dnsruby::Cache.validation_scope
    resolver.single_res_mutex.synchronize { assert_equal(before, Dnsruby::Cache.validation_scope) }
    {port: 5353, use_tcp: true, query_timeout: 7, packet_timeout: 2,
      retry_times: 3, retry_delay: 6, nameserver: ['127.0.0.2']}.each do |name, value|
      before = Dnsruby::Cache.validation_scope
      resolver.public_send("#{name}=", value)
      refute_equal(before, Dnsruby::Cache.validation_scope, name.to_s)
    end
    before = Dnsruby::Cache.validation_scope
    Dnsruby::Dnssec.do_validation_with_recursor(!previous_mode)
    refute_equal(before, Dnsruby::Cache.validation_scope)
    before = Dnsruby::Cache.validation_scope
    Dnsruby::Dnssec.default_resolver = Dnsruby::Resolver.new(nameserver: ['127.0.0.2'])
    refute_equal(before, Dnsruby::Cache.validation_scope)
  ensure
    Dnsruby::Dnssec.default_resolver&.close if Dnsruby::Dnssec.default_resolver != previous_resolver
    resolver&.close
    Dnsruby::Dnssec.default_resolver = previous_resolver
    Dnsruby::Dnssec.do_validation_with_recursor(previous_mode)
  end
end
