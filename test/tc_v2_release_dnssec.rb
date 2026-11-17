require_relative 'tc_dnssec_alias_validation'

class DnssecAliasValidationTest
  def test_signed_alias_to_unsigned_target_authenticates_target_delegation
    message = message_with('alias.left.', 'A', [signed('alias.left. 300 IN CNAME target.right.', 'left.'),
      Dnsruby::RRSet.new([Dnsruby::RR.create('target.right. 300 IN A 192.0.2.1')])])
    message.header.ad = true
    with_chain_resolver(responses: unsigned_delegation) do |queries, budgets|
      assert(@verifier.validate(message, nil))
      assert_equal(Dnsruby::Message::SecurityLevel.INSECURE, message.security_level)
      refute(message.header.ad)
      assert_includes(queries, ['right.', 'DS'])
      refute_includes(queries, ['right.', 'DNSKEY'])
      assert_equal(1, budgets.uniq.length)
    end
  end

  def test_signed_alias_to_unsigned_negative_target_accepts_unsigned_soa
    [Dnsruby::RCode.NOERROR, Dnsruby::RCode.NXDOMAIN].each do |rcode|
      message = message_with('alias.left.', 'A', [signed('alias.left. 300 IN CNAME absent.right.', 'left.')])
      message.header.rcode = rcode
      message.add_authority(Dnsruby::RR.create('right. 300 IN SOA ns.right. hostmaster.right. 1 3600 600 86400 300'))
      with_chain_resolver(responses: unsigned_delegation) do |_queries, _budgets|
        assert(@verifier.validate(message, nil))
        assert_equal(Dnsruby::Message::SecurityLevel.INSECURE, message.security_level)
      end
    end
  end

  def test_unsigned_alias_target_still_needs_authenticated_delegation
    message = message_with('alias.left.', 'A', [signed('alias.left. 300 IN CNAME target.right.', 'left.'),
      Dnsruby::RRSet.new([Dnsruby::RR.create('target.right. 300 IN A 192.0.2.1')])])
    denial = Dnsruby::Message.new('right.', 'DS')
    with_chain_resolver(responses: {['right.', 'DS'] => denial}) do |_queries, _budgets|
      assert_raises(Dnsruby::VerifyError) { @verifier.validate(message, nil) }
    end
  end

  def test_unsigned_alias_positive_cache_lifetime_is_bounded_by_no_ds_proof
    clock = @now
    Time.stub(:now, -> { Time.at(clock) }) do
      message = message_with('alias.left.', 'A', [signed('alias.left. 300 IN CNAME target.right.', 'left.'),
        Dnsruby::RRSet.new([Dnsruby::RR.create('target.right. 300 IN A 192.0.2.1')])])
      with_chain_resolver(responses: unsigned_delegation(lifetime: 7)) do |_queries, _budgets|
        assert(@verifier.validate(message, nil))
      end
      assert_equal(@now + 7, message.instance_variable_get(:@dnsruby_authenticated_expiration))
      cache = Dnsruby::Cache.new(preserve_security: true)
      cache.add(message)
      clock = @now + 6
      refute_nil(cache.find('alias.left.', 'A'))
      clock = @now + 7
      assert_nil(cache.find('alias.left.', 'A'))
    end
  end

  def test_unsigned_alias_negative_cache_lifetime_includes_proof_trust_chain
    clock = @now
    Time.stub(:now, -> { Time.at(clock) }) do
      message = message_with('alias.left.', 'A', [signed('alias.left. 300 IN CNAME absent.right.', 'left.')])
      message.header.rcode = Dnsruby::RCode.NXDOMAIN
      message.add_authority(Dnsruby::RR.create('right. 300 IN SOA ns.right. hostmaster.right. 1 3600 600 86400 300'))
      responses = unsigned_delegation(lifetime: 7).merge(
        ['.', 'DNSKEY'] => message_with('.', 'DNSKEY', [signed(@keys.fetch('.').first, '.', lifetime: 5)]))
      with_chain_resolver(responses: responses) { |_queries, _budgets| assert(@verifier.validate(message, nil)) }
      assert_equal(@now + 5, message.instance_variable_get(:@dnsruby_authenticated_expiration))
      cache = Dnsruby::Cache.new(preserve_security: true)
      cache.add(message)
      clock = @now + 4
      refute_nil(cache.find('alias.left.', 'A'))
      clock = @now + 5
      assert_nil(cache.find('alias.left.', 'A'))
    end
  end

  def test_unsigned_alias_insecurity_expiration_tracks_authenticated_unsupported_ds
    Time.stub(:now, Time.at(@now)) do
      message = message_with('alias.left.', 'A', [signed('alias.left. 300 IN CNAME target.right.', 'left.'),
        Dnsruby::RRSet.new([Dnsruby::RR.create('target.right. 300 IN A 192.0.2.1')])])
      ds = Dnsruby::RR.create('right. 300 IN DS 12345 5 2 ' + '01' * 32)
      responses = {['right.', 'DS'] => message_with('right.', 'DS', [signed(ds, '.', lifetime: 7)])}
      with_chain_resolver(responses: responses) { |_queries, _budgets| assert(@verifier.validate(message, nil)) }
      assert_equal(Dnsruby::Message::SecurityLevel.INSECURE, message.security_level)
      assert_equal(@now + 7, message.instance_variable_get(:@dnsruby_authenticated_expiration))
    end
  end

  private

  def unsigned_delegation(lifetime: 3600)
    denial = Dnsruby::Message.new('right.', 'DS')
    signed('right. 300 IN NSEC z. NS NSEC RRSIG', '.', lifetime: lifetime).each { |rr| denial.add_authority(rr) }
    {['right.', 'DS'] => denial}
  end
