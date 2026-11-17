require_relative 'spec_helper'

class ReleaseZoneDefaultsTest < Minitest::Test
  def record(reader, line)
    Dnsruby::RR.create(reader.process_line(line))
  end

  def test_ttl_directive_survives_explicit_record_ttls_and_origin_changes
    reader = Dnsruby::ZoneReader.new('example.', 900)
    reader.process_line('$TTL 1h')
    assert_equal(60, record(reader, 'one 60 IN A 192.0.2.1').ttl)
    assert_equal(3600, record(reader, 'two IN A 192.0.2.2').ttl)
    assert_equal(120, record(reader, 'three IN 2m A 192.0.2.3').ttl)
    reader.process_line('$ORIGIN sub')
    assert_equal(3600, record(reader, 'four IN A 192.0.2.4').ttl)
    reader.process_line('$TTL 0')
    assert_equal(60, record(reader, 'five 60 IN A 192.0.2.5').ttl)
    assert_equal(0, record(reader, 'six IN A 192.0.2.6').ttl)
  end

  def test_previous_ttl_inheritance_remains_available_without_a_directive
    reader = Dnsruby::ZoneReader.new('example.', 900)
    assert_equal(900, record(reader, 'one IN A 192.0.2.1').ttl)
    record(reader, 'two 60 IN A 192.0.2.2')
    assert_equal(60, record(reader, 'three IN A 192.0.2.3').ttl)
  end

  def test_single_quoted_text_survives_both_header_orders_and_omitted_owner
    %w[TXT SPF TYPE16 TYPE99].each do |type|
      reader = Dnsruby::ZoneReader.new('example.')
      ["www IN 1h #{type} 'one  ; (two)'", "www 1h IN #{type} 'one  ; (two)'",
       "  CLASS1 3600 #{type} 'one  ; (two)'"].each do |line|
        rr = record(reader, line)
        assert_equal(['one  ; (two)'], rr.strings)
        assert_equal(3600, rr.ttl)
      end
    end
  end

  def test_single_quoted_continuation_and_double_quoted_text
    reader = Dnsruby::ZoneReader.new('example.')
    assert_nil(reader.process_line("www IN 60 TXT ( 'one ;'\n"))
    rr = record(reader, " 'two (three)' )")
    assert_equal(['one ;', 'two (three)'], rr.strings)
    assert_equal(['one  ; (two)'], record(reader, 'www IN 1h TXT "one  ; (two)"').strings)
  end

  def test_rdata_domain_names_are_qualified_against_the_origin
    reader = Dnsruby::ZoneReader.new('example.', 60)
    examples = {
      'KX 10 mail' => :exchange,
      'NSEC next A NSEC RRSIG' => :next_domain,
      'NXT next A NXT' => :next_domain,
      'RRSIG A 8 2 60 20300101000000 20200101000000 1234 signer AQID' => :signers_name,
      'IPSECKEY 10 3 2 gateway AQID' => :gateway
    }
    examples.each do |rdata, field|
      rr = record(reader, "host IN #{rdata}")
      assert(rr.public_send(field).absolute?, rdata)
      assert(rr.public_send(field).subdomain_of?(Dnsruby::Name.create('example.')), rdata)
      message = Dnsruby::Message.new
      message.add_answer(rr)
      decoded = Dnsruby::Message.decode(message.encode).answer.first
      assert_equal(rr.public_send(field), decoded.public_send(field), rdata)
    end
    hip = record(reader, 'host IN HIP 2 00112233 AQID rendezvous other.example.net.')
    assert_equal(['rendezvous.example.', 'other.example.net.'], hip.rsvs.map { |name| name.to_s(true) })
    ipv4 = record(reader, 'host IN IPSECKEY 10 1 2 192.0.2.1 AQID')
    assert_equal('192.0.2.1', ipv4.gateway.to_s)
  end

  def test_relative_rdata_names_preserve_escaped_final_dots
    reader = Dnsruby::ZoneReader.new('example.', 60)
    rr = record(reader, 'host IN NSEC next\\. A NSEC RRSIG')
    assert_equal(['next.', 'example'], rr.next_domain.labels.map(&:string))
    rr = record(reader, 'host IN KX 10 mail\\.')
    assert_equal(['mail.', 'example'], rr.exchange.labels.map(&:string))
  end

  def test_compression_snapshots_mutable_label_strings
    text = +'before'
    name = Dnsruby::Name.new([Dnsruby::Name::Label.new(text), Dnsruby::Name::Label.new('example')], true)
    encoder = Dnsruby::MessageEncoder.new
    encoder.put_name(name)
    text.replace('after')
    encoder.put_name(name)
    encoder.put_name(Dnsruby::Name.create('before.example.'))
    decoder = Dnsruby::MessageDecoder.new(encoder.to_s)
    assert_equal(['before.example.', 'after.example.', 'before.example.'],
      3.times.map { decoder.get_name.to_s(true) })
  end

  def test_generic_rdata_bypasses_all_type_specific_name_and_interval_rewrites
    examples = [
      'MX 10 Mail.Other.', 'KX 10 Mail.Other.', 'NS Ns.Other.',
      'AFSDB 1 Host.Other.', 'NAPTR 10 20 "s" "SIP+D2U" "" Replacement.Other.',
      'RT 10 Host.Other.', 'SRV 0 5 5060 Service.Other.', 'CNAME Alias.Other.',
      'MB Mail.Other.', 'MG Mail.Other.', 'MR Mail.Other.', 'PTR Pointer.Other.',
      'DNAME Target.Other.',
      'SOA Ns.Other. Hostmaster.Other. 1 3600 600 86400 300',
      'MINFO One.Other. Two.Other.', 'PX 10 One.Other. Two.Other.',
      'RP Mailbox.Other. Text.Other.', 'NSEC Next.Other. A NSEC RRSIG',
      'NXT Next.Other. A NXT',
      'RRSIG NSEC 8 2 60 20300101000000 20200101000000 1234 Signer.Other. AQID',
      'IPSECKEY 10 3 2 Gateway.Other. AQID',
      'HIP 2 00112233 AQID Rendezvous.Other.',
      'SVCB 1 Target.Other. alpn=h2', 'HTTPS 1 Target.Other. port=443'
    ]
    examples.each do |presentation|
      original = Dnsruby::RR.create("owner.origin. 60 IN #{presentation}")
      bytes = rdata_bytes(original)
      generic = "\\# #{bytes.bytesize} #{bytes.unpack1('H*').upcase}"
      [original.type.to_s, "TYPE#{original.type.code}"].each do |type|
        reader = Dnsruby::ZoneReader.new('origin.')
        normalized = reader.process_line("owner IN 1m #{type} #{generic}")
        assert_equal(generic, normalized.split(/\s+/, 5).last.strip, presentation)
        parsed = Dnsruby::RR.create(normalized)
        assert_equal('owner.origin.', parsed.name.to_s(true), presentation)
        assert_equal(60, parsed.ttl, presentation)
        assert_equal(bytes, rdata_bytes(parsed), presentation)
      end
    end
  end

  def test_generic_rdata_preserves_zone_defaults_continuations_and_prefix_contract
    original = Dnsruby::RR.create('owner.origin. 60 IN NSEC Next.Other. A NSEC RRSIG')
    bytes = rdata_bytes(original)
    hex = bytes.unpack1('H*')
    reader = Dnsruby::ZoneReader.new('origin.')
    reader.process_line('$TTL 2h')
    assert_nil(reader.process_line("owner IN 1m TYPE47 ( \\# #{bytes.bytesize} #{hex[0, 8]}"))
    explicit = record(reader, "#{hex[8..]} )")
    inherited = record(reader, "  IN TYPE47 \\# #{bytes.bytesize} #{hex}")
    assert_equal([60, 7200], [explicit.ttl, inherited.ttl])
    assert_equal('owner.origin.', inherited.name.to_s(true))
    assert_equal(bytes, rdata_bytes(explicit))
    assert_equal(bytes, rdata_bytes(inherited))

    signature = Dnsruby::RR.create('owner.origin. 60 IN RRSIG NSEC 8 2 60 20300101000000 20200101000000 1234 Signer.Other. AQID')
    signature_bytes = rdata_bytes(signature)
    generic = "\\# #{signature_bytes.bytesize} #{signature_bytes.unpack1('H*')}"
    line, prefix, owner = reader.normalise_line("owner 60 IN TYPE46 #{generic}", true)
    assert_equal('ZZNSEC', prefix)
    assert_equal('owner.origin.', owner)
    assert_equal(generic, line.split(/\s+/, 5).last.strip)
    assert_equal(signature_bytes, rdata_bytes(Dnsruby::RR.create(line)))
    empty = record(reader, 'empty IN TYPE65000 \\# 0')
    assert_equal(7200, empty.ttl)
    assert_equal(''.b, rdata_bytes(empty))
  end

  private

  def rdata_bytes(rr)
    Dnsruby::MessageEncoder.new { |encoder| rr.encode_rdata(encoder, false) }.to_s
  end
end
