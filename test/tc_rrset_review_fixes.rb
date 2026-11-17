require_relative 'spec_helper'

class RRSetReviewFixesTest < Minitest::Test
  def record(address = '192.0.2.1')
    Dnsruby::RR.create("host.example. 300 IN A #{address}")
  end

  def signature
    Dnsruby::RR.create('host.example. 300 IN RRSIG A 8 2 300 20300101000000 20200101000000 123 example. AQID')
  end

  def test_add_set_and_delete_signature_preserve_membership
    source = Dnsruby::RRSet.new([record, signature])
    target = Dnsruby::RRSet.new(record('192.0.2.2'))
    assert(target.add(source))
    assert_equal(3, target.length)
    assert_equal(1, target.num_sigs)
    assert_equal(2, target.rrs.length)
    assert(target.delete(target.sigs.first))
    assert_equal(0, target.num_sigs)
    assert_empty(target.sigs)
    assert_equal(2, target.rrs.length)
    refute(target.add(Dnsruby::RRSet.new))
  end

  def test_bulk_membership_index_is_discarded_before_records_escape
    original = record
    set = Dnsruby::RRSet.new([original, record('192.0.2.2'), original])
    assert_equal(2, set.length)
    assert_nil(set.instance_variable_get(:@membership_index))
    set[0].address = Dnsruby::IPv4.create('192.0.2.3')
    assert(set.add(record))
    assert_equal(3, set.length)
    assert(set.add(record('192.0.2.3')))
    assert_equal(3, set.length)
    assert_equal('192.0.2.1', original.address.to_s)
  end

  def test_add_set_without_cloning_keeps_the_requested_aliasing
    source = Dnsruby::RRSet.new(record)
    target = Dnsruby::RRSet.new
    target.add(source, false)
    assert_same(source[0], target[0])
    source[0].address = Dnsruby::IPv4.create('192.0.2.2')
    target.add(record)
    assert_equal(2, target.length)
  end

  def test_bulk_grouping_deduplicates_and_normalizes_ttls
    records = [record, record('192.0.2.2'), record]
    records.last.ttl = 100
    sets = Dnsruby::Section.new.concat(records).rrsets
    assert_equal(1, sets.length)
    assert_equal(2, sets.first.length)
    assert_equal([100, 100], sets.first.rrs.map(&:ttl))
    assert_equal([300, 300, 100], records.map(&:ttl))
  end
end
