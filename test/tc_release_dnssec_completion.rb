require_relative 'tc_key_cache_identity'

class ReleaseDnssecCompletionTest < Minitest::Test
  def setup
    @now = Time.now.to_i
    @verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
  end

  def test_ds_owner_equivalence_and_configured_relative_anchor
    private_key = OpenSSL::PKey::EC.new(KeyCacheIdentityTest::PRIVATE_KEYS.first)
    public_bytes = private_key.public_key.to_octet_string(:uncompressed).byteslice(1, 64)
    key = Dnsruby::RR.create("Example.Invalid. 300 IN DNSKEY 257 3 13 #{Base64.strict_encode64(public_bytes)}")
    ds = Dnsruby::RR::DS.from_key(key, 2)
    ds.name = 'example.invalid'
    assert(ds.check_key(key))
    @verifier.add_trust_anchor(ds)
    signature = signature_for(key, key)
    der = private_key.dsa_sign_asn1(OpenSSL::Digest::SHA256.digest(canonical_data(key, signature)))
    signature.signature = OpenSSL::ASN1.decode(der).value.map { |n| n.value.to_s(2).rjust(32, "\0") }.join
    assert(@verifier.verify(Dnsruby::RRSet.new([key, signature])))
    other_owner = key.clone
    other_owner.name = 'other.invalid.'
    refute(ds.check_key(other_owner))
    other_class = key.clone
    other_class.klass = Dnsruby::Classes.CH
    refute(ds.check_key(other_class))
  end

  def test_ed25519_verification_and_public_operational_validation
    key, private_key = ed25519_key
    record = Dnsruby::RR.create('www.example.invalid. 300 IN A 192.0.2.1')
    records = eddsa_signed(record, key, private_key)
    assert_equal('ED25519', key.algorithm.to_s)
    assert_equal(256, key.key_length)
    assert_equal(32, key.key.bytesize)
    assert_equal(64, records.sigs.first.signature.bytesize)
    assert(@verifier.verify_rrset(records, key))
    @verifier.add_trust_anchor(key)
    message = Dnsruby::Message.new(record.name, 'A')
    records.each { |rr| message.add_answer(rr) }
    with_verifiers(anchor: @verifier) do
      Dnsruby::Dnssec.validation_policy = Dnsruby::Dnssec::ValidationPolicy::ALWAYS_LOCAL_ANCHORS_ONLY
      assert(Dnsruby::Dnssec.validate(message))
      assert_equal(Dnsruby::Message::SecurityLevel.SECURE, message.security_level)
    end
  end

  def test_ed25519_matches_published_dnssec_examples
    skip 'OpenSSL provider does not support Ed25519' unless Dnsruby::RR::DNSKEY.ed25519_supported?
    # RFC 8080 section 6.1, corrected by verified erratum 4935:
    # https://www.rfc-editor.org/rfc/inline-errata/rfc8080.html
    vectors = [
      ['l02Woi0iS8Aa25FQkUd9RMzZHJpBoRQwAQEX1SxZJA4=', 3613,
        '3aa5ab37efce57f737fc1627013fee07bdf241bd10f3b1964ab55c78e79a304b',
        'oL9krJun7xfBOIWcGHi7mag5/hdZrKWw15jPGrHpjQeRAvTdszaPD+QLs3fx8A4M3e23mRZ9VrbpMngwcrqNAg=='],
      ['zPnZ/QwEe7S8C5SPz2OfS5RR40ATk2/rYnE9xHIEijs=', 35217,
        '401781b934e392de492ec77ae2e15d70f6575a1c0bc59c5275c04ebe80c6614c',
        'zXQ0bkYgQTEFyfLyi9QoiY6D8ZdYo4wyUhVioYZXFdT410QPRITQSqJSnzQoSm5poJ7gD7AQR0O7KuI5k2pcBg==']
    ]
    Time.stub(:now, Time.at(1_439_000_000)) do
      record = Dnsruby::RR.create('example.com. 3600 IN MX 10 mail.example.com.')
      vectors.each do |public_key, tag, digest, signature|
        key = Dnsruby::RR.create("example.com. 3600 IN DNSKEY 257 3 15 #{public_key}")
        ds = Dnsruby::RR.create("example.com. 3600 IN DS #{tag} 15 2 #{digest}")
        sig = Dnsruby::RR.create("example.com. 3600 IN RRSIG MX 15 2 3600 1440021600 1438207200 #{tag} example.com. #{signature}")
        assert_equal(tag, key.key_tag)
        assert(ds.check_key(key))
        assert(@verifier.verify_rrset(Dnsruby::RRSet.new([record, sig]), key))
      end
    end
  end

  def test_ed25519_obeys_signature_time_identity_and_budget_checks
    key, private_key = ed25519_key
    record = Dnsruby::RR.create('www.example.invalid. 300 IN A 192.0.2.1')
    expired = eddsa_signed(record, key, private_key, expiration: @now - 1)
    assert_raises(Dnsruby::VerifyError) { @verifier.verify_rrset(expired, key) }
    other_owner = key.clone
    other_owner.name = 'other.invalid.'
    assert_raises(Dnsruby::VerifyError) { @verifier.verify_rrset(eddsa_signed(record, key, private_key), other_owner) }
    records = eddsa_signed(record, key, private_key)
    limits = Dnsruby::Dnssec.validation_limits.merge(max_signature_verifications: 0)
    budget = Dnsruby::ValidationBudget.new(limits: limits)
    assert_raises(Dnsruby::ValidationLimitError) do
      Dnsruby::ValidationBudget.with(budget) { @verifier.verify_rrset(records, key) }
    end
  end

  def test_ed25519_wire_sizes_and_changed_signatures_are_rejected
    key, private_key = ed25519_key
    assert_raises(ArgumentError) do
      Dnsruby::RR.create("example.invalid. 300 IN DNSKEY 257 3 15 #{Base64.strict_encode64(key.key.byteslice(0, 31))}")
    end
    record = Dnsruby::RR.create('www.example.invalid. 300 IN A 192.0.2.1')
    records = eddsa_signed(record, key, private_key)
    records.sigs.first.signature = records.sigs.first.signature.byteslice(0, 63)
    assert_raises(Dnsruby::VerifyError) { @verifier.verify_rrset(records, key) }
    records = eddsa_signed(record, key, private_key)
    records.sigs.first.signature = private_key.sign(nil, 'different ordinary message')
    assert_raises(Dnsruby::VerifyError) { @verifier.verify_rrset(records, key) }
  end

  def test_ed25519_unavailable_provider_uses_unsupported_algorithm_policy
    key, private_key = ed25519_key
    ds = Dnsruby::RR::DS.from_key(key, 2)
    records = eddsa_signed(Dnsruby::RR.create('www.example.invalid. 300 IN A 192.0.2.1'), key, private_key)
    assert(@verifier.send(:supported_ds?, ds))
    Dnsruby::RR::DNSKEY.stub(:ed25519_supported?, false) do
      refute(@verifier.send(:supported_ds?, ds))
      assert_raises(Dnsruby::VerifyError) { @verifier.verify_rrset(records, key) }
    end
  end

  def test_ed25519_old_openssl_binding_is_reported_as_unsupported
    klass = Dnsruby::RR::DNSKEY
    saved = klass.instance_variable_get(:@ed25519_supported)
    existed = klass.instance_variable_defined?(:@ed25519_supported)
    klass.remove_instance_variable(:@ed25519_supported) if existed
    old_key = Object.new
    old_key.define_singleton_method(:verify) { |*| raise TypeError, 'no implicit conversion of nil into String' }
    OpenSSL::PKey.stub(:read, old_key) { refute(klass.ed25519_supported?) }
  ensure
    if existed
      klass.instance_variable_set(:@ed25519_supported, saved)
    elsif klass.instance_variable_defined?(:@ed25519_supported)
      klass.remove_instance_variable(:@ed25519_supported)
    end
  end

  def test_ed448_verification_and_public_operational_validation
    key, private_key = ed448_key
    record = Dnsruby::RR.create('www.example.invalid. 300 IN A 192.0.2.1')
    records = eddsa_signed(record, key, private_key)
    assert_equal('ED448', key.algorithm.to_s)
    assert_equal(448, key.key_length)
    assert_equal(57, key.key.bytesize)
    assert_equal(114, records.sigs.first.signature.bytesize)
    assert(@verifier.verify_rrset(records, key))
    @verifier.add_trust_anchor(key)
    message = Dnsruby::Message.new(record.name, 'A')
    records.each { |rr| message.add_answer(rr) }
    with_verifiers(anchor: @verifier) do
      Dnsruby::Dnssec.validation_policy = Dnsruby::Dnssec::ValidationPolicy::ALWAYS_LOCAL_ANCHORS_ONLY
      assert(Dnsruby::Dnssec.validate(message))
      assert_equal(Dnsruby::Message::SecurityLevel.SECURE, message.security_level)
    end
  end

  def test_ed448_matches_published_dnssec_examples
    skip 'OpenSSL provider does not support Ed448' unless Dnsruby::RR::DNSKEY.ed448_supported?
    # RFC 8080 section 6.2, corrected by verified erratum 4935:
    # https://www.rfc-editor.org/rfc/inline-errata/rfc8080.html
    vectors = [
      ['3kgROaDjrh0H2iuixWBrc8g2EpBBLCdGzHmn+G2MpTPhpj/OiBVHHSfPodx1FYYUcJKm1MDpJtIA', 9713,
        '6ccf18d5bc5d7fc2fceb1d59d17321402f2aa8d368048db93dd811f5cb2b19c7',
        '3cPAHkmlnxcDHMyg7vFC34l0blBhuG1qpwLmjInI8w1CMB29FkEAIJUA0amxWndkmnBZ6SKiwZSAxGILn/NBtOXft0+Gj7FSvOKxE/07+4RQvE581N3Aj/JtIyaiYVdnYtyMWbSNyGEY2213WKsJlwEA'],
      ['kkreGWoccSDmUBGAe7+zsbG6ZAFQp+syPmYUurBRQc3tDjeMCJcVMRDmgcNLp5HlHAMy12VoISsA', 38353,
        '645ff078b3568f5852b70cb60e8e696cc77b75bfaaffc118cf79cbda1ba28af4',
        'E1/oLjSGIbmLny/4fcgM1z4oL6aqo+izT3urCyHyvEp4Sp8Syg1eI+lJ57CSnZqjJP41O/9l4m0AsQ4f7qI1gVnML8vWWiyW2KXhT9kuAICUSxv5OWbf81Rq7Yu60npabODB0QFPb/rkW3kUZmQ0YQUA']
    ]
    Time.stub(:now, Time.at(1_439_000_000)) do
      record = Dnsruby::RR.create('example.com. 3600 IN MX 10 mail.example.com.')
      vectors.each do |public_key, tag, digest, signature|
        key = Dnsruby::RR.create("example.com. 3600 IN DNSKEY 257 3 16 #{public_key}")
        ds = Dnsruby::RR.create("example.com. 3600 IN DS #{tag} 16 2 #{digest}")
        sig = Dnsruby::RR.create("example.com. 3600 IN RRSIG MX 16 2 3600 1440021600 1438207200 #{tag} example.com. #{signature}")
        assert_equal(tag, key.key_tag)
        assert(ds.check_key(key))
        assert(@verifier.verify_rrset(Dnsruby::RRSet.new([record, sig]), key))
      end
    end
  end

  def test_ed448_wire_sizes_and_unavailable_provider_policy
    key, private_key = ed448_key
    assert_raises(ArgumentError) do
      Dnsruby::RR.create("example.invalid. 300 IN DNSKEY 257 3 16 #{Base64.strict_encode64(key.key.byteslice(0, 56))}")
    end
    record = Dnsruby::RR.create('www.example.invalid. 300 IN A 192.0.2.1')
    records = eddsa_signed(record, key, private_key)
    records.sigs.first.signature = records.sigs.first.signature.byteslice(0, 113)
    assert_raises(Dnsruby::VerifyError) { @verifier.verify_rrset(records, key) }
    ds = Dnsruby::RR::DS.from_key(key, 2)
    assert(@verifier.send(:supported_ds?, ds))
    Dnsruby::RR::DNSKEY.stub(:ed448_supported?, false) do
      refute(@verifier.send(:supported_ds?, ds))
      assert_raises(Dnsruby::VerifyError) { @verifier.verify_rrset(eddsa_signed(record, key, private_key), key) }
    end
  end

  def test_ed448_old_openssl_binding_is_reported_as_unsupported
    klass = Dnsruby::RR::DNSKEY
    saved = klass.instance_variable_get(:@ed448_supported)
    existed = klass.instance_variable_defined?(:@ed448_supported)
    klass.remove_instance_variable(:@ed448_supported) if existed
    old_key = Object.new
    old_key.define_singleton_method(:verify) { |*| raise TypeError, 'no implicit conversion of nil into String' }
    OpenSSL::PKey.stub(:read, old_key) { refute(klass.ed448_supported?) }
  ensure
    if existed
      klass.instance_variable_set(:@ed448_supported, saved)
    elsif klass.instance_variable_defined?(:@ed448_supported)
      klass.remove_instance_variable(:@ed448_supported)
    end
  end

  def test_all_root_policies_preserve_authenticated_insecure_negative_lifetime
    policies = Dnsruby::Dnssec::ValidationPolicy
    [policies::ALWAYS_ROOT_ONLY, policies::LOCAL_ANCHORS_THEN_ROOT, policies::ROOT_THEN_LOCAL_ANCHORS].each do |policy|
      root = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
      root.add_trust_anchor(Dnsruby::RR.create(". 300 IN DS 12345 13 2 #{'AB' * 32}"))
      deadline = @now + 7
      root.define_singleton_method(:follow_chain) do |*|
        send(:trust_operation)[:insecurity_expirations] << deadline
        nil
      end
      query = Dnsruby::Message.new('absent.example.invalid.', 'A')
      query.do_validation = true
      query.header.cd = true
      message = Dnsruby::Message.new('absent.example.invalid.', 'A')
      message.header.qr = true
      message.header.rcode = Dnsruby::RCode.NXDOMAIN
      message.add_authority(Dnsruby::RR.create('example.invalid. 300 IN SOA ns.example.invalid. hostmaster.example.invalid. 1 3600 600 86400 300'))
      with_verifiers(root: root) do
        Dnsruby::Dnssec.validation_policy = policy
        clock = @now
        Time.stub(:now, -> { Time.at(clock) }) do
          assert(Dnsruby::Dnssec.validate_with_query(query, message))
          assert_equal(Dnsruby::Message::SecurityLevel.INSECURE, message.security_level)
          assert_equal(deadline, message.instance_variable_get(:@dnsruby_authenticated_expiration), policy.to_s)
          cache = Dnsruby::Cache::ResolutionCache.new
          cache.add(query, :test_scope, message, Dnsruby::NXDomain.new)
          refute_nil(cache.find(query, :test_scope), policy.to_s)
          clock = deadline
          assert_nil(cache.find(query, :test_scope))
        end
      end
    end
  end

  def test_unanchored_validation_does_not_reuse_previous_insecurity_proof
    message = Dnsruby::Message.new('absent.example.invalid.', 'A')
    message.instance_variable_set(:@dnsruby_authenticated_expiration, @now + 300)
    assert(@verifier.validate(message, nil))
    assert_nil(message.instance_variable_get(:@dnsruby_authenticated_expiration))
  end

  def test_hints_changes_replace_shared_recursor_and_close_bootstrap
    klass = Dnsruby::SingleVerifier
    saved = klass.instance_variables.to_h { |name| [name, klass.instance_variable_get(name)] }
    boots, hints, resets = [], [], []
    bootstrap = Struct.new(:closed) { def close; self.closed = true; end }
    recursor = Struct.new(:dnssec)
    Dnsruby::Recursor.stub(:reset_hints, -> { resets << true }) do
      Dnsruby::Recursor.stub(:set_hints, ->(value, _) { hints << value }) do
        Dnsruby::Recursor.stub(:new, -> { recursor.new }) do
          Dnsruby::Resolver.stub(:new, -> { boots << bootstrap.new; boots.last }) do
            supplied = [+'192.0.2.1']
            Dnsruby::Dnssec.set_hints(supplied)
            supplied.first.replace('192.0.2.9')
            first = @verifier.get_recursor
            assert_same(first, @verifier.get_recursor)
            Dnsruby::Dnssec.set_hints(['192.0.2.2'])
            second = @verifier.get_recursor
            refute_same(first, second)
            assert(second.dnssec)
            assert_equal([['192.0.2.1'], ['192.0.2.2']], hints)
            assert(boots.all?(&:closed))
            klass.set_recursor_hints(nil)
            refute_same(second, @verifier.get_recursor)
            assert_equal(3, resets.length)
          end
        end
      end
    end
  ensure
    (klass.instance_variables - saved.keys).each { |name| klass.remove_instance_variable(name) } if saved
    saved&.each { |name, value| klass.instance_variable_set(name, value) }
  end

  def test_dnssec_reset_discards_shared_recursor_hints
    saved = Dnsruby::Dnssec.class_variables.to_h { |name| [name, Dnsruby::Dnssec.class_variable_get(name)] }
    calls = []
    Dnsruby::SingleVerifier.stub(:set_recursor_hints, ->(hints) { calls << hints }) do
      Dnsruby::Dnssec.reset
      assert_equal([nil], calls)
    end
  ensure
    replacement = Dnsruby::Dnssec.default_resolver
    replacement.close unless replacement.equal?(saved[:@@default_resolver])
    saved.each { |name, value| Dnsruby::Dnssec.class_variable_set(name, value) }
  end

  private

  def ed25519_key
    skip 'OpenSSL provider does not support Ed25519' unless Dnsruby::RR::DNSKEY.ed25519_supported?
    # RFC 8032 section 7.1, test 1. Import its fixed private seed as RFC 8410 PKCS#8.
    seed = ['9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60'].pack('H*')
    der = OpenSSL::ASN1::Sequence.new([
      OpenSSL::ASN1::Integer.new(0),
      OpenSSL::ASN1::Sequence.new([OpenSSL::ASN1::ObjectId.new('1.3.101.112')]),
      OpenSSL::ASN1::OctetString.new(OpenSSL::ASN1::OctetString.new(seed).to_der)
    ]).to_der
    private_key = OpenSSL::PKey.read(der)
    public_bytes = OpenSSL::ASN1.decode(private_key.public_to_der).value.last.value
    assert_equal('d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a', public_bytes.unpack1('H*'))
    [Dnsruby::RR.create("example.invalid. 300 IN DNSKEY 257 3 ED25519 #{Base64.strict_encode64(public_bytes)}"), private_key]
  end

  def ed448_key
    skip 'OpenSSL provider does not support Ed448' unless Dnsruby::RR::DNSKEY.ed448_supported?
    # RFC 8032 section 7.4, empty-message vector, imported as RFC 8410 PKCS#8.
    seed = ['6c82a562cb808d10d632be89c8513ebf' \
      '6c929f34ddfa8c9f63c9960ef6e348a3' \
      '528c8a3fcc2f044e39a3fc5b94492f8f' \
      '032e7549a20098f95b'].pack('H*')
    der = OpenSSL::ASN1::Sequence.new([
      OpenSSL::ASN1::Integer.new(0),
      OpenSSL::ASN1::Sequence.new([OpenSSL::ASN1::ObjectId.new('1.3.101.113')]),
      OpenSSL::ASN1::OctetString.new(OpenSSL::ASN1::OctetString.new(seed).to_der)
    ]).to_der
    private_key = OpenSSL::PKey.read(der)
    public_bytes = OpenSSL::ASN1.decode(private_key.public_to_der).value.last.value
    [Dnsruby::RR.create("example.invalid. 300 IN DNSKEY 257 3 ED448 #{Base64.strict_encode64(public_bytes)}"), private_key]
  end

  def signature_for(record, key, expiration: @now + 300)
    Dnsruby::RR.create(name: record.name, type: 'RRSIG', ttl: record.ttl,
      type_covered: record.type, algorithm: key.algorithm, labels: record.name.labels.length,
      original_ttl: record.ttl, expiration: expiration, inception: @now - 60,
      key_tag: key.key_tag, signers_name: key.name)
  end

  def canonical_data(record, signature)
    signature.sig_data + Dnsruby::MessageEncoder.new { |encoder| encoder.put_rr(record, true) }.to_s
  end

  def eddsa_signed(record, key, private_key, **options)
    signature = signature_for(record, key, **options)
    signature.signature = private_key.sign(nil, canonical_data(record, signature))
    Dnsruby::RRSet.new([record, signature])
  end

  def with_verifiers(anchor: nil, root: nil)
    names = %i[@@anchor_verifier @@root_verifier @@dlv_verifier @@validation_policy]
    saved = names.map { |name| Dnsruby::Dnssec.class_variable_get(name) }
    values = [anchor, root, nil].map { |verifier| verifier || Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR) }
    names.first(3).zip(values).each { |name, value| Dnsruby::Dnssec.class_variable_set(name, value) }
    yield
  ensure
    names.zip(saved).each { |name, value| Dnsruby::Dnssec.class_variable_set(name, value) }
  end
