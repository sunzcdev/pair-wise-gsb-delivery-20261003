require_relative 'tc_pipeline_send'

class CancelReceiptTest < Minitest::Test
  # Engine-level receipts: every status, timer/retry release, exactly-once
  # terminal delivery, and a select thread left running.
  def test_receipt_distinguishes_not_started_in_flight_completed_and_cancelled
    isolated(<<~'RUBY')
      parent = Struct.new(:single_resolvers, :single_res_mutex).new([], Mutex.new)
      engine = Dnsruby::ResolverRuby.new(parent)
      sleeper = Thread.new { sleep }
      K.class_variable_set(:@@select_thread, sleeper)
      begin
        # Admitted with a pending retry, but no packet attempt in flight.
        client1, select1 = Queue.new, Queue.new
        SELECTOR.add_observer(select1, engine)
        engine.instance_variable_get(:@query_list)[:waiting] = [query('waiting.example.'), client1, select1, []]
        engine.instance_variable_get(:@timeouts)[:waiting] =
          [Dnsruby::TransportClock.now + 60, {Dnsruby::TransportClock.now + 30 => [Object.new, 0]}]
        engine.instance_variable_get(:@tick_deadlines)[:waiting] = Dnsruby::TransportClock.now + 30
        receipt = engine.cancel_async(:waiting)
        raise "wrong status #{receipt}" unless receipt.not_started? && receipt.delivery_prevented? &&
          !receipt.completed? && !receipt.in_flight?
        raise 'timer or retry plan retained' unless engine.instance_variable_get(:@timeouts).empty? &&
          engine.instance_variable_get(:@tick_deadlines).first.nil?
        raise 'cancel pushed a terminal state' unless client1.empty?
        raise 'observer retained' if K.class_variable_get(:@@observers).key?(select1)

        # An outstanding attempt is in flight; cancellation releases it.
        client2, select2 = Queue.new, Queue.new
        outstanding = [[Object.new, query('flying.example.'), :flying, 0]]
        SELECTOR.add_observer(select2, engine)
        engine.instance_variable_get(:@query_list)[:flying] = [outstanding.first[1], client2, select2, outstanding]
        engine.instance_variable_get(:@timeouts)[:flying] = [Dnsruby::TransportClock.now + 60, {}]
        receipt = engine.cancel_async(:flying)
        raise "wrong status #{receipt}" unless receipt.in_flight? && receipt.delivery_prevented?
        raise 'attempt records kept the query alive' unless outstanding.empty?
        raise 'cancel pushed a terminal state' unless client2.empty?
        again = engine.cancel_async(:flying)
        raise "wrong repeat status #{again}" unless again.already_cancelled? && again.delivery_prevented?

        # A delivered terminal result cannot be cancelled; the queue keeps
        # exactly one terminal state.
        client3, select3 = Queue.new, Queue.new
        SELECTOR.add_observer(select3, engine)
        engine.instance_variable_get(:@query_list)[:done] = [query('done.example.'), client3, select3, []]
        engine.instance_variable_get(:@timeouts)[:done] = [Dnsruby::TransportClock.now + 60, {}]
        engine.send_result_and_stop_querying(client3, :done, select3, nil, Dnsruby::OtherResolvError.new('boom'))
        raise 'terminal state not delivered' unless client3.length == 1 && client3.pop(true).first == :done
        receipt = engine.cancel_async(:done)
        raise "wrong status #{receipt}" unless receipt.completed? && !receipt.delivery_prevented?
        raise 'cancel added a second terminal state' unless client3.empty?

        receipt = engine.cancel_async(:never_seen)
        raise "wrong status #{receipt}" unless receipt.unknown? && !receipt.delivery_prevented?

        raise 'cancel killed the shared select thread' unless sleeper.alive? &&
          K.class_variable_get(:@@select_thread).equal?(sleeper)
      ensure
        sleeper.kill
      end
    RUBY
  end

  # Resolver-level in-flight cancel on a shared pipeline: late packets are
  # drained per connection protocol but delivered to nobody, never to a new
  # query reusing the DNS id, and the stream retires once its peer finishes.
  def test_cancelled_query_late_packets_are_drained_but_never_delivered
    isolated(<<~'RUBY')
      SELECTOR.singleton_class.remove_method(:notify_queue_observers)
      socket = FrameSocket.new
      writer = sender(socket)
      writer.use_tcp = true
      parent = Dnsruby::Resolver.new(nameserver: '127.0.0.1', use_tcp: true,
        tcp_pipelining: true, do_caching: false, dnssec: false)
      parent.single_resolvers = [writer]
      peer_q = query('peer.example.')
      peer_q.header.id = 111
      peer_queue = Queue.new
      parent.send_async(peer_q, peer_queue, :peer)
      q = query('late.example.')
      q.header.id = 222
      queue = Queue.new
      raise 'wrong id' unless parent.send_async(q, queue, :late) == :late

      receipt = parent.cancel_async(:late)
      raise "wrong status #{receipt}" unless receipt.in_flight? && receipt.delivery_prevented?
      raise 'cancel pushed a terminal state' unless queue.empty?
      engine = parent.instance_variable_get(:@resolver_ruby)
      raise 'engine retained query' unless engine.instance_variable_get(:@query_list).keys == [:peer]
      raise 'engine retained timer' if engine.instance_variable_get(:@timeouts).key?(:late)
      raise 'selector retained attempt' if K.class_variable_get(:@@query_hash).keys.any? { |id| id[2] == :late }
      raise 'peer stream closed early' if socket.closed?
      raise 'stream not retired for new queries' unless K.class_variable_get(:@@retiring_sockets).include?(socket)

      # A late packet for the cancelled query is consumed by the connection
      # but delivered to nobody.
      late = q.clone
      late.header.qr = true
      SELECTOR.process_message(late, late.encode, socket)
      SELECTOR.send_queued_validation_responses
      raise 'late packet delivered' unless queue.empty?

      # A new query reusing the DNS id goes to a fresh connection; the late
      # packet replayed on the old stream must not reach it.
      socket2 = FrameSocket.new
      writer2 = sender(socket2)
      writer2.use_tcp = true
      parent.single_resolvers = [writer2]
      q2 = query('late.example.')
      q2.header.id = 222
      queue2 = Queue.new
      parent.send_async(q2, queue2, :new)
      SELECTOR.process_message(late, late.encode, socket)
      SELECTOR.send_queued_validation_responses
      raise 'late packet reached a new query' unless queue2.empty?
      answer = q2.clone
      answer.header.qr = true
      SELECTOR.process_message(answer, answer.encode, socket2)
      SELECTOR.send_queued_validation_responses
      raise 'new query did not complete' unless queue2.length == 1
      id, msg, err = queue2.pop(true)
      raise 'wrong completion' unless id == :new && err.nil? && msg
      raise 'second terminal state delivered' unless queue2.empty?

      # The peer still completes on the draining stream, which then closes.
      peer_answer = peer_q.clone
      peer_answer.header.qr = true
      SELECTOR.process_message(peer_answer, peer_answer.encode, socket)
      SELECTOR.send_queued_validation_responses
      raise 'peer did not complete' unless peer_queue.length == 1 && peer_queue.pop(true).first == :peer
      raise 'drained stream left open' unless socket.closed?

      raise 'wrong repeat receipt' unless parent.cancel_async(:late).already_cancelled?
      raise 'wrong completed receipt' unless parent.cancel_async(:new).completed?
      raise 'wrong peer receipt' unless parent.cancel_async(:peer).completed?
      raise 'wrong unknown receipt' unless parent.cancel_async(:never_seen).unknown?
    RUBY
  end

  # Cancellation between reservation and transmission: the send does not go
  # out, a transport failure surfacing after the cancel is not delivered as
  # a second terminal state, and the shared select thread is left alone.
  def test_cancel_before_transmission_wins_and_late_failures_are_not_delivered
    isolated(<<~'RUBY')
      socket = FrameSocket.new
      writer = sender(socket)
      writer.use_tcp = true
      parent = Dnsruby::Resolver.new(nameserver: '127.0.0.1', use_tcp: true,
        tcp_pipelining: true, do_caching: false, dnssec: false)
      parent.single_resolvers = [writer]
      sleeper = Thread.new { sleep }
      K.class_variable_set(:@@select_thread, sleeper)
      begin
        sent = false
        writer.define_singleton_method(:send_async) do |*|
          sent = true
          raise IOError, 'simulated transport failure'
        end
        settings_for = SELECTOR.method(:query_settings_for_id)
        first_receipt = nil
        SELECTOR.define_singleton_method(:query_settings_for_id) do |id|
          settings = settings_for.call(id)
          if settings && !first_receipt
            first_receipt = parent.cancel_async(:early)
          end
          settings
        end
        queue = Queue.new
        parent.send_async(query('early.example.'), queue, :early)
        raise 'cancel did not run' unless first_receipt
        raise "wrong status #{first_receipt}" unless first_receipt.in_flight?
        raise 'send was not attempted' unless sent
        raise 'cancelled send wrote bytes' unless socket.written.empty?
        raise 'late transport failure delivered' unless queue.empty?
        raise 'selector retained attempt' unless K.class_variable_get(:@@query_hash).empty?
        raise 'engine retained query' unless parent.instance_variable_get(:@resolver_ruby).
          instance_variable_get(:@query_list).empty?
        raise 'wrong repeat receipt' unless parent.cancel_async(:early).already_cancelled?
        raise 'cancel killed the shared select thread' unless sleeper.alive? &&
          K.class_variable_get(:@@select_thread).equal?(sleeper)
      ensure
        sleeper.kill
      end
    RUBY
  end

  private

  def isolated(code)
    stdout, stderr, status = Open3.capture3(RbConfig.ruby, '-I', File.expand_path('../lib', __dir__),
      '-e', TestPipelineSend::FIXTURE + code)
    assert(status.success?, "#{stdout}\n#{stderr}")
    assert_empty(stderr)
  end
end