end

class V2ReleaseDnssecTest < Minitest::Test
  def setup
    @verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
  end

  def test_rsa_key_and_algorithm_mutation_refresh_public_key_and_length
    first = rsa_dnskey(OpenSSL::PKey::RSA.generate(1024), 8)
    second = rsa_dnskey(OpenSSL::PKey::RSA.generate(2048), 8)
    old = first.public_key
    first.key = Base64.strict_encode64(second.key).freeze
    refute_equal(old.n, first.public_key.n)
    assert_equal(second.public_key.n, first.public_key.n)
    assert_equal(2048, first.key_length)
    current = first.public_key
    first.algorithm = 10
    assert_nil(first.key_length)
    refute_same(current, first.public_key)
    assert_equal(second.public_key.n, first.public_key.n)
    assert_equal(2048, first.key_length)
  end

  def test_sha1_crypto_remains_available_but_configured_legacy_anchors_are_insecure
    private_key = OpenSSL::PKey::RSA.generate(1024)
    [5, 7].each do |algorithm|
      key = rsa_dnskey(private_key, algorithm)
      verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
      verifier.add_trust_anchor(key)
      record = Dnsruby::RR.create('www.example. 60 IN A 192.0.2.1')
      signature = Dnsruby::RR.create(name: record.name, type: 'RRSIG', ttl: 60,
        type_covered: 'A', algorithm: algorithm, labels: 2, original_ttl: 60,
        expiration: Time.now.to_i + 600, inception: Time.now.to_i - 60,
        key_tag: key.key_tag, signers_name: key.name)
      data = Dnsruby::MessageEncoder.new { |encoder| encoder.put_rr(record, true) }.to_s
      signature.signature = private_key.sign(OpenSSL::Digest::SHA1.new, signature.sig_data + data)
      rrset = Dnsruby::RRSet.new([record, signature])
      assert(verifier.verify(rrset, key))
      message = Dnsruby::Message.new('www.example.', 'A')
      rrset.each { |rr| message.add_answer(rr) }
      message.header.ad = true
      assert(verifier.validate(message, nil))
      assert_equal(Dnsruby::Message::SecurityLevel.INSECURE, message.security_level)
      refute(message.header.ad)
      # Explicit verification remains available after the operational call.
      assert(verifier.verify(rrset, key))
      ds = Dnsruby::RR.create(name: key.name, type: 'DS', klass: key.klass, ttl: 60,
        key_tag: key.key_tag, algorithm: algorithm, digest_type: 2)
      ds.digestbin = ds.digest_key(key)
      ds.digest = ds.digestbin.unpack1('H*')
      key_signature = signature.clone
      key_signature.name, key_signature.type_covered, key_signature.labels = key.name, Dnsruby::Types.DNSKEY, 1
      key_data = Dnsruby::MessageEncoder.new { |encoder| encoder.put_rr(key, true) }.to_s
      key_signature.signature = private_key.sign(OpenSSL::Digest::SHA1.new, key_signature.sig_data + key_data)
      assert(verifier.verify(Dnsruby::RRSet.new([key, key_signature]), ds))
    end
  end

  def test_rsa_integer_encoding_and_protocol_bounds_are_checked_before_openssl
    exponent = "\x01\x00\x01".b
    modulus = "\x80".b + "\x01".b * 63
    cases = [
      [8, "\0".b + [513].pack('n') + "\x01".b * 513 + modulus],
      [8, [3].pack('C') + exponent + "\x80".b + "\x01".b * 512],
      [8, [4].pack('C') + "\0".b + exponent + modulus],
      [8, [3].pack('C') + exponent + "\0".b + modulus],
      [8, [3].pack('C') + exponent + modulus.byteslice(0, 63)],
      [10, [3].pack('C') + exponent + modulus]
    ]
    OpenSSL::PKey::RSA.stub(:new, ->(*) { flunk('invalid DNSKEY reached OpenSSL') }) do
      cases.each do |algorithm, bytes|
        record = Dnsruby::RR::IN::DNSKEY.new([257, 3, algorithm, bytes])
        assert_raises(OpenSSL::PKey::PKeyError) { record.public_key }
      end
    end
  end

  def test_legacy_only_authenticated_ds_records_are_operationally_unsupported
    [5, 7].each do |algorithm|
      record = Dnsruby::RR.create("example. 60 IN DS 12345 #{algorithm} 2 #{'01' * 32}")
      assert_equal([false, nil], @verifier.get_anchor_for(record.name, record.name, record))
    end
  end

  def test_new_ds_delegations_default_to_sha256_and_reject_deprecated_algorithms
    private_key = OpenSSL::PKey::RSA.generate(1024)
    modern = rsa_dnskey(private_key, 8)
    default = Dnsruby::RR::DS.from_key(modern)
    assert_equal(2, default.digest_type.code)
    assert(default.check_key(modern))
    assert(Dnsruby::RR::DS.from_key(modern, 4).check_key(modern))
    assert_raises(ArgumentError) { Dnsruby::RR::DS.from_key(modern, 1) }
    [5, 7].each do |algorithm|
      legacy = rsa_dnskey(private_key, algorithm)
      assert_raises(ArgumentError) { Dnsruby::RR::DS.from_key(legacy) }
      assert_raises(ArgumentError) { Dnsruby::RR::DS.from_key(legacy, 2) }
    end
  end

  def test_ds_digest_preference_applies_to_explicit_configured_and_rrset_checks
    keys = [1024, 2048].map { |bits| rsa_dnskey(OpenSSL::PKey::RSA.generate(bits), 8) }
    # Legacy DS digest verification remains supported, even though publishing
    # new SHA-1 delegations through from_key is prohibited.
    sha1 = Dnsruby::RR::DS.from_key(keys.first)
    sha1.digest_type = 1
    sha1.digestbin = sha1.digest_key(keys.first)
    sha1.digest = sha1.digestbin.unpack1('H*')
    sha256 = Dnsruby::RR::DS.from_key(keys.last, 2)
    rrset = Dnsruby::RRSet.new(keys)
    dsset = Dnsruby::RRSet.new([sha1, sha256])
    candidates = @verifier.send(:ds_candidate_keys, rrset, dsset)
    assert_equal([keys.last.key], candidates.values.map { |key, _| key.key })
    [sha1, sha256].each { |ds| @verifier.add_trust_anchor(ds) }
    assert_equal([keys.last.key], @verifier.send(:ds_candidate_keys, rrset).values.map { |key, _| key.key })
    anchor = @verifier.find_closest_anchor_for(keys.first.name)
    assert_instance_of(Dnsruby::RRSet, anchor)
    assert_equal(2, anchor.rrs.length)
    assert_equal([keys.last.key], @verifier.send(:ds_candidate_keys, rrset, anchor).values.map { |key, _| key.key })
    assert(@verifier.check_ds(keys.last, dsset, Time.now.to_i + 60))
    refute(@verifier.check_ds(keys.first, dsset, Time.now.to_i + 60))
    assert(@verifier.check_ds(keys.first, Dnsruby::RRSet.new([sha1]), Time.now.to_i + 60))
  end

  def test_nsec3_and_nsec3param_round_trip_full_salt_length_range
    [0, 127, 128, 255].each do |length|
      salt = length.zero? ? '-' : 'ab' * length
      ['NSEC3PARAM 1 0 0 ' + salt,
        'NSEC3 1 0 0 ' + salt + ' 00000000000000000000000000000000 A RRSIG'].each do |rdata|
        record = Dnsruby::RR.create('example. 60 IN ' + rdata)
        message = Dnsruby::Message.new
        message.add_answer(record)
        decoded = Dnsruby::Message.decode(message.encode).answer.first
        assert_equal(length, decoded.salt_length)
        assert_equal(salt, decoded.salt)
        assert_equal(record.to_s, decoded.to_s)
      end
    end
  end

  def test_cache_identity_tracks_trust_addition_removal_and_expiry
    before = @verifier.validation_cache_identity
    key = rsa_dnskey(OpenSSL::PKey::RSA.generate(1024), 8)
    @verifier.add_trust_anchor(key)
    added = @verifier.validation_cache_identity
    refute_equal(before, added)
    assert_equal(added, @verifier.validation_cache_identity)
    @verifier.remove_trust_anchor(key)
    refute_equal(added, @verifier.validation_cache_identity)
    now = Time.now.to_i
    @verifier.add_trust_anchor_with_expiration(key, now + 1)
    current = @verifier.validation_cache_identity
    Time.stub(:now, Time.at(now + 2)) { refute_equal(current, @verifier.validation_cache_identity) }
  end

  def test_unknown_nsec3_hash_with_unsigned_length_round_trips
    record = Dnsruby::RR::IN::NSEC3.new([254, 0, 0, 0, ''.b, 128, "\x01".b * 128, []])
    record.name = 'example.'
    record.type, record.klass, record.ttl = Dnsruby::Types.NSEC3, Dnsruby::Classes.IN, 60
    message = Dnsruby::Message.new
    message.add_answer(record)
    decoded = Dnsruby::Message.decode(message.encode).answer.first
    assert_equal(254, decoded.hash_alg.code)
    assert_equal(128, decoded.hash_length)
    assert_equal(record.next_hashed, decoded.next_hashed)
  end

  private

  def rsa_dnskey(private_key, algorithm)
    exponent = private_key.e.to_s(2)
    bytes = [exponent.bytesize].pack('C') + exponent + private_key.n.to_s(2)
    Dnsruby::RR.create("example. 60 IN DNSKEY 257 3 #{algorithm} #{Base64.strict_encode64(bytes)}")
  end
