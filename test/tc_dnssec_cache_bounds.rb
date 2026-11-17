require_relative 'tc_dnssec_cache_lifetime'

class KeyCacheIdentityTest
  def test_candidate_index_keeps_collisions_and_removes_evicted_and_expired_keys
    cache = Dnsruby::KeyCache.new(max_keys: 2)
    signature = signed(@record, @a, 0).sigs.first
    cache.add_key_with_expiration(@a, @now.to_i + 60)
    cache.add_key_with_expiration(@b, @now.to_i + 120)
    assert_equal([@a, @b], cache.matching_keys([signature]))

    unrelated = @a.clone
    unrelated.name = 'another.invalid.'
    cache.add_key_with_expiration(unrelated, @now.to_i + 120)
    assert_equal([@b], cache.matching_keys([signature]))
    assert_equal([@b, unrelated], cache.keys)
    Time.stub(:now, @now + 121) do
      assert_empty(cache.matching_keys([signature]))
      assert_empty(cache.keys)
    end
  end

  def test_unrelated_cached_keys_do_not_consume_signature_matching_budget
    24.times do |i|
      key = @a.clone
      key.name = "zone#{i}.invalid."
      @verifier.add_trusted_key(Dnsruby::RRSet.new([key]), @now.to_i + 300)
    end
    @verifier.add_trust_anchor(@a)
    budget = Dnsruby::ValidationBudget.new(limits: Dnsruby::Dnssec.validation_limits.merge(max_work: 16))
    Dnsruby::ValidationBudget.with(budget) do
      assert(@verifier.verify_rrset(signed(@record, @a, 0)))
    end
  end

  def test_configured_ds_index_preserves_colliding_key_identities
    [@a, @b].each { |key| @verifier.add_trust_anchor(Dnsruby::RR::DS.from_key(key, 2)) }
    assert(@verifier.verify(signed(@a, @a, 0)))
    assert(@verifier.verify(signed(@b, @b, 1)))
  end

  def test_ds_anchor_expiration_prevents_in_flight_key_publication
    anchor = Dnsruby::RR::DS.from_key(@a, 2)
    @verifier.add_trust_anchor_with_expiration(anchor, @now.to_i + 1)
    clock = @now
    Time.stub(:now, -> { clock }) do
      pause_after_verification(-> { clock = @now + 1 }) do
        assert(@verifier.verify(signed(@a, @a, 0)))
      end
      assert_empty(@verifier.trusted_keys)
      assert_empty(@verifier.trust_anchors)
      assert_raises(Dnsruby::VerifyError) { @verifier.verify(signed(@record, @a, 0)) }
    end
  end

  def test_public_rrset_verification_uses_root_and_dlv_fallbacks
    [:root_verifier, :dlv_verifier].each do |destination|
      Dnsruby::Dnssec.reset
      Dnsruby::Dnssec.public_send(destination).add_trust_anchor(@a)
      assert(Dnsruby::Dnssec.verify_rrset(signed(@record, @a, 0)), destination.to_s)
    end
  ensure
    Dnsruby::Dnssec.reset
  end

  def test_public_rrset_fallback_keeps_one_signature_budget
    Dnsruby::Dnssec.reset
    Dnsruby::Dnssec.anchor_verifier.add_trust_anchor(@b)
    Dnsruby::Dnssec.root_verifier.add_trust_anchor(@a)
    limits = Dnsruby::Dnssec.validation_limits.merge(max_signature_verifications: 1)
    Dnsruby::Dnssec.stub(:validation_limits, limits) do
      assert_raises(Dnsruby::ValidationLimitError) do
        Dnsruby::Dnssec.verify_rrset(signed(@record, @a, 0))
      end
    end
    assert_nil(Dnsruby::ValidationBudget.current)
    assert_nil(Dnsruby::QueryControl.current)
  ensure
    Dnsruby::Dnssec.reset
  end

  def test_public_rrset_fallback_does_not_swallow_cancellation
    visited = []
    stopped = lambda do |*|
      visited << :anchor
      Dnsruby::ValidationBudget.current.cancel
      raise Dnsruby::VerifyError, 'No usable local key'
    end
    root = ->(*) { visited << :root; true }
    Dnsruby::Dnssec.anchor_verifier.stub(:verify_rrset, stopped) do
      Dnsruby::Dnssec.root_verifier.stub(:verify_rrset, root) do
        assert_raises(Dnsruby::ValidationCancelled) do
          Dnsruby::Dnssec.verify_rrset(signed(@record, @a, 0))
        end
      end
    end
    assert_equal([:anchor], visited)
    assert_nil(Dnsruby::ValidationBudget.current)
  end
