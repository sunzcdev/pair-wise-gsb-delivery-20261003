require_relative 'spec_helper'
require 'minitest/mock'

# Negative cache entries distinguish NXDOMAIN from NODATA, record the
# question scope, the authoritative SOA and the lifetime computed from it,
# and the alias-chain position where non-existence was proved.
class NegativeEntryTest < Minitest::Test
  def setup
    @now = Time.utc(2026, 11, 18, 12)
  end

  def test_entry_records_question_scope_soa_and_computed_lifetime
    entry = build(negative(:NXDOMAIN, ttl: 80, minimum: 15))
    assert_equal(Dnsruby::Name.create('missing.example.'), entry.qname)
    assert_equal(Dnsruby::Types::ANY, entry.qtype)
    assert_equal(Dnsruby::Classes::IN, entry.qclass)
    assert_equal(Dnsruby::Name.create('example.'), entry.soa.name)
    assert_equal(15, entry.lifetime)
    assert_equal(@now.to_i + 15, entry.expiration)
    assert_equal(15, entry.remaining_ttl(@now.to_i))
    assert_equal(7, entry.remaining_ttl(@now.to_i + 8))
    assert_equal(0, entry.remaining_ttl(@now.to_i + 15))
    refute(entry.expired?(@now.to_i + 14))
    assert(entry.expired?(@now.to_i + 15))
  end

  def test_nodata_scope_is_never_widened_into_a_name_error
    entry = build(negative(:NOERROR))
    assert(entry.nodata?)
    refute(entry.name_error?)
    assert_equal(Dnsruby::Types::A, entry.qtype)
    assert(entry.covers?('missing.example.', 'A'))
    refute(entry.covers?('missing.example.', 'AAAA'))
    refute(entry.covers?('missing.example.', 'A', 'CH'))
    refute(entry.covers?('other.example.', 'A'))
    nxdomain = build(negative(:NXDOMAIN))
    assert(nxdomain.name_error?)
    refute(nxdomain.nodata?)
    assert(nxdomain.covers?('missing.example.', 'AAAA'))
    assert(nxdomain.covers?('missing.example.', 'MX'))
    refute(nxdomain.covers?('other.example.', 'A'))
    refute(nxdomain.covers?('missing.example.', 'A', 'CH'))
  end

  def test_alias_chain_nonexistence_position_is_recorded
    response = negative(:NXDOMAIN, name: 'alias.example.')
    response.add_answer(Dnsruby::RR.create('alias.example. 90 IN CNAME missing.example.'))
    entry = build(response)
    assert_equal(Dnsruby::Name.create('missing.example.'), entry.terminal)
    refute(entry.name_error?)
    assert_equal(Dnsruby::Types::A, entry.qtype)
    dname = Dnsruby::Message.new('www.old.example.', 'AAAA')
    dname.add_answer(Dnsruby::RR.create('old.example. 60 IN DNAME new.example.'))
    dname.add_authority(Dnsruby::RR.create('new.example. 60 IN SOA ns.new.example. hostmaster.new.example. 1 60 60 60 30'))
    assert_equal(Dnsruby::Name.create('www.new.example.'), build(dname).terminal)
    bare = build(negative(:NXDOMAIN))
    assert_equal(bare.qname, bare.terminal)
  end

  def test_failure_without_authoritative_basis_gets_no_entry
    response = Dnsruby::Message.new('missing.example.', 'A')
    response.header.rcode = Dnsruby::RCode::NXDOMAIN
    assert_nil(Dnsruby::Cache.negative_proof(response))
    response.add_authority(Dnsruby::RR.create('unrelated. 30 IN SOA ns.unrelated. hostmaster.unrelated. 1 60 60 60 30'))
    assert_nil(Dnsruby::Cache.negative_proof(response))
    cache = Dnsruby::Cache.new
    assert_nil(cache.add(response))
    assert_equal(0, cache.length)
  end

  def test_zone_change_stops_reuse_of_earlier_negatives
    cache = Dnsruby::Cache.new
    cache.add(negative(:NXDOMAIN, serial: 1))
    cache.add(negative(:NOERROR, serial: 1, name: 'other.example.'))
    refute_nil(cache.find('missing.example.', 'AAAA'))
    refute_nil(cache.find('other.example.', 'A'))
    # A newer serial for the same zone retires every older negative.
    cache.add(negative(:NOERROR, serial: 2, name: 'third.example.'))
    assert_nil(cache.find('missing.example.', 'A'))
    assert_nil(cache.find('other.example.', 'A'))
    refute_nil(cache.find('third.example.', 'A'))
  end

  def test_positive_answer_outranks_a_contradictory_negative
    cache = Dnsruby::Cache.new
    cache.add(positive)
    cache.add(negative(:NOERROR, name: 'host.example.'))
    found = cache.find('host.example.', 'A')
    assert_equal('192.0.2.1', found.answer.first.address.to_s)
    cache = Dnsruby::Cache.new
    cache.add(negative(:NOERROR, name: 'host.example.'))
    cache.add(positive)
    found = cache.find('host.example.', 'A')
    assert_equal('192.0.2.1', found.answer.first.address.to_s)
    cache = Dnsruby::Cache.new
    cache.add(negative(:NXDOMAIN, name: 'host.example.'))
    cache.add(positive)
    assert_equal('192.0.2.1', cache.find('host.example.', 'A').answer.first.address.to_s)
    assert_nil(cache.find('host.example.', 'AAAA'))
  end

  def test_name_error_and_nodata_at_one_name_do_not_coexist
    cache = Dnsruby::Cache.new
    cache.add(negative(:NOERROR))
    cache.add(negative(:NXDOMAIN))
    assert_equal(Dnsruby::RCode::NXDOMAIN, cache.find('missing.example.', 'A').rcode)
    cache.add(negative(:NOERROR))
    assert_equal(Dnsruby::RCode::NOERROR, cache.find('missing.example.', 'A').rcode)
    assert_nil(cache.find('missing.example.', 'AAAA'))
  end

  def test_fixed_clock_decides_hit_scope_and_remaining_ttl
    cache = Dnsruby::Cache.new
    Time.stub(:now, @now) { cache.add(negative(:NXDOMAIN, ttl: 80, minimum: 15)) }
    Time.stub(:now, @now + 10) do
      found = cache.find('missing.example.', 'AAAA')
      assert_equal(5, found.authority.first.ttl)
      assert_equal(Dnsruby::Types::AAAA, found.question.first.qtype)
    end
    Time.stub(:now, @now + 15) do
      assert_nil(cache.find('missing.example.', 'AAAA'))
      assert_equal(0, cache.length)
    end
  end

  def test_expired_positive_no_longer_blocks_a_negative
    cache = Dnsruby::Cache.new
    Time.stub(:now, @now) { cache.add(positive(ttl: 5)) }
    Time.stub(:now, @now + 6) do
      cache.add(negative(:NOERROR, name: 'host.example.'))
      found = cache.find('host.example.', 'A')
      assert_equal(Dnsruby::RCode::NOERROR, found.rcode)
      assert_empty(found.answer)
    end
  end

  private

  def build(message)
    proof = Dnsruby::Cache.negative_proof(message)
    refute_nil(proof)
    Dnsruby::Cache::NegativeEntry.new(proof, @now.to_i)
  end

  def negative(rcode, ttl: 80, minimum: 15, name: 'missing.example.', serial: 1, zone: 'example.')
    response = Dnsruby::Message.new(name, 'A')
    response.header.rcode = Dnsruby::RCode.new(rcode.to_s)
    response.add_authority(Dnsruby::RR.create(
      "#{zone} #{ttl} IN SOA ns.#{zone} hostmaster.#{zone} #{serial} 60 60 60 #{minimum}"))
    response
  end

  def positive(ttl: 300)
    Dnsruby::Message.new('host.example.', 'A').tap do |message|
      message.add_answer(Dnsruby::RR.create("host.example. #{ttl} IN A 192.0.2.1"))
    end
  end
end