end

module V2PublicDnssecHelper
  def with_public_verifier(verifier)
    names = %i[@@anchor_verifier @@root_verifier @@dlv_verifier]
    previous = names.to_h { |name| [name, Dnsruby::Dnssec.class_variable_get(name)] }
    policy = Dnsruby::Dnssec.validation_policy
    Dnsruby::Dnssec.class_variable_set(:@@anchor_verifier, verifier)
    Dnsruby::Dnssec.class_variable_set(:@@root_verifier,
      Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ROOT))
    Dnsruby::Dnssec.class_variable_set(:@@dlv_verifier,
      Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::DLV))
    Dnsruby::Dnssec.validation_policy = Dnsruby::Dnssec::ValidationPolicy::ALWAYS_LOCAL_ANCHORS_ONLY
    yield
  ensure
    previous.each { |name, saved| Dnsruby::Dnssec.class_variable_set(name, saved) }
    Dnsruby::Dnssec.validation_policy = policy
  end
end

class V2ReleaseDnssecTest
  include V2PublicDnssecHelper

  def test_public_validation_fast_path_applies_legacy_algorithm_policy
    private_key = OpenSSL::PKey::RSA.generate(1024)
    [5, 7].each do |algorithm|
      key = rsa_dnskey(private_key, algorithm)
      verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
      verifier.add_trust_anchor(key)
      records = signed_rsa('www.example. 60 IN A 192.0.2.1', key, private_key)
      message = Dnsruby::Message.new('www.example.', 'A')
      records.each { |rr| message.add_answer(rr) }
      message.header.ad = true
      query = Dnsruby::Message.new('www.example.', 'A')
      query.header.cd = true
      with_public_verifier(verifier) do
        assert(Dnsruby::Dnssec.verify(records, key))
        assert(Dnsruby::Dnssec.validate_with_query(query, message))
        assert_equal(Dnsruby::Message::SecurityLevel.INSECURE, message.security_level)
        refute(message.header.ad)
        assert(Dnsruby::Dnssec.verify(records, key))
      end
    end
  end

  def test_historical_verification_cannot_seed_operational_trust_for_modern_descendants
    legacy_private = OpenSSL::PKey::RSA.generate(1024)
    modern_private = OpenSSL::PKey::RSA.generate(1024)
    legacy = rsa_dnskey(legacy_private, 5)
    modern = rsa_dnskey(modern_private, 8)
    modern.name = 'child.example.'
    @verifier.add_trust_anchor(legacy)
    ds_records = signed_rsa(Dnsruby::RR::DS.from_key(modern), legacy, legacy_private)
    keys = signed_rsa(modern, modern, modern_private)
    answer = signed_rsa('www.child.example. 60 IN A 192.0.2.1', modern, modern_private)
    message = Dnsruby::Message.new('www.child.example.', 'A')
    answer.each { |rr| message.add_answer(rr) }
    query = Dnsruby::Message.new('www.child.example.', 'A')
    query.header.cd = true
    with_public_verifier(@verifier) do
      assert(Dnsruby::Dnssec.verify(ds_records))
      assert(Dnsruby::Dnssec.verify(keys))
      assert(Dnsruby::Dnssec.verify(answer))
      assert_raises(Dnsruby::VerifyError) { @verifier.verify_for_validation(message) }
      assert(Dnsruby::Dnssec.validate_with_query(query, message))
      assert_equal(Dnsruby::Message::SecurityLevel.INSECURE, message.security_level)
    end
  end

  def test_public_validation_fast_path_obeys_exclusive_anchor_policy
    private_key = OpenSSL::PKey::RSA.generate(1024)
    key = rsa_dnskey(private_key, 8)
    @verifier.add_trust_anchor(key)
    records = signed_rsa('www.example. 60 IN A 192.0.2.1', key, private_key)
    policies = [Dnsruby::Dnssec::ValidationPolicy::ALWAYS_ROOT_ONLY,
      Dnsruby::Dnssec::ValidationPolicy::ALWAYS_LOCAL_ANCHORS_ONLY]
    policies.each do |policy|
      message = Dnsruby::Message.new('www.example.', 'A')
      records.each { |rr| message.add_answer(rr) }
      with_public_verifier(@verifier) do
        if policy == Dnsruby::Dnssec::ValidationPolicy::ALWAYS_LOCAL_ANCHORS_ONLY
          Dnsruby::Dnssec.class_variable_set(:@@root_verifier, @verifier)
          Dnsruby::Dnssec.class_variable_set(:@@anchor_verifier,
            Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR))
        end
        # A configured DLV key is also outside either exclusive policy.
        Dnsruby::Dnssec.class_variable_set(:@@dlv_verifier, @verifier)
        Dnsruby::Dnssec.validation_policy = policy
        assert(Dnsruby::Dnssec.verify(records))
        assert(Dnsruby::Dnssec.validate(message))
        assert_equal(Dnsruby::Message::SecurityLevel.INSECURE, message.security_level)
      end
    end
  end

  def test_nsec3_binary_hyphen_salt_round_trips
    %w[NSEC3 NSEC3PARAM].each do |type|
      text = "example. 60 IN #{type} 1 0 0 2d"
      text += ' 00000000000000000000000000000000 A' if type == 'NSEC3'
      record = Dnsruby::RR.create(text)
      message = Dnsruby::Message.new
      message.add_answer(record)
      decoded = Dnsruby::Message.decode(message.encode).answer.first
      assert_equal('2d', decoded.salt)
      assert_equal(1, decoded.salt_length)
    end
  end

  def test_validation_fast_path_leaves_dlv_eligibility_to_full_policy
    private_key = OpenSSL::PKey::RSA.generate(1024)
    key = rsa_dnskey(private_key, 8)
    @verifier.add_trust_anchor(key)
    records = signed_rsa('www.example. 60 IN A 192.0.2.1', key, private_key)
    message = Dnsruby::Message.new('www.example.', 'A')
    records.each { |rr| message.add_answer(rr) }
    with_public_verifier(@verifier) do
      Dnsruby::Dnssec.class_variable_set(:@@dlv_verifier, @verifier)
      Dnsruby::Dnssec.class_variable_set(:@@anchor_verifier,
        Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR))
      Dnsruby::Dnssec.validation_policy = Dnsruby::Dnssec::ValidationPolicy::LOCAL_ANCHORS_THEN_ROOT
      failure = ->(*) { raise Dnsruby::VerifyError, 'primary trust path failed' }
      Dnsruby::Dnssec.stub(:validate_with_anchors, failure) do
        Dnsruby::Dnssec.stub(:validate_with_root, failure) do
          assert_raises(Dnsruby::VerifyError) { Dnsruby::Dnssec.validate(message) }
        end
      end
      assert(Dnsruby::Dnssec.verify(records))
    end
  end

  private

  def signed_rsa(record, key, private_key)
    record = Dnsruby::RR.create(record) if record.is_a?(String)
    signature = Dnsruby::RR.create(name: record.name, type: 'RRSIG', ttl: record.ttl,
      type_covered: record.type, algorithm: key.algorithm, labels: record.name.labels.length,
      original_ttl: record.ttl, expiration: Time.now.to_i + 600,
      inception: Time.now.to_i - 60, key_tag: key.key_tag, signers_name: key.name)
    data = Dnsruby::MessageEncoder.new { |encoder| encoder.put_rr(record, true) }.to_s
    digest = [5, 7].include?(key.algorithm.code) ? OpenSSL::Digest::SHA1.new : OpenSSL::Digest::SHA256.new
    signature.signature = private_key.sign(digest, signature.sig_data + data)
    Dnsruby::RRSet.new([record, signature])
  end