end

class DnssecCacheLifetimeTest
  def test_learned_dnskey_capacity_also_bounds_provenance_and_keeps_configured_anchors
    verifier = new_verifier
    verifier.instance_variable_set(:@trusted_keys, Dnsruby::KeyCache.new(max_keys: 2))
    verifier.add_trust_anchor(@parent)
    at(0) do
      3.times do |i|
        key = @child.clone
        key.name = "child#{i}.cache.invalid."
        assert(verifier.verify(signed(key, key, 1), key))
      end
      assert_equal(2, verifier.trusted_keys.length)
      assert_equal(2, verifier.instance_variable_get(:@trusted_key_provenance).length)
      assert_equal([@parent], verifier.trust_anchors)
    end
  end

  def test_learned_ds_capacity_evicts_complete_rrsets_and_keeps_configured_anchors
    verifier = new_verifier
    verifier.instance_variable_set(:@discovered_ds_store, Dnsruby::DSCache.new(max_records: 4))
    verifier.add_trust_anchor(@parent)
    at(0) do
      3.times do |i|
        key = @child.clone
        key.name = "child#{i}.cache.invalid."
        assert(verifier.verify(signed(Dnsruby::RR::DS.from_key(key, 2), @parent, 0)))
      end
      assert_equal(['child1.cache.invalid', 'child2.cache.invalid'], verifier.trusted_keys.map { |ds| ds.name.to_s })
      assert_equal([@parent], verifier.trust_anchors)
    end
  end

  def test_unrelated_ds_rrsets_do_not_consume_candidate_budget
    verifier = new_verifier
    verifier.add_trust_anchor(@parent)
    at(0) do
      24.times do |i|
        key = @child.clone
        key.name = "child#{i}.cache.invalid."
        assert(verifier.verify(signed(Dnsruby::RR::DS.from_key(key, 2), @parent, 0)))
      end
      assert(verifier.verify(signed(@ds, @parent, 0)))
      budget = Dnsruby::ValidationBudget.new(limits: Dnsruby::Dnssec.validation_limits.merge(max_work: 16))
      Dnsruby::ValidationBudget.with(budget) { assert(verifier.verify(signed(@child, @child, 1))) }
    end
  end

  def test_timed_ds_and_dlv_anchors_expire_and_invalidate_dependent_trust
    ['DS', 'DLV'].each do |type|
      verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::DLV)
      anchor = Dnsruby::RR.create(@ds.to_s.sub(' DS ', " #{type} "))
      verifier.add_trust_anchor_with_expiration(anchor, @now.to_i + 5)
      verifier.add_trust_anchor(@parent)
      identity = at(0) do
        assert(verifier.verify(signed(@child, @child, 1)))
        assert_cached_child(verifier)
        verifier.validation_cache_identity
      end
      at(5) do
        assert_raises(Dnsruby::VerifyError) { verifier.verify(signed_child_answer) }
        assert_equal([@parent], verifier.trust_anchors)
        assert_empty(verifier.trusted_keys)
        refute_equal(identity, verifier.validation_cache_identity)
      end
    end
  end

  def test_removing_equivalent_ds_anchor_ignores_ttl_and_clears_learned_keys
    verifier = new_verifier
    verifier.add_trust_anchor_with_expiration(@ds, @now.to_i + 60)
    equivalent = @ds.clone
    equivalent.ttl += 1
    at(0) do
      assert(verifier.verify(signed(@child, @child, 1)))
      assert_equal(@ds, verifier.remove_trust_anchor(equivalent))
      assert_empty(verifier.trust_anchors)
      assert_empty(verifier.trusted_keys)
    end
  end

  def test_ds_cache_retains_complete_rrsets_for_digest_preference
    # SHA-256 anywhere in this DS RRset suppresses SHA-1, even if only the
    # SHA-1 key tag matches the DNSKEY being checked (RFC 4509 section 3).
    sha1 = @ds.clone
    sha1.digest_type = 1
    sha1.digestbin = sha1.digest_key(@child)
    sha1.digest = sha1.digestbin.unpack1('H*')
    other = @ds.clone
    other.key_tag = (@ds.key_tag + 1) % 65_536
    verifier = new_verifier
    verifier.add_trust_anchor(sha1)
    verifier.add_trust_anchor(other)
    at(0) do
      assert_raises(Dnsruby::VerifyError) { verifier.verify(signed(@child, @child, 1)) }
    end
  end
end
