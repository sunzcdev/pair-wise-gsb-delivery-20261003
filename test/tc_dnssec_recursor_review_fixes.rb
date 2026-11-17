require_relative 'spec_helper'
require 'minitest/mock'
require 'openssl'
require 'base64'

# Ordinary records and local doubles keep these contracts independent of DNS.
class DnssecRecursorReviewFixesTest < Minitest::Test
  def test_child_budgets_share_work_and_transaction_but_cancel_independently
    root = Dnsruby::ValidationBudget.new(limits: limits(max_work: 2))
    first, second = root.child, root.child
    assert_same(root.transaction, first.transaction)
    assert_same(root.transaction, second.transaction)
    first.check!(1)
    first.cancel
    assert_raises(Dnsruby::ValidationCancelled) { first.check! }
    assert(root.check!)
    second.check!(1)
    assert_raises(Dnsruby::ValidationLimitError) { second.check!(1) }
    assert_raises(Dnsruby::ValidationLimitError) { root.check! }
  end

  def test_parallel_children_share_signature_allowance
    root = Dnsruby::ValidationBudget.new(limits: limits(max_signature_verifications: 2))
    threads = 2.times.map { Thread.new { root.child.signature! } }
    assert(threads.all? { |thread| thread.join(2) })
    threads.each(&:value)
    assert_raises(Dnsruby::ValidationLimitError) { root.signature! }
  ensure
    threads&.each { |thread| thread.kill if thread.alive?; thread.join }
  end

  def test_parent_cancellation_reaches_children
    root = Dnsruby::ValidationBudget.new
    child = root.child
    root.cancel
    assert_raises(Dnsruby::ValidationCancelled) { child.check! }
  end

  def test_validator_hops_use_shared_budget_without_cancelling_the_lookup
    root = Dnsruby::ValidationBudget.new(limits: limits(max_signature_verifications: 2))
    transmissions = Dnsruby::SelectThread::TransmissionBudget.new
    transmissions.context(:validation) { root }
    controls = []
    selector = Object.new
    selector.define_singleton_method(:register_validation) { |_, _, control| controls << control; true }
    selector.define_singleton_method(:unregister_validation) { |_, _, control| control.cancel }
    selector.define_singleton_method(:push_validation_response_to_select) { |*_| }
    query = Dnsruby::Message.new('www.example.')
    query.do_validation = true
    resolver = Struct.new(:dnssec).new(true)
    validation = lambda do |_, response|
      budget = Dnsruby::ValidationBudget.current
      assert_same(root.transaction, budget.transaction)
      refute_same(root.control, budget.control)
      budget.signature!
      response.security_level = Dnsruby::Message::SecurityLevel.SECURE
      true
    end
    Dnsruby::Dnssec.stub(:validate_with_query, validation) do
      Dnsruby::PacketSender.stub(:cache, nil) do
        2.times do
          response = answer('www.example.', 'www.example. 60 IN A 192.0.2.1')
          Dnsruby::ValidatorThread.new(:client, Queue.new, response, nil, query,
            selector, resolver, transmissions).do_validate
          assert_equal(Dnsruby::Message::SecurityLevel.SECURE, response.security_level)
          assert(root.check!)
        end
      end
    end
    refute_same(*controls)
    assert_raises(Dnsruby::ValidationLimitError) { root.signature! }
  end

  def test_alias_query_keeps_original_question_and_one_budget_through_final_validation
    packets = {
      'www.example.' => answer('www.example.', 'www.example. 60 IN CNAME target.other.'),
      'target.other.' => answer('target.other.', 'target.other. 60 IN A 192.0.2.1')
    }
    with_recursor(packets) do |recursor, calls, budgets|
      validation = lambda do |response|
        assert_same(budgets.first, Dnsruby::ValidationBudget.current)
        assert_equal('www.example.', response.question.first.qname.to_s(true))
        true
      end
      Dnsruby::Dnssec.stub(:validate, validation) do
        response = recursor.query('www.example')
        assert_equal(%w[www.example. target.other.], calls)
        assert_equal([Dnsruby::Types.CNAME, Dnsruby::Types.A], response.answer.map(&:type))
        assert_equal(1, budgets.map(&:object_id).uniq.length)
        refute(response.header.aa)
      end
    end
    assert_nil(Dnsruby::ValidationBudget.current)
    assert_nil(Dnsruby::QueryControl.current)
  end

  def test_complete_alias_answer_uses_one_hop
    response = answer('www.example.', 'www.example. 60 IN CNAME target.example.',
      'target.example. 60 IN A 192.0.2.1')
    with_recursor('www.example.' => response) do |recursor, calls|
      assert_same(response, recursor.query('www.example.', 'A', 'IN', true))
      assert_equal(['www.example.'], calls)
    end
  end

  def test_combining_alias_answers_preserves_the_earliest_authenticated_lifetime
    first = answer('www.example.', 'www.example. 60 IN CNAME target.other.')
    last = answer('target.other.', 'target.other. 60 IN A 192.0.2.1')
    first.instance_variable_set(:@dnsruby_authenticated_expiration, 100)
    last.instance_variable_set(:@dnsruby_authenticated_expiration, 200)
    with_recursor('www.example.' => first, 'target.other.' => last) do |recursor|
      response = recursor.query('www.example.', 'A', 'IN', true)
      assert_equal(100, response.instance_variable_get(:@dnsruby_authenticated_expiration))
    end
  end

  def test_independent_queries_get_independent_validation_budgets
    with_recursor('www.example.' => answer('www.example.', 'www.example. 60 IN A 192.0.2.1')) do |recursor, _, budgets|
      Dnsruby::Dnssec.stub(:validate, true) do
        2.times { assert(recursor.query('www.example.')) }
      end
      refute_same(*budgets)
      refute_same(budgets.first.transaction, budgets.last.transaction)
    end
  end

  def test_dname_chase_preserves_synthesized_cname_and_original_question
    packets = {
      'host.branch.example.' => answer('host.branch.example.', 'branch.example. 60 IN DNAME other.'),
      'host.other.' => answer('host.other.', 'host.other. 60 IN A 192.0.2.2')
    }
    with_recursor(packets) do |recursor, calls|
      response = recursor.query('host.branch.example.', 'A', 'IN', true)
      assert_equal(%w[host.branch.example. host.other.], calls)
      assert_equal('host.branch.example.', response.question.first.qname.to_s(true))
      cname = response.answer.find { |rr| rr.type == Dnsruby::Types.CNAME }
      assert_equal('host.other.', cname.domainname.to_s(true))
      assert_equal(60, cname.ttl)
    end
  end

  def test_explicit_alias_queries_do_not_chase
    ['CNAME', 'DNAME'].each do |type|
      packet = answer('alias.example.', "alias.example. 60 IN #{type} target.other.")
      with_recursor('alias.example.' => packet) do |recursor, calls|
        assert_same(packet, recursor.query('alias.example.', type, 'IN', true))
        assert_equal(['alias.example.'], calls)
      end
    end
  end

  def test_nonvalidating_lookup_does_not_create_a_dnssec_budget
    with_recursor('www.example.' => answer('www.example.', 'www.example. 60 IN A 192.0.2.1')) do |recursor, _, budgets|
      Dnsruby::ValidationBudget.stub(:new, ->(*) { flunk('Nonvalidating recursion created a DNSSEC budget') }) do
        assert(recursor.query('www.example.', 'A', 'IN', true))
      end
      assert_equal([nil], budgets)
    end
  end

  def test_alias_steps_use_the_recursion_budget
    packets = {
      'www.example.' => answer('www.example.', 'www.example. 60 IN CNAME target.other.'),
      'target.other.' => answer('target.other.', 'target.other. 60 IN CNAME final.other.')
    }
    with_recursor(packets) do |recursor, calls|
      recursor.recursion_limits = { max_steps: 1 }
      assert_raises(Dnsruby::RecursionLimitError) { recursor.query('www.example.', 'A', 'IN', true) }
      assert_equal(%w[www.example. target.other.], calls)
    end
  end

  def test_zero_ttl_addresses_are_local_and_snapshots_are_independent
    cache = Dnsruby::Recursor::AddressCache.new
    cache.push(['192.0.2.1', 0])
    cache.push(['192.0.2.2', 60])
    assert_equal(%w[192.0.2.2 192.0.2.1], cache.current_values)
    assert_equal(['192.0.2.2'], cache.values)
    snapshot = cache.snapshot
    assert_equal(['192.0.2.2'], snapshot.current_values)
    cache.push(['192.0.2.3', 60])
    assert_equal(['192.0.2.2'], snapshot.values)
  end

  def test_zero_ttl_referral_glue_is_used_without_entering_shared_caches
    with_private_caches do
      recursor = bare_recursor
      referral = Dnsruby::Message.new('www.child.example.')
      referral.add_authority(Dnsruby::RR.create('child.example. 60 IN NS ns.child.example.'))
      referral.add_additional(Dnsruby::RR.create('ns.child.example. 0 IN A 192.0.2.1'))
      zone, authorities, ttl = recursor.referral_authorities(referral, 'example.', 'www.child.example.', 'IN')
      Dnsruby::Recursor.send(:cache_zone, zone, authorities, ttl)
      result = answer('www.child.example.', 'www.child.example. 60 IN A 192.0.2.2')
      sends = []
      factory = lambda do |settings|
        sends << settings[:nameserver]
        fake_resolver { |_| result }
      end
      Dnsruby::Resolver.stub(:new, factory) do
        assert_same(result, recursor.send(:_dorecursion, 'www.child.example.', 'A', 'IN', zone, authorities, 0, true))
      end
      assert_equal([['192.0.2.1']], sends)
      assert_empty(Dnsruby::Recursor.class_variable_get(:@@authority_cache).fetch('ns.child.example.').current_values)
      shared = Dnsruby::Recursor.class_variable_get(:@@zones_cache).fetch(zone)
      assert_empty(shared.fetch('ns.child.example.').current_values)
    end
  end

  def test_zero_ttl_discovered_nameserver_address_is_used_once
    with_private_caches do
      hints = Dnsruby::Recursor::AddressCache.new
      hints.push(['192.0.2.53', 60])
      Dnsruby::Recursor.class_variable_set(:@@hints, { 'root.example.' => hints })
      recursor = bare_recursor
      sent = []
      factory = lambda do |settings|
        fake_resolver do |query|
          name = query.question.first.qname.to_s(true)
          sent << [name, settings[:nameserver]]
          name == 'ns.other.' ? answer(name, 'ns.other. 0 IN A 192.0.2.1') :
            answer(name, 'www.child.example. 60 IN A 192.0.2.2')
        end
      end
      authorities = { 'ns.other.' => Dnsruby::Recursor::AddressCache.new }
      Dnsruby::Resolver.stub(:new, factory) do
        assert(recursor.send(:_dorecursion, 'www.child.example.', 'A', 'IN', 'child.example.', authorities, 0, true))
      end
      assert_equal([['ns.other.', ['192.0.2.53']], ['www.child.example.', ['192.0.2.1']]], sent)
      assert_empty(Dnsruby::Recursor.class_variable_get(:@@authority_cache).fetch('ns.other.').current_values)
    end
  end

  def test_signature_serial_time_across_the_epoch_boundary_and_cache_expiry
    epoch = 1 << 32
    now = epoch + 20
    private_key = OpenSSL::PKey::EC.generate('prime256v1')
    public_key = private_key.public_key.to_octet_string(:uncompressed).byteslice(1, 64)
    key = Dnsruby::RR.create("example. 300 IN DNSKEY 257 3 13 #{Base64.strict_encode64(public_key)}")
    signature = Dnsruby::RR.create(name: key.name, type: 'RRSIG', ttl: 300,
      type_covered: 'DNSKEY', algorithm: 13, labels: 1, original_ttl: 300,
      inception: epoch - 60, expiration: 80, key_tag: key.key_tag, signers_name: key.name)
    wire = Dnsruby::MessageEncoder.new { |encoder| encoder.put_rr(key, true) }.to_s
    der = private_key.dsa_sign_asn1(OpenSSL::Digest::SHA256.digest(signature.sig_data + wire))
    signature.signature = OpenSSL::ASN1.decode(der).value.map { |n| n.value.to_s(2).rjust(32, "\0") }.join
    records = Dnsruby::RRSet.new([key, signature])
    verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
    cache = Dnsruby::KeyCache.new
    Time.stub(:now, Time.at(now)) do
      assert(verifier.verify_rrset(records, key))
      assert_equal(60, records.ttl)
      cache.add_rrset(records)
      assert_equal(epoch + 80, cache.keys_and_expirations.first.first)
    end
    Time.stub(:now, Time.at(epoch + 79)) do
      assert_equal([Dnsruby::KeyCache.key_identity(key)], cache.keys.map { |item| Dnsruby::KeyCache.key_identity(item) })
    end
    Time.stub(:now, Time.at(epoch + 80)) { assert_empty(cache.keys) }
    refute(Dnsruby::SerialTime.valid?(epoch - 60, 80, epoch + 81))
    refute(Dnsruby::SerialTime.valid?(epoch - 60, 80, epoch - 61))
    assert_nil(Dnsruby::SerialTime.absolute(now + (1 << 31), now))
  end

  def test_provisional_insecure_and_missing_cached_keys_reach_full_policy
    [:insecure, Dnsruby::VerifyError.new('Signing key not found')].each do |cached|
      with_full_validation(cached) do |query, response, calls|
        assert(Dnsruby::Dnssec.validate_with_query(query, response))
        assert_equal([:full], calls)
        assert_equal(Dnsruby::Message::SecurityLevel.SECURE, response.security_level)
      end
    end
  end

  def test_full_policy_bogus_is_preserved_after_provisional_insecure
    with_full_validation(:insecure, bogus: true) do |query, response, calls|
      assert_raises(Dnsruby::VerifyError) { Dnsruby::Dnssec.validate_with_query(query, response) }
      assert_equal([:full], calls)
      assert_equal(Dnsruby::Message::SecurityLevel.BOGUS, response.security_level)
    end
  end

  private

  def limits(**changes)
    Dnsruby::ValidationBudget::DEFAULT_LIMITS.merge(changes)
  end

  def answer(name, *records)
    Dnsruby::Message.new(name).tap do |packet|
      packet.header.qr = packet.header.aa = true
      records.each { |record| packet.add_answer(Dnsruby::RR.create(record)) }
    end
  end

  def bare_recursor
    Dnsruby::Recursor.allocate.tap do |recursor|
      recursor.instance_variable_set(:@resolver, Struct.new(:query_timeout, :recurse).new(10, false))
      recursor.instance_variable_set(:@ipv6_ok, false)
      recursor.instance_variable_set(:@dnssec, false)
    end
  end

  def with_recursor(packets)
    recursor, calls, budgets = bare_recursor, [], []
    recursor.define_singleton_method(:recursion_cache_scope) { |_| nil }
    recursor.define_singleton_method(:get_closest_known_zone_authorities_for) { |_| ['.', {}] }
    recursor.define_singleton_method(:_dorecursion) do |name, *_|
      calls << name
      budgets << Dnsruby::ValidationBudget.current
      packets.fetch(name)
    end
    Dnsruby::Recursor.stub(:ensure_hints, 0) { yield recursor, calls, budgets }
  end

  def fake_resolver(&send_message)
    Struct.new(:dnssec, :recurse).new.tap do |resolver|
      resolver.define_singleton_method(:send_message) { |query| send_message.call(query) }
      resolver.define_singleton_method(:close) {}
    end
  end

  def with_private_caches
    fields = [:@@authority_cache, :@@zones_cache, :@@zone_expirations, :@@hints]
    saved = fields.to_h { |field| [field, Dnsruby::Recursor.class_variable_get(field)] }
    fields.each { |field| Dnsruby::Recursor.class_variable_set(field, {}) }
    yield
  ensure
    saved.each { |field, value| Dnsruby::Recursor.class_variable_set(field, value) }
  end

  def with_full_validation(cached, bogus: false)
    previous = Dnsruby::Dnssec.validation_policy
    Dnsruby::Dnssec.validation_policy = Dnsruby::Dnssec::ValidationPolicy::ALWAYS_LOCAL_ANCHORS_ONLY
    query = Dnsruby::Message.new('www.example.')
    query.header.cd = true
    response = answer('www.example.', 'www.example. 60 IN A 192.0.2.1')
    response.add_answer(Dnsruby::RR.create(name: 'www.example.', type: 'RRSIG'))
    calls = []
    fast = ->(*) { cached.is_a?(Exception) ? raise(cached) : cached }
    full = lambda do |message, _|
      calls << :full
      raise Dnsruby::VerifyError, 'Full validation failed' if bogus
      message.security_level = Dnsruby::Message::SecurityLevel.SECURE
      true
    end
    Dnsruby::Dnssec.stub(:verify_result, fast) do
      Dnsruby::Dnssec.stub(:validate_with_anchors, full) { yield query, response, calls }
    end
  ensure
    Dnsruby::Dnssec.validation_policy = previous
  end
end
