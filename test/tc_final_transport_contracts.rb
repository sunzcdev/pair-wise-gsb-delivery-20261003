require_relative 'spec_helper'
require 'minitest/mock'
require 'open3'
require 'timeout'

class FinalTransportContractsTest < Minitest::Test
  def test_single_resolver_replacement_preserves_concurrent_settings
    resolver = Dnsruby::SingleResolver.new(server: '192.0.2.1', packet_timeout: 5)
    entered, release = Queue.new, Queue.new
    replacement = nil
    original = Dnsruby::PacketSender.method(:new)
    construct = lambda do |*args|
      sender = original.call(*args)
      if sender.server.to_s == '192.0.2.2'
        entered << true
        release.pop
      end
      sender
    end
    Dnsruby::PacketSender.stub(:new, construct) do
      begin
        replacement = Thread.new { resolver.server = '192.0.2.2' }
        Timeout.timeout(5) { entered.pop }
        resolver.packet_timeout = 9
        resolver.port = 5353
        resolver.src_address = '192.0.2.10'
        resolver.dnssec = false
      ensure
        release << true
        Timeout.timeout(5) { replacement&.value }
      end
    end
    sender = resolver.single_resolvers.first
    assert_equal('192.0.2.2', resolver.server)
    [:packet_timeout, :port, :src_address, :dnssec].each do |field|
      assert_equal(resolver.public_send(field), sender.public_send(field), field)
    end
  ensure
    replacement&.kill if replacement&.alive?
    resolver&.close
  end

  def test_tcp_reply_reports_its_message_size
    # The selector is process-global; isolate its in-memory socket fixture.
    source = <<~'RUBY'
      require 'dnsruby'
      selector_class = Dnsruby::SelectThread
      selector_class.class_variable_set(:@@mutex, Mutex.new)
      selector = selector_class.send(:allocate)
      query = Dnsruby::Message.new('metadata.example.')
      query.do_validation = query.do_caching = false
      response = query.clone
      response.header.qr = true
      response.add_answer(Dnsruby::RR.create('metadata.example. 60 IN A 192.0.2.1'))
      wire = response.encode
      socket = Object.new
      socket.define_singleton_method(:getsockopt) { |*| Struct.new(:data).new([Socket::SOCK_STREAM].pack('i')) }
      socket.define_singleton_method(:closed?) { false }
      selector.define_singleton_method(:tcp_read) { |_| wire }
      settings = selector_class::QuerySettings.new(query.encode, query, false, Queue.new, :metadata,
        socket, '192.0.2.53', 53, Dnsruby::TransportClock.now + 1, 512, nil)
      selector_class.class_variable_set(:@@query_hash, {metadata: settings})
      selector_class.class_variable_set(:@@socket_hash, {socket => [:metadata]})
      decoded, = selector.get_incoming_data(socket, 512)
      raise 'Wrong TCP size' unless decoded.answersize == wire.bytesize
      raise 'Wrong TCP peer' unless decoded.answerip == '192.0.2.53'
    RUBY
    out, err, status = Open3.capture3(RbConfig.ruby, '-Ilib', '-e', source,
      chdir: File.expand_path('..', __dir__))
    assert(status.success?, "#{out}\n#{err}")
  end

  def test_zone_transfer_preserves_authenticated_server_errors
    [Dnsruby::RCode::BADTIME, 22].each do |error_code|
      key = Dnsruby::RR.create(name: 'transfer-key.example.', type: 'TSIG', klass: 'ANY',
        key: ['test-only transfer secret'].pack('m0'), algorithm: 'hmac-sha256')
      transfer = Dnsruby::ZoneTransfer.new
      transfer.tsig = key
      request = Dnsruby::Message.new('example.', 'AXFR')
      sink = Object.new
      sink.define_singleton_method(:write_nonblock) { |data| data.bytesize }
      transfer.send_message(sink, request)
      response = Dnsruby::Message.new('example.', 'AXFR')
      response.header.qr = true
      response.header.rcode = Dnsruby::RCode::NOTAUTH
      signer = key.dup
      signer.error = error_code
      server_time = Time.now.to_i + 3600
      if error_code == Dnsruby::RCode::BADTIME
        signer.other_data = [server_time >> 32, server_time & 0xffffffff].pack('nN')
        signer.other_size = 6
      end
      signer.apply(response, request)
      wire = response.encode
      frame = [wire.bytesize].pack('n') + wire
      socket = Object.new
      socket.define_singleton_method(:read_nonblock) { |size| frame.slice!(0, size) }
      error = assert_raises(Dnsruby::TsigError) { transfer.receive_message(socket) }
      assert_equal(:Verified, transfer.last_tsigstate)
      assert_includes(error.message, error_code == Dnsruby::RCode::BADTIME ? 'BADTIME' : 'BADTRUNC')
      assert_includes(error.message, server_time.to_s) if error_code == Dnsruby::RCode::BADTIME
    end
  end
end
