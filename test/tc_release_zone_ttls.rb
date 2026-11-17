require_relative 'spec_helper'

class ReleaseZoneTtlsTest < Minitest::Test
  def test_time_suffixes_in_directives_records_and_soa_intervals
    reader = Dnsruby::ZoneReader.new('example.')
    reader.process_line('$TTL 1H30m')
    inherited = Dnsruby::RR.create(reader.process_line('www IN A 192.0.2.1'))
    assert_equal(5400, inherited.ttl)
    explicit = Dnsruby::RR.create(reader.process_line('mail 2h IN A 192.0.2.2'))
    assert_equal(7200, explicit.ttl)
    soa = Dnsruby::RR.create(reader.process_line('@ IN 1h SOA ns.example. hostmaster.example. 1 2h 10m 1w 5m'))
    assert_equal([3600, 7200, 600, 604800, 300], [soa.ttl, soa.refresh, soa.retry, soa.expire, soa.minimum])
    assert_equal(3630, reader.get_ttl('1h30'))
  end

  def test_class_before_ttl_and_ttl_before_class_are_equivalent
    %w[3600 1h 1H 30m30m].each do |ttl|
      expected = Dnsruby::RR.create(Dnsruby::ZoneReader.new('example.').process_line("www #{ttl} IN A 192.0.2.1"))
      actual = Dnsruby::RR.create(Dnsruby::ZoneReader.new('example.').process_line("www IN #{ttl} A 192.0.2.1"))
      assert_equal(expected, actual)
      assert_equal(expected.ttl, actual.ttl)
    end
  end

  def test_class_before_ttl_preserves_public_normalisation_hook_and_quoted_data
    reader_class = Class.new(Dnsruby::ZoneReader) do
      attr_reader :normalised
      def normalise_line(*args)
        @normalised = true
        super
      end
    end
    reader = reader_class.new('example.')
    rr = Dnsruby::RR.create(reader.process_line('www IN 1h TXT "one ; two"'))
    assert(reader.normalised)
    assert_equal(3600, rr.ttl)
    assert_equal(['one ; two'], rr.strings)
  end
end
