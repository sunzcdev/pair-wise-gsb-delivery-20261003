require_relative 'tc_pipeline_send'

# These scenarios drive the real ResolverRuby fan-out/observer machinery with
# scripted in-process nameservers and a controlled response queue. No socket or
# public DNS is used: every reply is pushed through the same SelectThread queue
# the production transport would use.
class TestResolverWinner < Minitest::Test
  FIXTURE = <<~'RUBY'
    # Keep the real observer notifications so a queued event runs the exact
    # ResolverRuby#handle_queue_event winner/error path under its mutex.
    SELECTOR.singleton_class.remove_method(:notify_queue_observers)
    # Freeze the monotonic clock: with retry_delay 0 every nameserver is due in
    # the same tick, so the fan-out is genuinely parallel without real sleeps.
    Dnsruby::TransportClock.define_singleton_method(:now) { 1000.0 }

    def build_resolver
      resolver = Dnsruby::Resolver.new(
        nameserver: ['127.0.0.1', '127.0.0.2', '127.0.0.3'],
        dnssec: false, do_caching: false, do_validation: false,
        retry_times: 1, retry_delay: 0, query_timeout: 60, packet_timeout: 60)
      attempts = Queue.new
      resolver.single_resolvers.each do |writer|
        writer.define_singleton_method(:send_async) do |_message, _queue, id|
          attempts << [self, id]
        end
      end
      [resolver, attempts]
    end

    # Dispatch every admission-time attempt (retry_delay keeps later rounds far
    # in the future) and return [sender, attempt_id] keyed by server address.
    def dispatch(resolver, attempts)
      SELECTOR.send_tick_to_observers
      sent = {}
      until attempts.empty?
        sender, id = attempts.pop(true)
        sent[sender.server] = [sender, id]
      end
      raise 'not every nameserver was queried' unless sent.size == resolver.single_resolvers.size
      sent
    end

    def engine(resolver)
      resolver.instance_variable_get(:@resolver_ruby)
    end

    def select_queue(resolver, client_query_id)
      engine(resolver).instance_variable_get(:@query_list).fetch(client_query_id)[2]
    end

    def answer_for(id, rcode: Dnsruby::RCode.NOERROR, address: '192.0.2.10')
      _sender, message, _client_query_id, _retry = id
      response = message.clone
      response.header.qr = true
      response.header.rcode = rcode
      response.add_answer(Dnsruby::RR.create(
        name: message.question.first.qname, type: 'A', address: address)) if rcode == Dnsruby::RCode.NOERROR
      response
    end

    # Deliver a RECEIVED event carrying an RCODE-derived error, exactly as the
    # SelectThread does for a SERVFAIL datagram.
    def deliver_rcode(resolver, sent, server, rcode)
      _sender, id = sent.fetch(server)
      queue = select_queue(resolver, id[2])
      response = answer_for(id, rcode: rcode)
      error = response.get_exception
      queue << [id, Dnsruby::Resolver::EventType::RECEIVED, response, error]
      SELECTOR.notify_queue_observers(queue, id)
      [response, error]
    end

    def deliver_success(resolver, sent, server, address: '192.0.2.10')
      sender, id = sent.fetch(server)
      queue = select_queue(resolver, id[2])
      response = answer_for(id, address: address)
      queue << [id, Dnsruby::Resolver::EventType::RECEIVED, response, nil]
      SELECTOR.notify_queue_observers(queue, id)
      # Reproduce SelectThread#push_to_client for a non-DNSSEC query: the
      # accepted answer needs a VALIDATED event (same full attempt id) before
      # ResolverRuby completes it.
      SELECTOR.push_validation_response_to_select(id, queue, response, nil, id[1], sender)
      SELECTOR.send_queued_validation_responses
      response
    end

    def deliver_transport_error(resolver, sent, server, error)
      _sender, id = sent.fetch(server)
      queue = select_queue(resolver, id[2])
      queue << [id, Dnsruby::Resolver::EventType::ERROR, nil, error]
      SELECTOR.notify_queue_observers(queue, id)
    end

    # Outstanding attempts owned by the logical query inside ResolverRuby. The
    # scripted senders do not register real sockets, so this (not the selector
    # socket table) is the fan-out state the winner must drain.
    def outstanding_attempts(resolver, client_query_id)
      engine(resolver).instance_variable_get(:@query_list).fetch(client_query_id)[3]
    end

    def retry_plan(resolver, client_query_id)
      engine(resolver).instance_variable_get(:@timeouts).fetch(client_query_id)[1]
    end

    def recorded_failures(resolver, client_query_id)
      engine(resolver).instance_variable_get(:@query_failures).fetch(client_query_id)[:failures]
    end

    def finish_query(resolver, results)
      raise 'missing or duplicate completion' unless results.size == 1
      id, response, error = results.pop
      [id, response, error]
    ensure
      resolver.close
    end
  RUBY

  # An early SERVFAIL retires that one side but must not terminate the logical
  # query; a later legitimate answer from another nameserver still wins.
  def test_early_servfail_does_not_block_a_later_valid_answer
    isolated(<<~'RUBY')
      resolver, attempts = build_resolver
      results = Queue.new
      resolver.send_async(query('race.example.'), results, :race)
      sent = dispatch(resolver, attempts)

      deliver_rcode(resolver, sent, '127.0.0.1', Dnsruby::RCode.SERVFAIL)
      raise 'SERVFAIL completed the query early' unless results.empty?
      raise 'failed side reason was not recorded' unless
        recorded_failures(resolver, :race).map(&:first) == [sent.fetch('127.0.0.1').first]
      # The two healthy sides remain outstanding and eligible to answer.
      remaining = outstanding_attempts(resolver, :race).map { |id| id[0].server }.sort
      raise 'healthy nameservers were cancelled' unless remaining == ['127.0.0.2', '127.0.0.3']
      raise 'failed side kept a scheduled retry' unless retry_plan(resolver, :race).values.none? do |attempt|
        (attempt.first.is_a?(Array) ? attempt.map(&:first) : [attempt.first]).include?(sent.fetch('127.0.0.1').first)
      end

      winner = deliver_success(resolver, sent, '127.0.0.2')
      id, response, error = finish_query(resolver, results)
      raise 'wrong completion id' unless id == :race
      raise 'winner raised an error' if error
      raise 'wrong answer delivered' unless response.equal?(winner) &&
        response.answer.first.address.to_s == '192.0.2.10'
    RUBY
  end

  # Selecting a winner cancels every competing transmission and timer; a reply
  # arriving afterwards is dropped and cannot complete the query a second time.
  def test_winner_cancels_other_sides_and_a_late_reply_is_delivered_once
    isolated(<<~'RUBY')
      resolver, attempts = build_resolver
      results = Queue.new
      resolver.send_async(query('winner.example.'), results, :winner)
      sent = dispatch(resolver, attempts)
      # Capture the observer queue before the winner completes and removes it.
      late_queue = select_queue(resolver, :winner)
      raise 'fixture did not fan out to every nameserver' unless
        outstanding_attempts(resolver, :winner).size == 3

      winner = deliver_success(resolver, sent, '127.0.0.2')
      # Accepting the winner drains the competing attempts and clears the retry
      # plan via begin_validation; the VALIDATED event then completes it once.
      raise 'winner did not complete exactly once' unless results.size == 1
      raise 'observer retained after completion' unless K.class_variable_get(:@@observers).empty?

      # Late replies from the cancelled sides arrive through the real queue and
      # must both be ignored; the logical query already published its result.
      late_id = sent.fetch('127.0.0.1').last
      late_response = answer_for(late_id, rcode: Dnsruby::RCode.SERVFAIL)
      late_queue << [late_id, Dnsruby::Resolver::EventType::RECEIVED, late_response, Dnsruby::ServFail.new]
      SELECTOR.notify_queue_observers(late_queue, late_id)
      other_id = sent.fetch('127.0.0.3').last
      other_response = answer_for(other_id, address: '192.0.2.20')
      late_queue << [other_id, Dnsruby::Resolver::EventType::RECEIVED, other_response, nil]
      SELECTOR.notify_queue_observers(late_queue, other_id)

      id, response, error = results.pop
      raise 'query completed more than once' unless results.empty?
      raise 'wrong winner' unless id == :winner && error.nil? && response.equal?(winner)
      resolver.close
    RUBY
  end

  # Two good answers racing in the same dispatch produce exactly one
  # completion: the first accepted answer wins and the second is a no-op.
  def test_simultaneous_successes_select_one_winner_and_complete_once
    isolated(<<~'RUBY')
      resolver, attempts = build_resolver
      results = Queue.new
      resolver.send_async(query('simul.example.'), results, :simul)
      sent = dispatch(resolver, attempts)
      stale_queue = select_queue(resolver, :simul)

      first = deliver_success(resolver, sent, '127.0.0.1', address: '192.0.2.11')
      raise 'first answer did not complete the query once' unless results.size == 1
      # The second good answer races in after the winner locked completion. Its
      # RECEIVED and VALIDATED events target a query that no longer exists, so
      # they must be ignored rather than publishing a second result.
      _sender2, second_id = sent.fetch('127.0.0.2')
      second = answer_for(second_id, address: '192.0.2.12')
      stale_queue << [second_id, Dnsruby::Resolver::EventType::RECEIVED, second, nil]
      SELECTOR.notify_queue_observers(stale_queue, second_id)
      SELECTOR.push_validation_response_to_select(second_id, stale_queue, second, nil, second_id[1], _sender2)
      SELECTOR.send_queued_validation_responses

      id, response, error = finish_query(resolver, results)
      raise 'duplicate completion' unless results.empty?
      raise 'unexpected error' if error
      raise 'first eligible answer did not win' unless response.equal?(first)
      raise 'second answer leaked through' if response.equal?(second)
    RUBY
  end

  # When every side fails, the caller receives one error carrying each side's
  # reason in the public nameserver order, even when the failures are reported
  # out of order.
  def test_all_failing_sides_keep_ordered_reasons
    isolated(<<~'RUBY')
      resolver, attempts = build_resolver
      results = Queue.new
      resolver.send_async(query('fail.example.'), results, :fail)
      sent = dispatch(resolver, attempts)

      # Report in reverse order; the aggregate must still follow configuration.
      deliver_rcode(resolver, sent, '127.0.0.3', Dnsruby::RCode.REFUSED)
      raise 'query ended before every side failed' unless results.empty?
      deliver_transport_error(resolver, sent, '127.0.0.2',
        Dnsruby::OtherResolvError.new('connection reset'))
      deliver_rcode(resolver, sent, '127.0.0.1', Dnsruby::RCode.SERVFAIL)

      id, response, error = finish_query(resolver, results)
      raise 'wrong completion id' unless id == :fail
      raise 'expected an aggregate failure' unless error.is_a?(Dnsruby::AllServersFailed)
      servers = error.server_failures.map(&:first)
      raise "unexpected failure order: #{servers.inspect}" unless
        servers == ['127.0.0.1', '127.0.0.2', '127.0.0.3']
      classes = error.server_failures.map { |_server, reason| reason.class }
      raise 'reasons were not preserved' unless
        classes == [Dnsruby::ServFail, Dnsruby::OtherResolvError, Dnsruby::Refused]
      raise 'aggregate retained a response from an RCODE-only failure' unless error.response.nil?
    RUBY
  end

  # A single failing nameserver keeps its original exception type so the
  # one-second negative failure cache and single-server contracts are unchanged.
  def test_single_failing_side_preserves_its_original_error
    isolated(<<~'RUBY')
      resolver = Dnsruby::Resolver.new(
        nameserver: ['127.0.0.1'], dnssec: false, do_caching: true, do_validation: false,
        retry_times: 1, retry_delay: 0, query_timeout: 60, packet_timeout: 60)
      attempts = Queue.new
      resolver.single_resolvers.first.define_singleton_method(:send_async) do |_message, _queue, id|
        attempts << [self, id]
      end
      results = Queue.new
      cached_query = Dnsruby::Message.new('single.example.')
      cached_query.do_caching = true
      cached_query.do_validation = false
      resolver.send_async(cached_query, results, :single)
      SELECTOR.send_tick_to_observers
      _sender, id = attempts.pop
      queue = resolver.instance_variable_get(:@resolver_ruby).
        instance_variable_get(:@query_list).fetch(:single)[2]
      response = answer_for(id, rcode: Dnsruby::RCode.SERVFAIL)
      queue << [id, Dnsruby::Resolver::EventType::RECEIVED, response, Dnsruby::ServFail.new]
      SELECTOR.notify_queue_observers(queue, id)

      _id, returned, error = finish_query(resolver, results)
      raise 'single-server ServFail was wrapped' unless error.instance_of?(Dnsruby::ServFail)
      raise 'response was detached' unless returned.equal?(response)

      # The concrete exception class is retained in the one-second failure cache.
      failure_cache = resolver.instance_variable_get(:@resolver_ruby).
        instance_variable_get(:@response_cache)
      stored = failure_cache.instance_variable_get(:@failures).values.first
      raise 'single-server failure was not cached' unless stored && stored[2] == Dnsruby::ServFail
    RUBY
  end

  # End-to-end over real loopback UDP sockets (still no public DNS): the first
  # nameserver answers SERVFAIL while the second answers NOERROR concurrently.
  # The real transport and selector must still deliver the legitimate winner.
  def test_real_udp_fan_out_recovers_from_an_early_servfail
    sockets, threads, seen = [], [], Queue.new
    # Bind two distinct loopback addresses to the same port so one Resolver
    # (which applies a single port to every nameserver) can target both peers.
    probe = UDPSocket.new
    probe.bind('127.0.0.1', 0)
    port = probe.addr[1]
    probe.close
    answerer = lambda do |host, rcode|
      socket = UDPSocket.new
      socket.bind(host, port)
      sockets << socket
      thread = Thread.new do
        Thread.current.report_on_exception = false
        packet, sender = socket.recvfrom(65_535)
        message = Dnsruby::Message.decode(packet)
        message.header.qr = true
        message.header.ra = true
        message.header.rcode = rcode
        if rcode == Dnsruby::RCode.NOERROR
          message.add_answer(Dnsruby::RR.create(
            name: message.question.first.qname, type: 'A', address: '192.0.2.50'))
        end
        socket.send(message.encode, 0, sender[3], sender[1])
        seen << rcode
      end
      threads << thread
    end

    answerer.call('127.0.0.1', Dnsruby::RCode.SERVFAIL)
    answerer.call('127.0.0.2', Dnsruby::RCode.NOERROR)
    resolver = Dnsruby::Resolver.new(
      nameserver: ['127.0.0.1', '127.0.0.2'], port: port,
      dnssec: false, do_caching: false, retry_times: 1, retry_delay: 0,
      packet_timeout: 3, query_timeout: 8)
    begin
      response = resolver.query('real.example.')
      assert_equal(Dnsruby::RCode.NOERROR, response.rcode)
      assert_equal('192.0.2.50', response.answer.first.address.to_s)
      # Both sides really were contacted before the winner was selected.
      rcodes = 2.times.map { seen.pop }
      assert_includes(rcodes, Dnsruby::RCode.SERVFAIL)
      assert_includes(rcodes, Dnsruby::RCode.NOERROR)
    ensure
      resolver.close
      threads.each { |thread| thread.join(2) }
      sockets.each { |socket| socket.close unless socket.closed? }
    end
  end

  private
  def isolated(body)
    options = ''.frozen? ? ['--enable-frozen-string-literal'] : []
    stdout, stderr, status = Open3.capture3(RbConfig.ruby, *options, '-I',
      File.expand_path('../lib', __dir__), '-e',
      "#{TestPipelineSend::FIXTURE}\nTimeout.timeout(10) do\n#{FIXTURE}\n#{body}\nend")
    assert(status.success?, "#{stdout}\n#{stderr}")
    assert_empty(stderr)
  end
end