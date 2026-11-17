# frozen_string_literal: true

require_relative 'spec_helper'

class TestRecordCodecContracts < Minitest::Test
  include Dnsruby

  def copy_record(record)
    message = Message.new
    message.add_answer(record)
    Message.decode(message.encode).answer.first
  end

  def test_received_ttl_normalisation_leaves_ordinary_values_and_opt_fields_intact
    [0, 300, 0x7fffffff].each do |ttl|
      rr = RR.create(name: 'host.example.', type: 'A', address: '192.0.2.1', ttl: ttl)
      assert_equal(ttl, copy_record(rr).ttl)
    end
    [0x80000000, 0xffffffff].each do |ttl|
      rr = RR.create(name: 'host.example.', type: 'A', address: '192.0.2.1', ttl: ttl)
      assert_equal(0, copy_record(rr).ttl)
    end
    opt = RR::OPT.new(1232, 0x80008000)
    encoder = MessageEncoder.new { |out| out.put_rr(opt) }
    assert_equal(opt.ttl, MessageDecoder.new(encoder.to_s).get_rr.ttl)
  end

  def test_a_root_name_requires_an_encoded_root_label
    assert_equal('.', MessageDecoder.new("\0".b).get_name.to_s(true))
    assert_raises(DecodeError) { MessageDecoder.new(''.b).get_name }
    %w[NS CNAME PTR].each do |type|
      assert_equal('.', copy_record(RR.create("owner.example. IN #{type} .")).domainname.to_s(true))
    end
    assert_equal('.', copy_record(RR.create('owner.example. IN MX 0 .')).exchange.to_s(true))
    assert_equal('.', copy_record(RR.create('owner.example. IN SRV 0 0 0 .')).target.to_s(true))
    assert_equal('.', copy_record(RR.create('owner.example. IN NAPTR 1 1 "" "" "" .')).replacement.to_s(true))
  end

  def test_empty_name_only_update_markers_keep_their_type_and_roundtrip
    update = Update.new('example.')
    %w[NS CNAME PTR].each do |type|
      [update.present('owner.example.', type), update.absent('owner.example.', type),
        update.delete('owner.example.', type)].each do |rr|
        assert_equal(rr.type, rr.clone.type)
        assert_equal(rr.klass, rr.clone.klass)
      end
    end
    copy = Message.decode(update.encode)
    assert_equal(update.encode, copy.encode)
    assert_equal(update.answer.map(&:type), copy.answer.map(&:type))
    assert_equal(update.authority.map(&:type), copy.authority.map(&:type))
    %w[ANY NONE].each do |klass|
      rr = RR.new_from_data('owner.example.', Types::NS, klass, 0, 0, ''.b, 0)
      assert_equal(Classes.new(klass), rr.klass)
      assert_equal(0, rr.domainname.length)
    end
  end

  def test_rrsig_algorithm_octets_preserve_private_values
    [8, 253, 254].each do |algorithm|
      rr = RR.create(name: 'example.', type: 'RRSIG', type_covered: 'A',
        algorithm: algorithm, labels: 1, original_ttl: 300, expiration: 2_000_000_000,
        inception: 1_900_000_000, key_tag: 1234, signers_name: 'example.', signature: 'signature')
      copy = copy_record(rr)
      assert_equal(algorithm, copy.algorithm.code)
      assert_equal(rr.sig_data, copy.sig_data)
      assert_equal(rr, copy)
    end
  end

  def test_caa_accepts_empty_values_extension_tags_and_unquoted_values
    ['0 issue ""', '0 custom123 "opaque value"', '128 FutureTag token',
      '0 issue ca.example', '0 issue "ca.example; account=123"'].each do |rdata|
      rr = RR.create("example. IN CAA #{rdata}")
      copy = copy_record(rr)
      assert_equal(rr, copy)
      assert_equal(rr, RR.create(rr.to_s))
      assert_equal(rr.property_value, copy.property_value)
    end
  end

  def test_caa_escapes_preserve_binary_values_and_long_strings
    rr = RR.create('example. IN CAA 0 custom "quote\"slash\\\\tab\009byte\255"')
    assert_equal("quote\"slash\\tab\tbyte\xff".b, rr.property_value)
    assert_equal(rr, copy_record(rr))
    assert_equal(rr, RR.create(rr.to_s))
    long = RR.create(name: 'example.', type: 'CAA', flag: 0,
      property_tag: 'custom', property_value: 'a' * 300)
    assert_equal(long, copy_record(long))
    assert_equal(long, RR.create(long.to_s))
    %w[bad.tag bad-tag].each do |tag|
      assert_raises(DecodeError) { RR.create("example. IN CAA 0 #{tag} value") }
    end
  end

  def test_ds_family_equality_and_hash_use_the_same_binary_digest
    %w[DS CDS DLV].each do |type|
      lower = RR.create("example. IN #{type} 1234 8 2 #{'ab' * 32}")
      upper = RR.create("example. IN #{type} 1234 8 2 #{'AB' * 32}")
      assert_equal(lower, upper)
      assert_equal(lower.hash, upper.hash)
      assert_equal(:found, { lower => :found }[upper])
      different = RR.create("example. IN #{type} 1234 8 2 #{'cd' * 32}")
      refute_equal(lower, different)
    end
  end
end
