require_relative 'spec_helper'

class ReviewRecordCompatibilityTest < Minitest::Test
  def test_decoder_byte_access_keeps_direct_multibyte_and_mutation_contracts
    text = +'AA'
    decoder = Dnsruby::MessageDecoder.new(text)
    assert_equal(65, decoder.get_byte_at(1))
    text.replace('éA')
    assert_equal(195, decoder.get_byte_at(0))
    assert_equal(65, decoder.get_byte_at(1))
    assert_nil(decoder.get_byte_at(2))
    binary = Dnsruby::MessageDecoder.new(text.b)
    assert_equal([195, 169, 65], 3.times.map { |index| binary.get_byte_at(index) })
    assert_nil(binary.get_byte_at(3))
  end

  def test_caa_clone_preserves_string_rdata_and_comparisons
    record = Dnsruby::RR.create('example. 300 IN CAA 128 issue "ca.example"')
    copy = record.clone
    assert_instance_of(String, copy.rdata)
    assert_equal(record.rdata, copy.rdata)
    assert_equal(0, record <=> copy)
    assert_equal(2, [record, copy].sort.length)
    copy.property_value = 'other.example'
    assert_equal('128 issue "ca.example"', copy.rdata)
    assert_equal('128 issue "other.example"', copy.rdata_to_string)
  end

  def test_caa_binary_value_round_trips_without_presentation_reparsing
    value = "\0\"\\\xFF".b
    record = Dnsruby::RR.create(name: 'example.', type: 'CAA', klass: 'IN', ttl: 300,
      flag: 0, property_tag: 'issue', property_value: value)
    copy = record.clone
    assert_equal(value, copy.property_value)
    assert_instance_of(String, copy.rdata)
    assert_equal(copy.rdata_to_string, copy.rdata)
    assert_equal(value, copy.clone.property_value)
  end

  def test_escaped_origin_and_rdata_names_remain_complete
    reader = Dnsruby::ZoneReader.new('example.')
    reader.process_line('$ORIGIN foo\\ bar.example.')
    address = Dnsruby::RR.create(reader.process_line('host 60 IN A 192.0.2.1'))
    assert_equal(['host', 'foo bar', 'example'], address.name.labels.map(&:string))

    mx = Dnsruby::RR.create(reader.process_line('mail 60 IN MX 10 target\\ name'))
    assert_equal(['target name', 'foo bar', 'example'], mx.exchange.labels.map(&:string))
    nsec = Dnsruby::RR.create(reader.process_line('owner 60 IN NSEC next\\ name A NSEC RRSIG'))
    assert_equal(['next name', 'foo bar', 'example'], nsec.next_domain.labels.map(&:string))
    assert_equal(%w[A NSEC RRSIG], nsec.types.map(&:to_s))

    direct_mx = Dnsruby::RR.create('mail.example. 60 IN MX 10 target\\ name.example.')
    direct_nsec = Dnsruby::RR.create('owner.example. 60 IN NSEC next\\ name.example. A NSEC')
    assert_equal(['target name', 'example'], direct_mx.exchange.labels.map(&:string))
    assert_equal(['next name', 'example'], direct_nsec.next_domain.labels.map(&:string))
  end

  def test_escaped_trailing_whitespace_in_zone_names
    reader = Dnsruby::ZoneReader.new('example.')
    rr = Dnsruby::RR.create(reader.process_line('alias 60 IN CNAME trailing\\ '))
    assert_equal(['trailing ', 'example'], rr.domainname.labels.map(&:string))
  end

  def test_zone_txt_uses_dns_escapes_while_direct_parser_keeps_legacy_behavior
    reader = Dnsruby::ZoneReader.new('example.')
    %w[TXT SPF].each do |type|
      rr = Dnsruby::RR.create(reader.process_line("owner 60 IN #{type} \"one\\ntwo\\tthree\\032four\\010five\""))
      assert_equal(["onentwotthree four\nfive"], rr.strings)
      quoted = Dnsruby::RR.create(reader.process_line("owner 60 IN #{type} \"say\\\"hi\\\"\\\\end\""))
      assert_equal(['say"hi"\\end'], quoted.strings)
      unquoted = Dnsruby::RR.create(reader.process_line("owner 60 IN #{type} one\\ two"))
      assert_equal(['one two'], unquoted.strings)
    end
    assert_equal(["one\ntwo"], Dnsruby::RR::TXT.parse('"one\\ntwo"'))
  end

  def test_canonical_sort_keeps_references_and_normalizes_mutated_ttls
    records = [3, 1, 2].map { |last| Dnsruby::RR.create("example. 300 IN A 192.0.2.#{last}") }
    set = Dnsruby::RRSet.new(records)
    originals = set.rrs
    originals[0].ttl = 100
    sorted = set.sort_canonical
    assert_equal(%w[192.0.2.1 192.0.2.2 192.0.2.3], sorted.rrs.map { |rr| rr.address.to_s })
    sorted.rrs.each { |rr| assert_same(originals.find { |old| old.address == rr.address }, rr) }
    assert_equal([100, 100, 100], originals.map(&:ttl))
    sorted.rrs.first.address = Dnsruby::IPv4.create('192.0.2.9')
    assert(sorted.add(Dnsruby::RR.create('example. 100 IN A 192.0.2.1'), false))
    assert_equal(4, sorted.length)
  end

  def test_canonical_sort_deduplicates_publicly_mutated_records
    set = Dnsruby::RRSet.new([
      Dnsruby::RR.create('example. 300 IN A 192.0.2.1'),
      Dnsruby::RR.create('example. 300 IN A 192.0.2.2')
    ])
    set.rrs.last.address = Dnsruby::IPv4.create('192.0.2.1')
    sorted = set.sort_canonical
    assert_equal(1, sorted.length)
    assert_same(set.rrs.last, sorted.rrs.first)
  end
end
