require_relative 'tc_pipeline_send'

# A UDP answer with TC set is only a half answer. These scenarios feed
# controlled packet sequences to both transports and check that the fallback
# keeps the original question, endpoint and policy, that the half answer is never
# delivered, that a still-truncated or mismatched TCP answer is reported
# distinctly, that the fallback is bounded, and that cancelling stops both
# attempts.
class TruncationFallbackTest < Minitest::Test
  HELPERS = <<~'RUBY'
    def answer_for(request, text, name: nil)
      question = request.question.first
      reply = Dnsruby::Message.new
      reply.header.id = request.header.id
      reply.header.qr = true
      reply.header.rcode = Dnsruby::RCode::NOERROR
      reply.add_question(name || question.qname, question.qtype, question.qclass)
      reply.add_answer(Dnsruby::RR.create("#{name || question.qname} 60 #{question.qclass} TXT \"#{text}\""))
      reply
    end

    def truncated_answer(request, text)
      answer_for(request, text).tap { |reply| reply.header.tc = true }
    end

    def install_udp_factory
      sockets = []
      UDPSocket.define_singleton_method(:new) do |*|
        FrameSocket.new.tap do |socket|
          socket.define_singleton_method(:bind) { |*| }
          socket.define_singleton_method(:connect) { |*| }
          socket.define_singleton_method(:getsockopt) do |*|
            Struct.new(:data).new([Socket::SOCK_DGRAM].pack('i'))
          end
          sockets << socket
        end
      end
      sockets
    end

    def feed_udp(socket, wire)
      bytes = wire.is_a?(String) ? wire : wire.encode
      socket.define_singleton_method(:recvfrom_nonblock) do |*|
        [bytes, ['AF_INET', 53, '127.0.0.1', '127.0.0.1']]
      end
      incoming = SELECTOR.get_incoming_data(socket, 512)
      SELECTOR.process_message(*incoming, socket) if incoming
    end

    def feed_tcp(socket, wire)
      bytes = wire.is_a?(String) ? wire : wire.encode
      socket.incoming = [bytes.bytesize].pack('n') + bytes
      incoming = SELECTOR.get_incoming_data(socket, 0)
      SELECTOR.process_message(*incoming, socket) if incoming
    end

    def delivered(queue)
      SELECTOR.send_queued_exceptions
      SELECTOR.send_queued_responses
      SELECTOR.send_queued_validation_responses
      events = []
      events << queue.pop(true) until queue.empty?
      events
    end
  RUBY

  def test_truncated_udp_answer_is_replaced_by_one_tcp_attempt_with_the_original_transaction
    isolated(<<~'RUBY')
      udp_sockets = install_udp_factory
      writer = Dnsruby::PacketSender.new(server: '127.0.0.1', port: 5301, dnssec: false, packet_timeout: 1)
      tcp = FrameSocket.new
      connected = []
      writer.define_singleton_method(:connect_tcp) do |server, port, source, source_port, *_|
        connected << [server, port, source, source_port]
        tcp
      end
      request, queue = query('fallback.example.'), Queue.new
      writer.send_async(request, queue, :fallback)
      udp = udp_sockets.first
      udp_query = Dnsruby::Message.decode(udp.written)
      raise 'UDP attempt did not advertise EDNS' unless udp_query.get_opt
      raise 'UDP attempt unexpectedly asked for DNSSEC' if udp_query.get_opt.dnssec_ok

      # A reconfiguration applies to later queries, never to the attempt that is
      # already in progress.
      writer.server = '127.0.0.2'
      writer.port = 5302
      writer.src_address = '127.0.0.2'
      writer.src_port = 55_001
      writer.no_tcp = true
      writer.ignore_truncation = true
      writer.dnssec = true
      writer.packet_timeout = 30

      feed_udp(udp, truncated_answer(request, 'half'))
      raise 'half answer was delivered' unless queue.empty?
      raise 'fallback did not connect exactly once' unless connected.length == 1
      raise 'fallback followed the reconfigured nameserver' unless connected.first[0, 2] == ['127.0.0.1', 5301]
      raise 'fallback followed the reconfigured source' unless connected.first[2] == '0.0.0.0'
      retry_query = Dnsruby::Message.decode(tcp.written.byteslice(2..-1))
      raise 'fallback changed the transaction ID' unless retry_query.header.id == request.header.id
      raise 'fallback changed the question' unless retry_query.question.first.qname.to_s(true) == 'fallback.example.'
      raise 'fallback inherited the later DNSSEC setting' if retry_query.get_opt && retry_query.get_opt.dnssec_ok
      raise 'fallback changed the advertised size' unless retry_query.get_opt.nil? || retry_query.get_opt.payloadsize == 1232

      feed_tcp(tcp, answer_for(request, 'complete'))
      events = delivered(queue)
      raise 'fallback did not deliver exactly once' unless events.length == 2
      raise 'delivery carried an error' unless events.all? { |event| event.last.nil? }
      raise 'wrong answer' unless events.last[2].answer.first.strings == ['complete']
      raise 'half answer survived' if events.last[2].header.tc
      raise 'completed transaction retained' if SELECTOR.query_settings_for_id(:fallback)
      assert_clean(udp)
      assert_clean(tcp)
    RUBY
  end

  def test_truncated_udp_answer_for_another_query_cannot_borrow_its_settings
    isolated(<<~'RUBY')
      udp_sockets = install_udp_factory
      writer = Dnsruby::PacketSender.new(server: '127.0.0.1', port: 5301, dnssec: false, packet_timeout: 1)
      tcp = FrameSocket.new
      connected = []
      writer.define_singleton_method(:connect_tcp) do |server, port, source, source_port, *_|
        connected << [server, port, source, source_port]
        tcp
      end
      first, second = query('first.example.'), query('second.example.')
      first_queue, second_queue = Queue.new, Queue.new
      writer.send_async(first, first_queue, :first)
      writer.send_async(second, second_queue, :second)
      raise 'both UDP attempts were not sent' unless udp_sockets.length == 2
      first_socket, second_socket = udp_sockets
      first_wire = first_socket.written.dup
      second_wire = second_socket.written.dup

      feed_udp(first_socket, truncated_answer(first, 'first-half'))
      raise 'first half answer was delivered' unless first_queue.empty?
      raise 'second query was disturbed' unless second_queue.empty?
      raise 'fallback connected more than once' unless connected.length == 1
      retry_query = Dnsruby::Message.decode(tcp.written.byteslice(2..-1))
      raise 'fallback used the other query name' unless retry_query.question.first.qname.to_s(true) == 'first.example.'
      raise 'second attempt lost its reservation' unless SELECTOR.query_settings_for_id(:second)
      raise 'second attempt was replaced' if SELECTOR.query_settings_for_id(:second).query.equal?(first)
      raise 'second UDP attempt was rewritten' unless second_socket.written == second_wire
      raise 'first UDP attempt was rewritten' unless first_socket.written == first_wire

      feed_tcp(tcp, answer_for(first, 'first-complete'))
      events = delivered(first_queue)
      raise 'first query did not complete' unless events.length == 2 &&
        events.all? { |event| event.last.nil? } && events.last[2].answer.first.strings == ['first-complete']

      feed_udp(second_socket, answer_for(second, 'second-complete'))
      second_events = delivered(second_queue)
      raise 'second query lost its own answer' unless second_events.length == 2 &&
        second_events.last[2].answer.first.strings == ['second-complete']
      raise 'second query fell back' unless connected.length == 1
      assert_clean(first_socket)
      assert_clean(second_socket)
      assert_clean(tcp)
    RUBY
  end

  def test_tcp_answer_that_is_still_truncated_is_an_error_not_the_half_answer
    isolated(<<~'RUBY')
      [true, false].each do |pipeline|
        udp_sockets = install_udp_factory
        writer = Dnsruby::PacketSender.new(server: '127.0.0.1', port: 5301, dnssec: false,
          packet_timeout: 1, tcp_pipelining: pipeline)
        tcp = FrameSocket.new
        connects = 0
        if pipeline
          writer.define_singleton_method(:tcp_pipeline_socket) { |_| connects += 1; [tcp, true] }
        else
          writer.define_singleton_method(:connect_tcp) { |*| connects += 1; tcp }
        end
        request, queue = query('still-truncated.example.'), Queue.new
        writer.send_async(request, queue, :tc)
        feed_udp(udp_sockets.first, truncated_answer(request, 'udp-half'))
        raise 'UDP half answer delivered' unless queue.empty?
        feed_tcp(tcp, truncated_answer(request, 'tcp-half'))
        events = delivered(queue)
        raise 'truncated TCP answer was delivered as a complete answer' unless events.length == 1
        error = events.first.last
        raise "wrong error #{error.inspect}" unless error.is_a?(Dnsruby::TruncatedResponseError)
        raise 'half answer leaked through the error path' unless events.first[2].nil?
        raise 'one UDP answer produced more than one TCP attempt' unless connects == 1
        raise 'transaction retained' if SELECTOR.query_settings_for_id(:tc)
        SELECTOR.retire_sender(writer)
        assert_clean(tcp)
      end
    RUBY
  end

  def test_tcp_answer_for_a_different_question_ends_the_fallback_distinctly
    isolated(<<~'RUBY')
      udp_sockets = install_udp_factory
      writer = Dnsruby::PacketSender.new(server: '127.0.0.1', port: 5301, dnssec: false, packet_timeout: 1)
      tcp = FrameSocket.new
      writer.define_singleton_method(:connect_tcp) { |*| tcp }
      request, queue = query('asked.example.'), Queue.new
      writer.send_async(request, queue, :mismatch)
      feed_udp(udp_sockets.first, truncated_answer(request, 'udp-half'))
      raise 'UDP half answer delivered' unless queue.empty?
      feed_tcp(tcp, answer_for(request, 'elsewhere', name: Dnsruby::Name.create('other.example.')))
      events = delivered(queue)
      raise 'mismatched TCP answer delivered as a result' unless events.length == 1
      error = events.first.last
      raise "wrong error #{error.inspect}" unless error.is_a?(Dnsruby::MismatchedResponseError)
      raise 'mismatched answer reported as data' unless events.first[2].nil?
      raise 'mismatched answer retained the transaction' if SELECTOR.query_settings_for_id(:mismatch)
      assert_clean(tcp)
    RUBY
  end

  def test_tcp_connection_loss_after_a_truncated_udp_answer_reports_the_interruption
    isolated(<<~'RUBY')
      udp_sockets = install_udp_factory
      writer = Dnsruby::PacketSender.new(server: '127.0.0.1', port: 5301, dnssec: false, packet_timeout: 1)
      writer.define_singleton_method(:connect_tcp) do |server, *|
        raise Errno::ECONNREFUSED, server
      end
      request, queue = query('interrupted.example.'), Queue.new
      writer.send_async(request, queue, :interrupted)
      feed_udp(udp_sockets.first, truncated_answer(request, 'udp-half'))
      events = delivered(queue)
      raise 'interruption was not reported' unless events.length == 1
      error = events.first.last
      raise "wrong error #{error.inspect}" unless error.is_a?(IOError)
      raise 'UDP half answer delivered after a lost TCP connection' unless events.first[2].nil?
      raise 'interrupted transaction retained' if SELECTOR.query_settings_for_id(:interrupted)
      assert_clean(udp_sockets.first)
    RUBY
  end

  def test_repeated_truncation_is_bounded_to_one_fallback
    isolated(<<~'RUBY')
      writer = Dnsruby::PacketSender.new(server: '127.0.0.1', dnssec: false, packet_timeout: 1)
      retries = []
      writer.define_singleton_method(:send_async) { |*args| retries << args }
      request = query
      first_attempt = K::QuerySettings.new(request.encode, request, false, Queue.new, :bounded, nil,
        '127.0.0.1', 53, Dnsruby::TransportClock.now + 1, 512, writer)
      half = truncated_answer(request, 'udp-half')
      result = begin
        Thread.current[:dnsruby_response_query] = first_attempt
        writer.check_response(half, half.encode, request, first_attempt.client_queue, :bounded, false)
      ensure
        Thread.current[:dnsruby_response_query] = nil
      end
      raise 'first truncated answer was accepted' unless result == false
      raise 'first truncated answer did not start exactly one fallback' unless retries.length == 1 &&
        retries.first[3] == true
      raise 'half answer was not discarded' if first_attempt.client_queue.length > 0

      # The replacement attempt inherits the budget; a second truncated reply on
      # that transaction must not start another transport.
      retried = K::QuerySettings.new(request.encode, request, false, first_attempt.client_queue, :bounded, nil,
        '127.0.0.1', 53, Dnsruby::TransportClock.now + 1, 512, writer)
      retried.fallback_count = first_attempt.fallback_count
      retried.truncation_escalation = first_attempt.truncation_escalation
      again = truncated_answer(request, 'again')
      bounded = begin
        Thread.current[:dnsruby_response_query] = retried
        writer.check_response(again, again.encode, request, retried.client_queue, :bounded, true)
      ensure
        Thread.current[:dnsruby_response_query] = nil
      end
      raise 'bounded TCP fallback restarted another transport' unless retries.length == 1
      raise "wrong bounded result #{bounded.inspect}" unless bounded.is_a?(Dnsruby::TruncatedResponseError)
    RUBY
  end

  def test_cancelling_a_lookup_cleans_up_both_attempts
    isolated(<<~'RUBY')
      udp_sockets = install_udp_factory
      writer = Dnsruby::PacketSender.new(server: '127.0.0.1', port: 5301, dnssec: false, packet_timeout: 30)
      tcp = FrameSocket.new
      writer.define_singleton_method(:connect_tcp) { |*| tcp }
      request, queue = query('cancelled.example.'), Queue.new
      writer.send_async(request, queue, :cancelled)
      udp = udp_sockets.first
      feed_udp(udp, truncated_answer(request, 'udp-half'))
      raise 'fallback was not started' unless tcp.written.bytesize > 0
      raise 'TCP attempt not registered' unless SELECTOR.query_settings_for_id(:cancelled)
      SELECTOR.remove_id(:cancelled)
      raise 'cancel left the TCP attempt registered' if SELECTOR.query_settings_for_id(:cancelled)
      raise 'cancel produced a completion' unless queue.empty?

      # Late answers on either transport must not complete the cancelled work.
      feed_udp(udp, answer_for(request, 'late-udp'))
      feed_tcp(tcp, answer_for(request, 'late-tcp'))
      raise 'late cancellation result' unless queue.empty?
      assert_clean(tcp)
      assert_clean(udp)
    RUBY
  end

  def test_completed_fallback_answer_is_cached_under_the_original_route
    isolated(<<~'RUBY')
      udp_sockets = install_udp_factory
      writer = Dnsruby::PacketSender.new(server: '127.0.0.1', port: 5301, dnssec: false, packet_timeout: 1)
      tcp = FrameSocket.new
      writer.define_singleton_method(:connect_tcp) { |*| tcp }
      request, queue = query('cache-fallback.example.'), Queue.new
      request.do_caching = true
      writer.send_async(request, queue, :cached)
      feed_udp(udp_sockets.first, truncated_answer(request, 'udp-half'))
      # Move the sender to another nameserver before the TCP answer arrives.
      writer.server = '127.0.0.2'
      writer.port = 5302
      answer = answer_for(request, 'complete')
      answer.header.aa = true
      feed_tcp(tcp, answer)
      events = delivered(queue)
      raise 'fallback answer was not delivered' unless events.length == 2 && events.all? { |event| event.last.nil? }
      scope = events.last[2].instance_variable_get(:@dnsruby_cache_scope)
      raise 'fallback answer was not cached' unless scope && scope != false
      raise 'fallback cached under the new nameserver' unless scope[0, 2] == ['127.0.0.1', 5301]
      assert_clean(tcp)
    RUBY
  end

  def test_pre_fallback_mismatches_still_only_ignore_the_packet
    isolated(<<~'RUBY')
      writer = Dnsruby::PacketSender.new(server: '127.0.0.1', dnssec: false, packet_timeout: 1)
      retries = []
      writer.define_singleton_method(:send_async) { |*args| retries << args }
      request = query('plain.example.')
      attempt = K::QuerySettings.new(request.encode, request, false, Queue.new, :plain, nil,
        '127.0.0.1', 53, Dnsruby::TransportClock.now + 1, 512, writer)
      other = answer_for(request, 'other', name: Dnsruby::Name.create('elsewhere.example.'))
      Thread.current[:dnsruby_response_query] = attempt
      result = begin
        writer.check_response(other, other.encode, request, attempt.client_queue, :plain, false)
      ensure
        Thread.current[:dnsruby_response_query] = nil
      end
      raise 'an unrelated UDP answer completed the query' unless result == false
      raise 'an unrelated UDP answer started a retry' unless retries.empty?
      raise 'an unrelated UDP answer was delivered' unless attempt.client_queue.empty?
    RUBY
  end

  private

  def isolated(body)
    options = ''.frozen? ? ['--enable-frozen-string-literal'] : []
    script = TestPipelineSend::FIXTURE + "\n" + HELPERS + "\nTimeout.timeout(10) do\n" + body + "\nend"
    stdout, stderr, status = Open3.capture3(RbConfig.ruby, *options, '-I',
      File.expand_path('../lib', __dir__), '-e', script)
    assert(status.success?, "#{stdout}\n#{stderr}")
    assert_empty(stderr)
  end
end
