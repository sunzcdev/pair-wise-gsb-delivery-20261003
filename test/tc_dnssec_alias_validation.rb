require_relative 'tc_key_cache_identity'

class DnssecAliasValidationTest < Minitest::Test
  def setup
    @now = Time.now.to_i
    @private_keys = KeyCacheIdentityTest::PRIVATE_KEYS.map { |pem| OpenSSL::PKey::EC.new(pem) }
    @keys = %w[. left. right.].each_with_index.to_h do |zone, index|
      private_key = @private_keys[index % @private_keys.length]
      bytes = private_key.public_key.to_octet_string(:uncompressed).byteslice(1, 64)
      [zone, [Dnsruby::RR.create("#{zone} 300 IN DNSKEY 257 3 13 #{Base64.strict_encode64(bytes)}"), private_key]]
    end
    @verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
    @verifier.add_trust_anchor(@keys.fetch('.').first)
  end

  def test_cold_cross_zone_cname_discovers_target_signer_with_the_same_budget
    message = cross_zone_message
    budget = Dnsruby::ValidationBudget.new
    with_chain_resolver do |queries, budgets|
      Dnsruby::ValidationBudget.with(budget) do
        assert(@verifier.validate(message, Dnsruby::Message.new('alias.left.', 'A')))
      end
      assert_equal(Dnsruby::Message::SecurityLevel.SECURE, message.security_level)
      assert_equal(1, queries.count(['right.', 'DS']))
      assert_equal(1, queries.count(['right.', 'DNSKEY']))
      refute_includes(queries, ['target.right.', 'DS'])
      assert(budgets.all? { |used| used.equal?(budget) })
    end
  end

  def test_cross_zone_negative_answer_discovers_authority_signer
    message = message_with('alias.left.', 'A', [signed('alias.left. 300 IN CNAME target.right.', 'left.')])
    signed('right. 300 IN SOA ns.right. hostmaster.right. 1 3600 600 86400 300', 'right.').
      each { |rr| message.add_authority(rr) }
    signed('target.right. 300 IN NSEC z.right. TXT NSEC RRSIG', 'right.').
      each { |rr| message.add_authority(rr) }
    with_chain_resolver do |queries, _budgets|
      assert(@verifier.validate(message, Dnsruby::Message.new('alias.left.', 'A')))
      assert_equal(Dnsruby::Message::SecurityLevel.SECURE, message.security_level)
      assert_equal(1, queries.count(['right.', 'DNSKEY']))
    end
  end

  def test_cached_target_key_avoids_repeating_signer_discovery
    @verifier.add_trust_anchor(@keys.fetch('right.').first)
    message = cross_zone_message
    with_chain_resolver do |queries, _budgets|
      assert(@verifier.validate(message, nil))
      assert_equal(Dnsruby::Message::SecurityLevel.SECURE, message.security_level)
      assert_empty(queries.select { |name, _type| name == 'right.' })
    end
  end

  def test_overlapping_rollover_refreshes_a_cached_key_with_the_same_tag
    new_key = @keys.fetch('right.').first
    old_private = @private_keys.fetch(1)
    old_bytes = old_private.public_key.to_octet_string(:uncompressed).byteslice(1, 64)
    old_key = Dnsruby::RR.create("right. 300 IN DNSKEY 257 3 13 #{Base64.strict_encode64(old_bytes)}")
    refute_equal(old_key.key, new_key.key)
    assert_equal(old_key.key_tag, new_key.key_tag)
    @verifier.add_trust_anchor(old_key)

    rollover = Dnsruby::RRSet.new([old_key, new_key])
    signature = Dnsruby::RR.create(name: 'right.', type: 'RRSIG', ttl: 300,
      type_covered: Dnsruby::Types.DNSKEY, algorithm: 13, labels: 1,
      original_ttl: 300, expiration: @now + 3600, inception: @now - 60,
      key_tag: old_key.key_tag, signers_name: 'right.')
    bytes = @verifier.send(:canonical_rrset_data, rollover, signature, {})
    der = old_private.dsa_sign_asn1(OpenSSL::Digest::SHA256.digest(signature.sig_data + bytes))
    signature.signature = OpenSSL::ASN1.decode(der).value.map { |n| n.value.to_s(2).rjust(32, "\0") }.join
    rollover.add(signature)
    # Authenticate the ordinary two-key rollover independently, without
    # teaching its new key to the verifier whose discovery is under test.
    check = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
    assert(check.verify(rollover, old_key))

    responses = {['right.', 'DNSKEY'] => message_with('right.', 'DNSKEY', [rollover])}
    message = cross_zone_message
    with_chain_resolver(responses: responses) do |queries, _budgets|
      assert(@verifier.validate(message, nil))
      assert_equal(Dnsruby::Message::SecurityLevel.SECURE, message.security_level)
      assert_equal(1, queries.count(['right.', 'DNSKEY']))
    end
  end

  def test_signer_discovery_does_not_reset_the_shared_work_limit
    budget = Dnsruby::ValidationBudget.new
    before_response = lambda do |name, type|
      if name.to_s(true) == 'right.' && type == Dnsruby::Types.DS
        assert_same(budget, Dnsruby::ValidationBudget.current)
        budget.check!(Dnsruby::Dnssec.validation_limits[:max_work] + 1)
      end
    end
    with_chain_resolver(before_response) do |_queries, _budgets|
      assert_raises(Dnsruby::ValidationLimitError) do
        Dnsruby::ValidationBudget.with(budget) { @verifier.validate(cross_zone_message, nil) }
      end
    end
    assert_nil(Dnsruby::ValidationBudget.current)
  end

  def test_additional_data_does_not_trigger_signer_discovery
    message = cross_zone_message
    signed('extra.right. 300 IN A 192.0.2.2', 'right.').each { |rr| message.add_additional(rr) }
    requested = []
    @verifier.stub(:find_closest_anchor_for, ->(name) { requested << name.to_s(true); false }) do
      @verifier.send(:discover_signer_keys, message_with('left.', 'A', [], message.additional))
    end
    assert_empty(requested)
  end

  def test_optional_foreign_signed_data_is_removed_without_rejecting_the_answer
    @verifier.add_trust_anchor(@keys.fetch('left.').first)
    answer = signed('left. 300 IN MX 10 mail.right.', 'left.')
    extra = signed('mail.right. 300 IN A 192.0.2.2', 'right.')
    message = message_with('left.', 'MX', [answer], extra.rrs + extra.sigs)
    assert(@verifier.verify(message))
    assert_empty(message.additional)
    assert_equal(0, message.header.arcount)
    assert(message.answer.all? { |rr| rr.instance_variable_get(:@dnsruby_authenticated_expiration) })
    refute(@verifier.get_keys_to_check.any? { |key| key.name == @keys.fetch('right.').first.name })
  end

  def test_optional_foreign_dnskey_and_ds_learning_is_opportunistic
    @verifier.add_trust_anchor(@keys.fetch('left.').first)
    answer = signed('left. 300 IN MX 10 mail.right.', 'left.')
    key = @keys.fetch('right.').first
    [signed(key, 'right.'), signed(Dnsruby::RR::DS.from_key(key, 2), 'right.')].each do |extra|
      message = message_with('left.', 'MX', [answer], extra.rrs + extra.sigs)
      assert(@verifier.verify(message))
      assert_empty(message.additional)
      refute(@verifier.get_keys_to_check.any? { |known| known.name == key.name })
    end
  end

  def test_verified_optional_data_is_retained_and_unsigned_glue_stays_unauthenticated
    %w[left. right.].each { |zone| @verifier.add_trust_anchor(@keys.fetch(zone).first) }
    extra = signed('mail.right. 300 IN A 192.0.2.2', 'right.')
    glue = Dnsruby::RR.create('ns.right. 300 IN A 192.0.2.3')
    message = message_with('left.', 'MX', [signed('left. 300 IN MX 10 mail.right.', 'left.')],
      extra.rrs + extra.sigs + [glue])
    assert(@verifier.verify(message))
    assert_equal(3, message.additional.length)
    message.additional.each do |record|
      if record.name == glue.name
        assert_nil(record.instance_variable_get(:@dnsruby_authenticated_expiration))
      else
        refute_nil(record.instance_variable_get(:@dnsruby_authenticated_expiration))
      end
    end
  end

  def test_unsigned_additional_copy_does_not_inherit_answer_authentication
    @verifier.add_trust_anchor(@keys.fetch('left.').first)
    answer = signed('mail.left. 300 IN A 192.0.2.2', 'left.')
    extra = answer.rrs.first.clone
    message = message_with('mail.left.', 'A', [answer], [extra])
    assert(@verifier.verify(message))
    refute_nil(message.answer.first.instance_variable_get(:@dnsruby_authenticated_expiration))
    assert_nil(message.additional.first.instance_variable_get(:@dnsruby_authenticated_expiration))
  end

  def test_optional_verification_preserves_the_shared_signature_limit
    %w[left. right.].each { |zone| @verifier.add_trust_anchor(@keys.fetch(zone).first) }
    extra = signed('mail.right. 300 IN A 192.0.2.2', 'right.')
    message = message_with('left.', 'MX', [signed('left. 300 IN MX 10 mail.right.', 'left.')], extra.rrs + extra.sigs)
    limits = Dnsruby::Dnssec.validation_limits.merge(max_signature_verifications: 1)
    Dnsruby::Dnssec.stub(:validation_limits, limits) do
      assert_raises(Dnsruby::ValidationLimitError) { @verifier.verify(message) }
    end
    assert_nil(Dnsruby::ValidationBudget.current)
  end

  def test_dname_authenticates_synthesized_cname_in_either_record_order
    @verifier.add_trust_anchor(@keys.fetch('left.').first)
    [false, true].each do |cname_first|
      message = dname_message(cname_first: cname_first)
      assert(@verifier.verify(message))
      assert_equal('www.new.left', message.answer.find { |rr| rr.type == Dnsruby::Types.CNAME }.domainname.to_s)
    end
  end

  def test_dname_redirects_without_a_server_synthesized_cname
    @verifier.add_trust_anchor(@keys.fetch('left.').first)
    message = dname_message
    message.answer.delete_if { |rr| rr.type == Dnsruby::Types.CNAME }
    assert(@verifier.verify(message))
  end

  def test_synthesized_cname_inherits_dname_expiration_and_preserves_zero_ttl
    @verifier.add_trust_anchor(@keys.fetch('left.').first)
    Time.stub(:now, Time.at(@now)) do
      [:expiration, :signature_ttl].each do |bound|
        [0, 300].each do |ttl|
          message = dname_message(cname_ttl: ttl, dname_lifetime: bound == :expiration ? 7 : 3600)
          if bound == :signature_ttl
            message.answer.find { |rr| rr.type == Dnsruby::Types.RRSIG && rr.type_covered == Dnsruby::Types.DNAME }.ttl = 7
          end
          assert(@verifier.verify(message))
          cname = message.answer.find { |rr| rr.type == Dnsruby::Types.CNAME }
          assert_equal([ttl, 7].min, cname.ttl)
          assert_equal(@now + [ttl, 7].min, cname.instance_variable_get(:@dnsruby_authenticated_expiration))
        end
      end
    end
  end

  def test_synthesized_cname_uses_labels_and_case_insensitive_names
    @verifier.add_trust_anchor(@keys.fetch('left.').first)
    message = message_with('www\\.part.old.left.', 'A', [
      signed('old.left. 300 IN DNAME new.left.', 'left.'),
      Dnsruby::RRSet.new([Dnsruby::RR.create('www\\.part.OLD.LEFT. 300 IN CNAME www\\.part.NEW.LEFT.')]),
      signed('www\\.part.new.left. 300 IN A 192.0.2.1', 'left.')])
    assert(@verifier.verify(message))
  end

  def test_an_ordinary_unsigned_cname_still_requires_authentication
    @verifier.add_trust_anchor(@keys.fetch('left.').first)
    message = message_with('alias.left.', 'A', [
      Dnsruby::RRSet.new([Dnsruby::RR.create('alias.left. 300 IN CNAME target.left.')]),
      signed('target.left. 300 IN A 192.0.2.1', 'left.')])
    assert_raises(Dnsruby::VerifyError) { @verifier.verify(message) }
  end

  private

  def signed(record, zone, lifetime: 3600)
    record = Dnsruby::RR.create(record) if record.is_a?(String)
    key, private_key = @keys.fetch(zone)
    signature = Dnsruby::RR.create(name: record.name, type: 'RRSIG', ttl: record.ttl,
      type_covered: record.type, algorithm: 13, labels: record.name.labels.length,
      original_ttl: record.ttl, expiration: @now + lifetime, inception: @now - 60,
      key_tag: key.key_tag, signers_name: key.name)
    bytes = Dnsruby::MessageEncoder.new { |encoder| encoder.put_rr(record, true) }.to_s
    der = private_key.dsa_sign_asn1(OpenSSL::Digest::SHA256.digest(signature.sig_data + bytes))
    signature.signature = OpenSSL::ASN1.decode(der).value.map { |n| n.value.to_s(2).rjust(32, "\0") }.join
    Dnsruby::RRSet.new([record, signature])
  end

  def message_with(name, type, answer, additional = [])
    Dnsruby::Message.new(name, type).tap do |message|
      answer.each { |set| set.each { |rr| message.add_answer(rr) } }
      additional.each { |rr| message.add_additional(rr) }
    end
  end

  def cross_zone_message
    message_with('alias.left.', 'A', [signed('alias.left. 300 IN CNAME target.right.', 'left.'),
      signed('target.right. 300 IN A 192.0.2.1', 'right.')])
  end

  def dname_message(cname_first: false, cname_ttl: 300, dname_lifetime: 3600)
    dname = signed('old.left. 300 IN DNAME new.left.', 'left.', lifetime: dname_lifetime)
    cname = Dnsruby::RRSet.new([Dnsruby::RR.create("www.old.left. #{cname_ttl} IN CNAME www.new.left.")])
    answer = cname_first ? [cname, dname] : [dname, cname]
    answer << signed('www.new.left. 300 IN A 192.0.2.1', 'left.')
    message_with('www.old.left.', 'A', answer)
  end

  def with_chain_resolver(before_response = nil, responses: {})
    lookup = {}
    @keys.each do |zone, (key, _)|
      lookup[[zone, 'DNSKEY']] = message_with(zone, 'DNSKEY', [signed(key, zone)])
    end
    %w[left. right.].each do |zone|
      ds = Dnsruby::RR::DS.from_key(@keys.fetch(zone).first, 2)
      lookup[[zone, 'DS']] = message_with(zone, 'DS', [signed(ds, '.')])
    end
    lookup[['alias.left.', 'DS']] = message_with('alias.left.', 'DS', [
      signed('alias.left. 300 IN CNAME target.right.', 'left.')])
    lookup.merge!(responses)
    queries, budgets = [], []
    resolver = Object.new
    resolver.define_singleton_method(:query_no_validation_or_recursion) do |name, type|
      before_response&.call(name, type)
      identity = [name.to_s(true), type.to_s]
      queries << identity
      budgets << Dnsruby::ValidationBudget.current
      lookup.fetch(identity)
    end
    Dnsruby::Dnssec.stub(:do_validation_with_recursor?, false) do
      Dnsruby::Dnssec.stub(:default_resolver, resolver) do
        @verifier.stub(:get_nameservers_for, resolver) do
          @verifier.stub(:get_nameservers_from_message, resolver) { yield queries, budgets }
        end
      end
    end
  end
end
