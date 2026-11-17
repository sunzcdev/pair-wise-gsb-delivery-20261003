require_relative 'spec_helper'

class RRSetGroupingTest < Minitest::Test
  def test_grouping_preserves_order_class_and_signature_membership
    signature = Dnsruby::RR.create('Host.Example. 300 IN RRSIG A 8 2 300 20300101000000 20200101000000 123 example. AQID')
    records = [signature,
      Dnsruby::RR.create('other.example. 300 IN A 192.0.2.2'),
      Dnsruby::RR.create('host.example. 200 IN A 192.0.2.1'),
      Dnsruby::RR.create('host.example. 100 IN A 192.0.2.3'),
      Dnsruby::RR.create('host.example. 300 CH TXT "other class"')]
    section = Dnsruby::Section.new.concat(records)
    sets = section.rrsets
    assert_equal([Dnsruby::Types::A, Dnsruby::Types::A, Dnsruby::Types::TXT], sets.map(&:type).map(&:code))
    assert_equal(1, sets.first.num_sigs)
    assert_equal(2, sets.first.rrs.length)
    assert_equal([100, 100], sets.first.rrs.map(&:ttl))
    assert_equal([200, 100], records[2..3].map(&:ttl), 'Grouping must not mutate source records')
    assert_equal(1, section.rrsets('RRSIG').length)
    assert_equal(2, section.rrsets('A').length)
    assert_equal(0, section.rrsets('AAAA').length)
  end

  def test_grouping_copies_each_record_once
    records = Array.new(40) { |i| Dnsruby::RR.create("host#{i}.example. 300 IN A 192.0.2.1") }
    copies = 0
    records.each do |record|
      original = record.method(:clone)
      record.define_singleton_method(:clone) { copies += 1; original.call }
    end
    sets = Dnsruby::Section.new.concat(records).rrsets
    assert_equal(40, sets.length)
    assert_equal(40, copies)
    assert_equal(records.map(&:name), sets.map(&:name))
  end

  def test_grouping_observes_validation_work_limits
    section = Dnsruby::Section.new.concat(Array.new(3) { |i| Dnsruby::RR.create("host#{i}.example. 300 IN A 192.0.2.1") })
    limits = Dnsruby::ValidationBudget::DEFAULT_LIMITS.merge(max_work: 2)
    budget = Dnsruby::ValidationBudget.new(limits: limits)
    assert_raises(Dnsruby::ValidationLimitError) { Dnsruby::ValidationBudget.with(budget) { section.rrsets } }
  end

  def test_rejected_rrset_add_does_not_clone_the_record
    set = Dnsruby::RRSet.new(Dnsruby::RR.create('one.example. 300 IN A 192.0.2.1'))
    other = Dnsruby::RR.create('two.example. 300 IN A 192.0.2.2')
    other.define_singleton_method(:clone) { raise 'Unexpected clone' }
    refute(set.add(other))
    assert_equal(1, set.length)
  end
end
