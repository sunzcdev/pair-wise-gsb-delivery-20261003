require_relative 'spec_helper'

class FinalReviewFixesTest < Minitest::Test
  def test_source_port_replacement_is_published_after_enumeration
    sender = Dnsruby::PacketSender.new(server: '127.0.0.1', src_port: 60_001)
    ports = Object.new
    seen = []
    ports.define_singleton_method(:each) do |&block|
      [60_002, 60_003].each do |port|
        seen << sender.get_next_src_port
        block.call(port)
      end
    end
    sender.src_port = ports
    assert_equal([60_001, 60_001], seen)
    assert_equal([60_002, 60_003], sender.src_port)
    assert_raises(ArgumentError) { sender.src_port = [] }
    assert_equal([60_002, 60_003], sender.src_port)
    assert_raises(ArgumentError) { sender.src_port = [60_004, 53] }
    assert_equal([60_002, 60_003], sender.src_port)
    sender.src_port << 60_004
    assert_equal([60_002, 60_003, 60_004], sender.src_port)
  end

  def test_compression_preserves_label_boundaries_and_case
    names = ['a\\.b.example.', 'a.b.example.', 'A.B.example.', 'other.example.']
    message = Dnsruby::Message.new
    names.each { |name| message.add_question(name, 'A') }
    decoded = Dnsruby::Message.decode(message.encode)
    assert_equal(names.map { |name| Dnsruby::Name.create(name).labels.map(&:string) },
      decoded.question.map { |question| question.qname.labels.map(&:string) })
  end

  def test_txt_round_trip_preserves_utf8_and_decimal_escape_octets
    ['café', 'é' * 127, '"café" "\\255"'].each do |text|
      record = Dnsruby::RR.create(name: 'text.example.', type: 'TXT', strings: text)
      unless text.start_with?('"')
        assert_equal(text, record.data)
        assert_equal(text.encoding, record.data.encoding)
      end
      message = Dnsruby::Message.new
      message.add_answer(record)
      decoded = Dnsruby::Message.decode(message.encode).answer.first
      assert_equal(record.strings.map(&:b), decoded.strings)
      reparsed = Dnsruby::RR.create(decoded.to_s)
      assert_equal(decoded.strings, reparsed.strings)
    end
    commented = Dnsruby::RR.create(name: 'text.example.', type: 'TXT', strings: '"café" ; comment')
    assert_equal('café', commented.data)
    assert_equal(Encoding::UTF_8, commented.data.encoding)
    record = Dnsruby::RR.create(name: 'text.example.', type: 'TXT', strings: 'é' * 128)
    assert_equal('é' * 128, record.data)
    message = Dnsruby::Message.new
    message.add_answer(record)
    assert_raises(Dnsruby::EncodeError) { message.encode }
  end

  def test_txt_can_split_a_utf8_sequence_at_a_wire_string_boundary
    text = 'é' * 128
    presentation = '"'.b + text.b.byteslice(0, 255) + '" "' + text.b.byteslice(255, 1) + '"'
    record = Dnsruby::RR.create(name: 'text.example.', type: 'TXT', strings: presentation)
    assert_equal(text.b, record.data)
    assert_equal([255, 1], record.strings.map(&:bytesize))
    message = Dnsruby::Message.new
    message.add_answer(record)
    decoded = Dnsruby::Message.decode(message.encode).answer.first
    assert_equal(text.b, decoded.data)
    assert_equal(record.strings, decoded.strings)
  end

  def test_ds_family_preserves_private_algorithm_and_unknown_digest_numbers
    %w[DS CDS DLV].each do |type|
      record = Dnsruby::RR.create("example. 60 IN #{type} 12345 253 253 01020304")
      message = Dnsruby::Message.new
      message.add_answer(record)
      decoded = Dnsruby::Message.decode(message.encode).answer.first
      assert_equal(253, decoded.algorithm.code)
      assert_equal(253, decoded.digest_type.code)
      assert_equal(record.digestbin, decoded.digestbin)
      assert_raises(Dnsruby::DecodeError) { record.algorithm = 256 }
      assert_raises(Dnsruby::DecodeError) { record.digest_type = -1 }
    end
  end

  def test_zone_reader_qualifies_relative_origins_soa_and_literal_final_dots
    reader = Dnsruby::ZoneReader.new('example.', 60)
    reader.process_line('$ORIGIN sub')
    soa = Dnsruby::RR.create(reader.process_line('@ IN SOA ns hostmaster 1 60 60 60 60'))
    assert_equal('sub.example.', soa.name.to_s(true))
    assert_equal('ns.sub.example.', soa.mname.to_s(true))
    assert_equal('hostmaster.sub.example.', soa.rname.to_s(true))
    record = Dnsruby::RR.create(reader.process_line('owner\\. IN CNAME target\\.'))
    assert_equal(['owner.', 'sub', 'example'], record.name.labels.map(&:string))
    assert_equal(['target.', 'sub', 'example'], record.cname.labels.map(&:string))
    reader.process_line('$ORIGIN elsewhere.')
    record = Dnsruby::RR.create(reader.process_line('@ IN NS ns'))
    assert_equal('elsewhere.', record.name.to_s(true))
    assert_equal('ns.elsewhere.', record.nsdname.to_s(true))
  end

  def test_zone_reader_preserves_backslashes_when_replacing_names
    origin = Dnsruby::Name.new(['slash\\origin', 'example'].map { |label| Dnsruby::Name::Label.new(label) }, true)
    reader = Dnsruby::ZoneReader.new(origin, 60)
    target = Dnsruby::Name.new([Dnsruby::Name::Label.new('label\\&')], false)
    record = Dnsruby::RR.create(reader.process_line("#{target.to_s} IN CNAME @"))
    assert_equal(target.labels + origin.labels, record.name.labels)
    assert_equal(origin, record.cname)
    record = Dnsruby::RR.create(reader.process_line('@ IN NS ns'))
    assert_equal(origin, record.name)
  end

  def test_positive_cache_scope_separates_upstream_and_request_context
    sender = Dnsruby::PacketSender.new(server: '192.0.2.53', dnssec: false)
    query = Dnsruby::Message.new('answer.example.', 'A')
    sender.make_query_packet(query)
    scope = sender.positive_cache_scope(query)
    query.header.id = (query.header.id + 1) % 65_536
    assert_equal(scope, sender.positive_cache_scope(query))
    cache = Dnsruby::Cache.new
    response = Dnsruby::Message.new('answer.example.', 'A')
    response.add_answer(Dnsruby::RR.create('answer.example. 60 IN A 192.0.2.1'))
    cache.add(response, scope: scope)
    assert(cache.find('answer.example.', 'A', scope: sender.positive_cache_scope(query)).cached)
    sender.port = 5353
    assert_nil(cache.find('answer.example.', 'A', scope: sender.positive_cache_scope(query)))
    sender.port = 53
    sender.server = '192.0.2.54'
    assert_nil(cache.find('answer.example.', 'A', scope: sender.positive_cache_scope(query)))
    sender.server = '192.0.2.53'
    query.get_opt.dnssec_ok = true
    assert_nil(cache.find('answer.example.', 'A', scope: sender.positive_cache_scope(query)))
  end

  def test_truncated_answers_are_never_cached
    cache = Dnsruby::Cache.new
    response = Dnsruby::Message.new('answer.example.', 'A')
    response.add_answer(Dnsruby::RR.create('answer.example. 60 IN A 192.0.2.1'))
    response.header.tc = true
    assert_nil(cache.add(response))
    assert_nil(cache.find('answer.example.', 'A'))
    response.header.tc = false
    refute_nil(cache.add(response))
  end

  def test_referral_keeps_one_enclosing_delegation_and_sibling_glue
    recursor = Dnsruby::Recursor.allocate
    response = Dnsruby::Message.new('www.child.example.', 'A')
    response.add_authority(Dnsruby::RR.create('child.example. 60 IN NS ns.sibling.example.'))
    response.add_authority(Dnsruby::RR.create('other.example. 10 IN NS ns.other.example.'))
    response.add_additional(Dnsruby::RR.create('ns.sibling.example. 120 IN A 192.0.2.53'))
    response.add_additional(Dnsruby::RR.create('ns.other.example. 120 IN A 192.0.2.54'))
    zone, authorities, ttl = recursor.referral_authorities(response, 'example.', 'www.child.example.', 'IN')
    assert_equal('child.example.', zone)
    assert_equal(['ns.sibling.example.'], authorities.keys)
    assert_equal(['192.0.2.53'], authorities.values.first.values)
    assert_equal(60, ttl)
    relative = recursor.referral_authorities(response, 'example.', 'www.child.example', 'IN')
    assert_equal(zone, relative.first)
    at_delegation = recursor.referral_authorities(response, 'example.', 'child.example', 'IN')
    assert_equal(zone, at_delegation.first)
    assert_nil(recursor.referral_authorities(response, 'child.example.', 'www.child.example.', 'IN'))
    assert_nil(recursor.referral_authorities(response, 'example.', 'www.unrelated.example.', 'IN'))
    response.authority.clear
    response.add_authority(Dnsruby::RR.create('child.example. 60 IN SOA ns.child.example. hostmaster.child.example. 1 60 60 60 60'))
    assert_nil(recursor.referral_authorities(response, 'example.', 'www.child.example.', 'IN'))
  end
end