end

require_relative 'tc_dnssec_denial_proofs'
class DnssecDenialProofsTest
  include V2PublicDnssecHelper

  def test_public_validation_full_policy_retains_cached_opt_out_proof_lifetime
    message = response('missing.collision.invalid.', 'AAAA', nxdomain: true)
    add_chain(message, flags: 1)
    with_public_verifier(@verifier) do
      assert_equal(:insecure, @verifier.verify_for_validation(message))
      cached_expiry = message.instance_variable_get(:@dnsruby_authenticated_expiration)
      full_validation = @verifier.method(:validate)
      policy_calls = 0
      validate = lambda do |*args|
        policy_calls += 1
        full_validation.call(*args)
      end
      @verifier.stub(:follow_chain, Dnsruby::RRSet.new([@key])) do
        @verifier.stub(:validate, validate) do
          assert(Dnsruby::Dnssec.validate(message))
        end
      end
      assert_equal(1, policy_calls)
      assert_equal(Dnsruby::Message::SecurityLevel.INSECURE, message.security_level)
      expiry = message.instance_variable_get(:@dnsruby_authenticated_expiration)
      assert(expiry)
      assert_equal(cached_expiry, expiry)
      assert_operator(expiry, :<=, Time.now.to_i + 300)
    end
  end
end