end

class KeyCacheIdentityTest
  def test_changed_anchor_owner_does_not_match_the_old_name_index
    [@a.clone, Dnsruby::RR::DS.from_key(@a, 2)].each do |anchor|
      @verifier.clear_trust_anchors
      @verifier.add_trust_anchor(anchor)
      assert(@verifier.find_closest_anchor_for('www.collision.invalid.'))
      anchor.name = 'moved.invalid.'
      refute(@verifier.find_closest_anchor_for('www.collision.invalid.'))
    end
  end

  def test_owner_indexes_remove_evicted_and_expired_keys_and_ds_sets
    clock = @now
    Time.stub(:now, -> { clock }) do
      keys = Dnsruby::KeyCache.new(max_keys: 1)
      first, second = @a, @a.clone
      second.name = 'second.invalid.'
      keys.add_key_with_expiration(first, @now.to_i + 60)
      assert_equal([first], keys.keys_for_name(Dnsruby::Name.create('COLLISION.INVALID')))
      keys.add_key_with_expiration(second, @now.to_i + 60)
      assert_empty(keys.keys_for_name(first.name))
      assert_equal([second], keys.keys_for_name(second.name))
      keys.delete(second)
      assert_empty(keys.keys_for_name(second.name))
      keys.add_key_with_expiration(second, @now.to_i + 60)
      sets = Dnsruby::DSCache.new(max_records: 1)
      first_ds, second_ds = [first, second].map { |key| Dnsruby::RRSet.new([Dnsruby::RR::DS.from_key(key, 2)]) }
      sets.add(first_ds, @now.to_i + 60)
      assert_equal([first_ds], sets.rrsets_for_name(Dnsruby::Name.create('COLLISION.INVALID')))
      sets.add(second_ds, @now.to_i + 60)
      assert_empty(sets.rrsets_for_name(first.name))
      assert_equal([second_ds], sets.rrsets_for_name(second.name))
      clock = @now + 60
      assert_empty(keys.keys_for_name(second.name))
      assert_empty(sets.rrsets_for_name(second.name))
    end
  end

  def test_closest_anchor_does_not_visit_unrelated_learned_provenance
    24.times do |index|
      key = @a.clone
      key.name = "zone#{index}.invalid."
      @verifier.add_trusted_key(Dnsruby::RRSet.new([key]), @now.to_i + 300)
    end
    @verifier.add_trust_anchor(@a)
    @verifier.stub(:usable_cached_key?, ->(_) { flunk('unrelated provenance should not be consulted') }) do
      assert_equal(@a, @verifier.find_closest_anchor_for('www.COLLISION.invalid'))
    end
  end
end
