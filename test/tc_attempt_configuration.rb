require_relative 'tc_pipeline_send'
require 'minitest/mock'

class AttemptConfigurationTest < Minitest::Test
  def in_memory(code)
    out, err, status = Open3.capture3(RbConfig.ruby, '-Ilib', '-e', TestPipelineSend::FIXTURE + code,
      chdir: File.expand_path('..', __dir__))
    assert(status.success?, "#{out}\n#{err}")
  end

  def test_attempt_configuration_survives_tcp_connect_reconfiguration
    in_memory(<<~'RUBY')
      [false, true].each do |pipeline|
        resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', port: 5301,
          use_tcp: true, tcp_pipelining: pipeline, tcp_pipelining_max_queries: :infinite,
          tcp_idle_timeout: 10, dnssec: false)
        writer = resolver.single_resolvers.first
        connections, sockets = [], []
        writer.define_singleton_method(:connect_tcp) do |server, port, source, source_port, *_|
          connections << [server, port, source, source_port]
          if connections.length == 1
            resolver.port = 5302
            resolver.src_address = '127.0.0.2'
            resolver.src_port = 55001
            resolver.ignore_truncation = true
            resolver.no_tcp = true
            resolver.tcp_idle_timeout = 20
            resolver.tcp_pipelining_max_queries = 3
          end
          FrameSocket.new.tap do |socket|
            socket.define_singleton_method(:connect) { |_| raise Errno::EISCONN }
            sockets << socket
          end
        end
        message = query
        writer.send_async(message, Queue.new, :first)
        first = SELECTOR.query_settings_for_id(:first)
        raise 'connected endpoint changed' unless connections.first[0, 3] == ['127.0.0.1', 5301, '0.0.0.0']
        raise 'registered peer changed' unless [first.dest_server, first.dest_port] == connections.first[0, 2]
        raise 'response policy changed' if first.ignore_truncation || first.no_tcp
        raise 'idle policy changed' unless first.tcp_idle_timeout == 10
        raise 'cache route changed' unless first.cache_scope[0, 3] == ['127.0.0.1', 5301, '0.0.0.0']
        raise 'mutable attempt configuration' unless first.transport_configuration.frozen? && first.transport_configuration[:src_port].frozen?
        if pipeline
          raise 'pipeline capacity changed' unless first.tcp_pipelining_max_queries == :infinite
        end
        writer.send_async(query('next.example.'), Queue.new, :next)
        following = SELECTOR.query_settings_for_id(:next)
        raise 'later send missed new settings' unless connections.last == ['127.0.0.1', 5302, '127.0.0.2', 55001]
        raise 'later policy missed new settings' unless following.ignore_truncation && following.no_tcp && following.tcp_idle_timeout == 20
        SELECTOR.remove_id(:first)
        SELECTOR.remove_id(:next)
        resolver.close
        sockets.each { |socket| assert_clean(socket) }
      end
    RUBY
  end

  def test_attempt_configuration_survives_udp_bind_reconfiguration
    in_memory(<<~'RUBY')
      writer = Dnsruby::PacketSender.new(server: '127.0.0.1', port: 5301, src_port: 55000, dnssec: false)
      socket = FrameSocket.new
      connected = nil
      socket.define_singleton_method(:getsockopt) { |*| Struct.new(:data).new([Socket::SOCK_DGRAM].pack('i')) }
      socket.define_singleton_method(:bind) do |address, port|
        raise 'wrong source' unless [address, port] == ['0.0.0.0', 55000]
        writer.server = '127.0.0.2'
        writer.port = 5302
      end
      socket.define_singleton_method(:connect) { |*endpoint| connected = endpoint }
      UDPSocket.define_singleton_method(:new) { |*| socket }
      writer.send_async(query, Queue.new, :udp)
      pending = SELECTOR.query_settings_for_id(:udp)
      raise 'UDP connected to new endpoint' unless connected == ['127.0.0.1', 5301]
      raise 'UDP registration changed endpoint' unless [pending.dest_server, pending.dest_port] == connected
      SELECTOR.remove_id(:udp)
      writer.close
      assert_clean(socket)
    RUBY
  end

  def test_attempt_configuration_preserves_truncation_policy_until_response
    in_memory(<<~'RUBY')
      writer = Dnsruby::PacketSender.new(server: '127.0.0.1', no_tcp: true, dnssec: false)
      socket = FrameSocket.new
      socket.define_singleton_method(:bind) { |*| }
      socket.define_singleton_method(:connect) { |*| }
      socket.define_singleton_method(:getsockopt) { |*| Struct.new(:data).new([Socket::SOCK_DGRAM].pack('i')) }
      UDPSocket.define_singleton_method(:new) { |*| socket }
      message = query
      queue = Queue.new
      writer.send_async(message, queue, :policy)
      pending = SELECTOR.query_settings_for_id(:policy)
      writer.no_tcp = false
      writer.define_singleton_method(:send_async) { |*| raise 'response used changed no_tcp policy' }
      response = message.clone
      response.header.qr = response.header.tc = true
      Thread.current[:dnsruby_response_query] = pending
      raise 'ordinary truncated response rejected' unless writer.check_response(response, response.encode, message, queue, :policy, false)
      Thread.current[:dnsruby_response_query] = nil
      SELECTOR.remove_id(:policy)
      writer.close
      assert_clean(socket)
    RUBY
  end

  def test_attempt_configuration_explicit_ports_replace_default_random_pool
    [Dnsruby::Resolver, Dnsruby::SingleResolver, Dnsruby::PacketSender].each do |klass|
      resolver = klass.new(server: '127.0.0.1')
      resolver.add_src_port(55000)
      assert_equal(55000, resolver.src_port, klass)
      resolver.add_src_port(55001..55002)
      assert_equal([55000, 55001, 55002], resolver.src_port, klass)
      [[0, 55003], [55003, 0]].each do |ports|
        assert_raises(ArgumentError) { resolver.src_port = ports }
        assert_equal([55000, 55001, 55002], resolver.src_port, klass)
      end
      assert_raises(ArgumentError) { resolver.add_src_port(0) }
      assert_equal([55000, 55001, 55002], resolver.src_port, klass)
      if resolver.respond_to?(:single_resolvers)
        assert_equal(resolver.src_port, resolver.single_resolvers.first.src_port)
      end
      resolver.close
    end
  end

  def test_attempt_configuration_disabled_cache_skips_response_classification
    request = Dnsruby::Message.new('cache.example.')
    request.do_caching = false
    response = request.clone
    response.header.qr = true
    response.add_answer(Dnsruby::RR.create('cache.example. 60 IN A 192.0.2.1'))
    Dnsruby::Cache.stub(:negative_soa, ->(*) { raise 'disabled cache examined records' }) do
      assert_nil(Dnsruby::PacketSender.cache(request, response))
    end
  end
end
