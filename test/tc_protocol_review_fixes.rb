require_relative 'spec_helper'

class ProtocolReviewFixesTest < Minitest::Test
  def test_repeated_compressed_names_use_independent_mutable_labels
    encoder = Dnsruby::MessageEncoder.new
    %w[First.Example. Second.Example. First.Example.].each do |name|
      encoder.put_name(Dnsruby::Name.create(name))
    end
    decoder = Dnsruby::MessageDecoder.new(encoder.to_s)
    names = 3.times.map { decoder.get_name }
    assert_equal(%w[first.example. second.example. first.example.], names.map { |name| name.to_s(true) })
    names.first.labels.first.string.replace('Changed')
    names.first.labels.last.string.replace('Changed')
    assert_equal('first.example.', names.last.to_s(true))
    assert_equal('second.example.', names[1].to_s(true))
    suffixes = decoder.instance_variable_get(:@name_suffixes)
    assert(suffixes.keys.all? { |offset| offset < 0x4000 && offset < encoder.to_s.bytesize })
    assert(suffixes.values.all? { |value| value.frozen? && value[0].frozen? && value[0].all?(&:frozen?) })
    assert_equal(encoder.to_s.bytesize, decoder.index)
  end

  def test_rfc_name_fields_are_uncompressed_with_existing_case_rules
    records = [
      ['SRV 0 5 443 Target.Example.', 6],
      ['DNAME Target.Example.', 0],
      ['RRSIG A 8 2 300 20300101000000 20200101000000 123 Target.Example. AQID', 18],
      ['NSEC Target.Example. A NSEC RRSIG', 0]
    ]
    target = Dnsruby::Name.create('Target.Example.')
    expected = Dnsruby::MessageEncoder.new { |msg| msg.put_name(target, true, false) }.to_s
    records.each do |rdata, name_offset|
      rr = Dnsruby::RR.create("owner.example. 300 IN #{rdata}")
      encoder = Dnsruby::MessageEncoder.new
      encoder.put_name(target)
      encoder.put_rr(rr)
      decoder = Dnsruby::MessageDecoder.new(encoder.to_s)
      decoder.get_name
      decoder.get_name
      decoder.get_unpack('nnN')
      wire_rdata = decoder.get_length16 { decoder.get_bytes }
      assert_equal(expected, wire_rdata.byteslice(name_offset, expected.bytesize), rdata)
      decoded = Dnsruby::MessageDecoder.new(encoder.to_s)
      decoded.get_name
      assert_equal(rr.clone, decoded.get_rr, rdata)
    end
  end

  def test_all_known_empty_update_markers_round_trip_and_clone
    types = %w[A AAAA AFSDB ANY APL CAA CDNSKEY CDS CERT CNAME DHCID DLV DNAME DNSKEY DS
      GPOS HINFO HIP HTTPS IPSECKEY ISDN KX LOC MB MG MINFO MR MX NAPTR NS NSAP NSEC NSEC3
      NSEC3PARAM NXT PTR PX RP RRSIG RT SOA SPF SRV SSHFP SVCB TLSA TXT URI WKS X25 TYPE65400]
    types.each do |type|
      update = Dnsruby::Update.new('example.')
      records = [update.present('owner.example.', type), update.absent('owner.example.', type),
        update.delete('owner.example.', type)]
      decoded = Dnsruby::Message.decode(update.encode)
      actual = decoded.answer.to_a + decoded.authority.to_a
      assert_equal(3, actual.length, type)
      records.zip(actual).each do |record, copy|
        assert_equal(record.type, copy.type, type)
        assert_equal(record.klass, copy.klass, type)
        assert_equal(0, copy.rdlength, type)
        assert_equal('', copy.rdata_to_string, type)
        assert_equal(0, copy.clone.rdlength, type)
        assert_equal(record.class.superclass, copy.class.superclass, type)
      end
      assert_equal('', Dnsruby::RR.create("owner.example. 0 ANY #{type}").rdata_to_string, type)
    end
  end

  def test_value_dependent_update_records_keep_their_data
    update = Dnsruby::Update.new('example.')
    update.present('owner.example.', 'MX', '10 mail.example.')
    update.delete('owner.example.', 'MX', '10 mail.example.')
    decoded = Dnsruby::Message.decode(update.encode)
    assert_equal(Dnsruby::Classes::IN, decoded.answer.first.klass)
    assert_equal(Dnsruby::Classes::NONE, decoded.authority.first.klass)
    assert_equal('10 mail.example.', decoded.answer.first.rdata_to_string)
    assert_equal('10 mail.example.', decoded.authority.first.rdata_to_string)
  end

  def test_escaped_owner_tokens_survive_zone_normalization
    reader = Dnsruby::ZoneReader.new('example.', 300)
    { 'space\\ name' => 'space name', 'trailing\\ ' => 'trailing ', 'quote\\"name' => 'quote"name',
      'back\\\\slash' => 'back\\slash' }.each do |owner, label|
      rr = Dnsruby::RR.create(reader.process_line("#{owner} IN TXT \"ordinary text\""))
      assert_equal([label, 'example'], rr.name.labels.map(&:string))
      assert_equal(['ordinary text'], rr.strings)
      omitted = Dnsruby::RR.create(reader.process_line('  IN A 192.0.2.1'))
      assert_equal(rr.name, omitted.name)
    end
    rr = Dnsruby::RR.create('space\\ name.example. 300 IN A 192.0.2.1')
    assert_equal(['space name', 'example'], rr.name.labels.map(&:string))
  end
end
