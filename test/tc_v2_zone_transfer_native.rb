require_relative 'spec_helper'
require 'socket'
require 'timeout'

class V2ZoneTransferNativeTest < Minitest::Test
  def test_axfr_over_tcp_with_and_without_tsig
    [false, true].each do |signed|
      listener = TCPServer.new('127.0.0.1', 0)
      transfer = Dnsruby::ZoneTransfer.new
      transfer.server = '127.0.0.1'
      transfer.port = listener.addr[1]
      transfer.io_timeout = 2
      transfer.transfer_timeout = 3
      transfer.tsig = ['key.example.', ['local-transfer-secret'].pack('m0')] if signed
      soa = Dnsruby::RR.create('example. 300 IN SOA ns.example. hostmaster.example. 2 60 60 3600 60')
      address = Dnsruby::RR.create('www.example. 300 IN A 192.0.2.1')
      server = Thread.new do
        socket = listener.accept
        query = Dnsruby::Message.decode(socket.read(socket.read(2).unpack1('n')))
        response = Dnsruby::Message.new('example.', 'AXFR')
        response.header.id = query.header.id
        response.header.qr = response.header.aa = true
        response.add_answer(soa)
        response.add_answer(address)
        if signed
          response.add_answer!(soa.clone)
          key = Dnsruby::RR.create(name: 'key.example.', type: 'TSIG', klass: 'ANY',
            key: ['local-transfer-secret'].pack('m0'))
          key.apply(response, query)
          responses = [response]
        else
          closing = Dnsruby::Message.new
          closing.header.id = query.header.id
          closing.header.qr = closing.header.aa = true
          closing.add_answer(soa.clone)
          responses = [response, closing]
        end
        responses.each do |message|
          wire = message.encode
          # TCP may coalesce these writes or deliver any combination of them.
          socket.write([wire.bytesize].pack('n').byteslice(0, 1))
          socket.write([wire.bytesize].pack('n').byteslice(1, 1))
          socket.write(wire)
        end
      ensure
        socket&.close
      end
      assert_equal([soa, address], Timeout.timeout(5) { transfer.transfer('example') })
      assert_equal(:Verified, transfer.last_tsigstate) if signed
      assert_nil(transfer.last_tsigstate) unless signed
      Timeout.timeout(2) { server.value }
    ensure
      listener&.close
      server&.kill
      server&.join
    end
  end
end
