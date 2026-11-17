require_relative 'tc_pipeline_send'
require 'minitest/mock'

class V2ReleaseTransportTest < Minitest::Test
  def test_udp_readiness_without_data_keeps_the_attempt_registered
    isolated(<<~'SCENARIO')
      SELECTOR.define_singleton_method(:tcp?) { |_| false }
      socket = FrameSocket.new
      request = query
      setting = settings(socket, :datagram, request)
      setting.is_persistent_socket = false
      SELECTOR.add_to_select(setting)
      [IO::EAGAINWaitReadable, Errno::EINTR].each do |exception|
        socket.define_singleton_method(:recvfrom_nonblock) { |_| raise exception }
        raise 'unexpected response' unless SELECTOR.get_incoming_data(socket, 512).nil?
        raise 'attempt lost' unless SELECTOR.query_settings_for_id(:datagram).equal?(setting)
        raise 'socket closed' if socket.closed?
        raise 'retryable read became an error' unless setting.client_queue.empty?
      end
      SELECTOR.remove_id(:datagram)
    SCENARIO
  end

  def test_pipeline_configuration_is_snapshotted_before_worker_dispatch
    isolated(<<~'SCENARIO')
      [false, true].each do |initial_mode|
        socket = FrameSocket.new
        writer = Dnsruby::PacketSender.new(server: '127.0.0.1', tcp_pipelining: initial_mode)
        writer.define_singleton_method(:connect_tcp) { |*| socket }
        writer.define_singleton_method(:tcp_pipeline_socket) do |*|
          raise 'shared writer mutex missing' unless instance_variable_get(:@pipeline_mutex).owned?
          [socket, true]
        end
        K.class_variable_set(:@@select_thread, Thread.current)
        dispatched = nil
        SELECTOR.define_singleton_method(:defer_send) do |owner, &operation|
          dispatched = owner
          writer.tcp_pipelining = !initial_mode
          operation.call
        end
        request = query
        writer.send_dnsruby(request.encode, request, initial_mode, Queue.new, true)
        registered = SELECTOR.query_settings_for_id(initial_mode)
        raise 'configuration changed admitted socket mode' unless registered &&
          !!registered.is_persistent_socket == initial_mode
        raise 'worker grouping changed during admission' unless dispatched.equal?(initial_mode ? writer : nil)
        raise 'incomplete frame' unless socket.written.bytesize == request.encode.bytesize + 2
        SELECTOR.remove_id(initial_mode)
        socket.close
      end
    SCENARIO
  end

  def test_packet_timeout_processing_visits_only_due_owners
    isolated(<<~'SCENARIO')
      now = Dnsruby::TransportClock.now
      due = settings(FrameSocket.new, :due)
      later = settings(FrameSocket.new, :later)
      due.endtime, later.endtime = now - 1, now + 60
      [due, later].each { |setting| SELECTOR.add_to_select(setting) }
      table = K.class_variable_get(:@@timeouts)
      [:each, :map, :keys].each { |method| table.define_singleton_method(method) { raise 'scanned packet table' } }
      SELECTOR.process_timeouts
      raise 'future attempt expired' unless SELECTOR.query_settings_for_id(:later).equal?(later)
      raise 'due attempt retained' if SELECTOR.query_settings_for_id(:due)
      raise 'timeout missing' unless due.client_queue.pop.last.is_a?(Dnsruby::ResolvTimeout)
      SELECTOR.remove_id(:later)
    SCENARIO
  end

  def test_logical_tick_visits_only_due_plans_in_admission_order
    isolated(<<~'SCENARIO')
      sent = []
      writer = Object.new
      writer.define_singleton_method(:server) { '192.0.2.53' }
      writer.define_singleton_method(:prepare_send_async) { |_, _, id, *| -> { sent << id[2] } }
      parent = Struct.new(:single_resolvers, :single_res_mutex).new([writer], Mutex.new)
      engine = Dnsruby::ResolverRuby.new(parent)
      now = Dnsruby::TransportClock.now
      plans = engine.instance_variable_get(:@timeouts)
      [:due, :later].each do |id|
        engine.instance_variable_get(:@query_list)[id] = [query, Queue.new, Queue.new, [], nil]
        plans[id] = [now + 120, {(id == :due ? now - 1 : now + 60) => [writer, 0]}, false]
        engine.send(:update_tick_deadline, id)
      end
      plans.define_singleton_method(:keys) { raise 'scanned all logical queries' }
      plans[:later][1].define_singleton_method(:each_key) { raise 'visited future retry plan' }
      engine.tick
      raise 'incorrect due sends' unless sent == [:due]
      raise 'incorrect next deadline' unless engine.next_tick_at == now + 60
    SCENARIO
  end

  def test_retries_are_bounded_per_server_and_transport
    budget = Dnsruby::SelectThread::TransmissionBudget.new
    3.times { assert(budget.admit('192.0.2.53', 53, false)) }
    refute(budget.admit('192.0.2.53', 53, false))
    assert(budget.admit('192.0.2.53', 53, true))
    assert(budget.admit('192.0.2.54', 53, false))
    resolver = Dnsruby::Resolver.new(nameserver: ['192.0.2.53', '192.0.2.54'], retry_times: 6)
    counts = resolver.generate_timeouts.values.group_by(&:first).transform_values(&:length)
    assert_equal([3, 3], counts.values.sort)
  ensure
    resolver&.close
  end

  def test_validation_worker_shares_the_original_transmission_limit
    budget = Dnsruby::SelectThread::TransmissionBudget.new
    query = Dnsruby::Message.new('example.', 'A')
    query.do_validation = true
    assert(budget.admit('192.0.2.53', 53, false, query.question.first))
    response = query.clone
    completions = []
    selector = Object.new
    selector.define_singleton_method(:validation_transmission_budget) { |*| budget }
    selector.define_singleton_method(:register_validation) { |*| true }
    selector.define_singleton_method(:unregister_validation) { |*| }
    selector.define_singleton_method(:push_validation_response_to_select) { |*args| completions << args }
    sender = Struct.new(:dnssec).new(true)
    validator = Dnsruby::ValidatorThread.new(:client, Queue.new, response, nil, query, selector, sender)
    validate = lambda do |_, answer|
      # Separate helper send tokens must count against the original resolution.
      2.times do
        token = Dnsruby::SelectThread::SendToken.new
        assert(token.transmission_budget.admit('192.0.2.53', 53, false, query.question.first))
      end
      token = Dnsruby::SelectThread::SendToken.new
      refute(token.transmission_budget.admit('192.0.2.53', 53, false, query.question.first))
      answer.security_level = Dnsruby::Message::SecurityLevel.SECURE
      true
    end
    Dnsruby::Dnssec.stub(:validate_with_query, validate) do
      Dnsruby::PacketSender.stub(:cache, ->(*) {}) do
        assert_nil(Thread.new { validator.do_validate; Dnsruby::SelectThread::TransmissionBudget.current }.value)
      end
    end
    assert_equal(1, completions.length)
    assert_nil(completions.first[3])
  end

  class TransferSocket
    attr_reader :written
    def initialize(input = ''.b)
      @input, @written, @closed = input.dup, +''.b, false
    end
    def write_nonblock(bytes)
      count = [bytes.bytesize, 3].min
      @written << bytes.byteslice(0, count)
      count
    end
    def read_nonblock(length)
      raise EOFError if @input.empty?
      @input.slice!(0, [length, 5].min)
    end
    def close; @closed = true; end
    def closed?; @closed; end
  end

  def test_zone_transfer_writes_complete_frames_and_reads_fragmented_frames
    transfer = Dnsruby::ZoneTransfer.new
    request = Dnsruby::Message.new('example.', 'AXFR')
    socket = TransferSocket.new(frame(request))
    transfer.send_message(socket, request)
    assert_equal(frame(request), socket.written)
    assert_equal(request.question, transfer.receive_message(socket).question)
  end

  def test_zone_transfer_accepts_multi_message_axfr_with_an_empty_later_question
    transfer = Dnsruby::ZoneTransfer.new
    soa = Dnsruby::RR.create('example. 300 IN SOA ns.example. hostmaster.example. 4 60 60 3600 60')
    address = Dnsruby::RR.create('www.example. 300 IN A 192.0.2.1')
    first = Dnsruby::Message.new('example.', 'AXFR')
    first.add_answer(soa)
    first.add_answer(address)
    last = Dnsruby::Message.new
    last.add_answer(soa.clone)
    responses = [first, last]
    socket = TransferSocket.new
    transfer.define_singleton_method(:send_message) do |_, request|
      responses.each { |response| response.header.id = request.header.id; response.header.qr = true }
    end
    transfer.define_singleton_method(:receive_message) { |_| responses.shift }
    Socket.stub(:tcp, socket) do
      assert_equal([soa, address], transfer.do_transfer('example.', '192.0.2.53'))
    end
    assert(socket.closed?)
  end

  def test_zone_transfer_timeout_configuration_and_interrupted_wait
    transfer = Dnsruby::ZoneTransfer.new
    assert_equal(30, transfer.io_timeout)
    assert_nil(transfer.transfer_timeout)
    transfer.io_timeout = 1
    transfer.transfer_timeout = 2
    assert_equal(1.0, transfer.io_timeout)
    assert_equal(2.0, transfer.transfer_timeout)
    transfer.transfer_timeout = nil
    assert_raises(ArgumentError) { transfer.io_timeout = 0 }
    assert_raises(ArgumentError) { transfer.transfer_timeout = Float::INFINITY }
    calls = 0
    IO.stub(:select, ->(*_) { calls += 1; raise Errno::EINTR if calls == 1; nil }) do
      transfer.send(:wait_for_io, Object.new, Dnsruby::TransportClock.now + 1, true)
    end
    assert_equal(2, calls)
    assert_raises(Dnsruby::ResolvTimeout) do
      transfer.tcp_read(TransferSocket.new, 2, Dnsruby::TransportClock.now - 1)
    end
  end

  def test_zone_transfer_matches_the_name_sent_in_the_question
    zones = ['example', 'example.', 'EXAMPLE', Dnsruby::Name.create('example').freeze,
      Dnsruby::Name.create('example.').freeze, '192.0.2.1']
    ['AXFR', 'IXFR'].each do |type|
      zones.each do |zone|
        owner = zone == '192.0.2.1' ? '1.2.0.192.in-addr.arpa.' : 'example.'
        soa = Dnsruby::RR.create("#{owner} 300 IN SOA ns.example. hostmaster.example. 4 60 60 3600 60")
        address = Dnsruby::RR.create("www.#{owner} 300 IN A 192.0.2.1")
        original_absolute = zone.absolute? if zone.is_a?(Dnsruby::Name)
        answers = if type == 'AXFR'
          [soa, address, soa.clone]
        else
          [soa, soa.clone.tap { |rr| rr.serial = 2 }, soa.clone, address, soa.clone]
        end
        result = transfer_with_answers(zone, type, owner, answers)
        if type == 'AXFR'
          assert_equal([soa, address], result, zone.inspect)
        else
          assert_equal([[2, 4]], result.map { |delta| [delta.start, delta.end] }, zone.inspect)
          assert_equal([soa, address], result.first.adds)
        end
        assert_equal(original_absolute, zone.absolute?) if zone.is_a?(Dnsruby::Name)
      end
    end
  end

  def test_zone_transfer_still_rejects_an_initial_soa_for_another_zone_or_class
    ['AXFR', 'IXFR'].each do |type|
      ['example', 'example.'].each do |zone|
        ['other. 300 IN', 'example. 300 CH'].each do |owner_and_class|
          soa = Dnsruby::RR.create("#{owner_and_class} SOA ns.example. hostmaster.example. 4 60 60 3600 60")
          error = assert_raises(Dnsruby::ResolvError) do
            transfer_with_answers(zone, type, 'example.', [soa])
          end
          assert_equal('initial SOA does not match transfer zone', error.message)
        end
      end
    end
  end

  def test_ixfr_terminal_soa_matches_the_opening_soa
    opening = Dnsruby::RR.create('example. 300 IN SOA ns.example. hostmaster.example. 4 60 60 3600 60')
    [opening.clone, opening.clone.tap { |rr| rr.refresh = 120 }].each do |closing|
      transfer = Dnsruby::ZoneTransfer.new
      transfer.instance_variable_set(:@zone_name, Dnsruby::Name.create('example.'))
      transfer.instance_variable_set(:@transfer_type, Dnsruby::Types.IXFR)
      transfer.instance_variable_set(:@serial, 2)
      transfer.instance_variable_set(:@state, :InitialSoa)
      # Two ordinary deltas, including their repeated intermediate SOA.
      old = opening.clone.tap { |rr| rr.serial = 2 }
      middle = opening.clone.tap { |rr| rr.serial = 3 }
      [opening, old, middle, middle.clone, opening.clone].each { |rr| transfer.parseRR(rr) }
      if closing.refresh == opening.refresh
        transfer.parseRR(closing)
        assert_equal(:End, transfer.instance_variable_get(:@state))
        assert_equal([[2, 3], [3, 4]], transfer.instance_variable_get(:@ixfr).map { |delta| [delta.start, delta.end] })
      else
        assert_raises(Dnsruby::ResolvError) { transfer.parseRR(closing) }
        assert_equal(:Ixfr_Add, transfer.instance_variable_get(:@state))
      end
    end
  end

  private

  def transfer_with_answers(zone, type, owner, answers)
    transfer = Dnsruby::ZoneTransfer.new
    transfer.transfer_type = type
    transfer.serial = 2
    response = Dnsruby::Message.new(owner, type)
    response.header.qr = true
    answers.each { |rr| response.add_answer!(rr) }
    socket = TransferSocket.new
    transfer.define_singleton_method(:send_message) do |_, request|
      response.header.id = request.header.id
    end
    transfer.define_singleton_method(:receive_message) { |_| Dnsruby::Message.decode(response.encode) }
    Socket.stub(:tcp, socket) { transfer.do_transfer(zone, '192.0.2.53') }
  ensure
    assert(socket.closed?) if socket
  end

  def frame(message)
    wire = message.encode
    [wire.bytesize].pack('n') + wire
  end

  def isolated(script)
    options = ''.frozen? ? ['--enable-frozen-string-literal'] : []
    stdout, stderr, status = Open3.capture3(RbConfig.ruby, *options, '-I',
      File.expand_path('../lib', __dir__), '-e', "#{TestPipelineSend::FIXTURE}\nTimeout.timeout(10) do\n#{script}\nend")
    assert(status.success?, "#{stdout}\n#{stderr}")
    assert_empty(stderr)
  end
end
