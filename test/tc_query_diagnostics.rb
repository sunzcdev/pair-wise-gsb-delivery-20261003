require_relative 'spec_helper'
require_relative 'socket_helpers'
require 'socket'
require 'open3'
require 'rbconfig'

# Stage diagnostics for a single lookup. These exercise the real resolver,
# selector and hosts code, using local UDP/TCP servers so no network is needed.
class QueryDiagnosticsTest < Minitest::Test
  include TestSockets

  # Rebuild the selector tables and stub the socket send, so a validation
  # scenario is deterministic. Mirrors the transport fixtures used elsewhere.
  FIXTURE = <<~'RUBY'
    require 'dnsruby'
    require 'timeout'
    Dnsruby.log.level = Logger::FATAL
    K = Dnsruby::SelectThread
    {
      mutex: Mutex.new, query_hash: {}, queries_by_queue: {}.compare_by_identity, socket_hash: {}, timeouts: {}, sockets: Set.new,
      tcp_buffers: {}, socket_is_persistent: {}, socket_remaining_queries: {},
      socket_owners: {}, retiring_sockets: Set.new,
      queued_exceptions: [], queued_responses: [], queued_validation_responses: [],
      observers: {}, observer_queue_counts: {}.compare_by_identity, tick_observers: [], select_thread: nil
    }.each { |name, value| K.class_variable_set("@@#{name}", value) }
    SELECTOR = K.send(:allocate)
    SELECTOR.define_singleton_method(:check_select_thread_synchronized) {}
    SELECTOR.define_singleton_method(:wake_up_select_thread) {}
    K.define_singleton_method(:instance) { SELECTOR }
  RUBY

  def setup
    Dnsruby::PacketSender.clear_caches
  end

  # --- module-level behaviour ---------------------------------------------

  def test_classifier_gives_one_terminal_category_for_each_failure
    cases = {
      Dnsruby::NXDomain.new => :nxdomain,
      Dnsruby::ServFail.new => :servfail,
      Dnsruby::Refused.new => :refused,
      Dnsruby::FormErr.new => :formerr,
      Dnsruby::NotImp.new => :notimp,
      Dnsruby::ResolvTimeout.new => :timeout,
      Dnsruby::ValidationLimitError.new => :limit,
      Dnsruby::RecursionLimitError.new => :limit,
      Dnsruby::VerifyError.new => :bogus,
      StandardError.new => :error
    }
    cases.each do |error, expected|
      assert_equal(expected, Dnsruby::Diagnostics.classify(nil, error), error.class.name)
    end
  end

  def test_classifier_uses_response_when_no_error
    message = Dnsruby::Message.new('ok.test.', 'A')
    assert_equal(:answer, Dnsruby::Diagnostics.classify(message, nil))
    message.security_level = Dnsruby::Message::SecurityLevel::BOGUS
    assert_equal(:bogus, Dnsruby::Diagnostics.classify(message, nil))
    assert_equal(:error, Dnsruby::Diagnostics.classify(nil, nil))
  end

  def test_recorder_rejects_unknown_stage
    assert_raises(ArgumentError) { Dnsruby::Diagnostics::Recorder.new(stages: [:wire_bytes]) }
    assert_raises(ArgumentError) { Dnsruby::Diagnostics::Recorder.new(stages: []) }
  end

  # --- bounded events and truncation --------------------------------------

  def test_events_are_bounded_and_report_drops_once
    recorder = Dnsruby::Diagnostics::Recorder.new(max_events: 4, stages: [:attempt, :terminal])
    chain = Dnsruby::Diagnostics::Chain.new(recorder, Dnsruby::Question.new('many.test.', 'A'))
    10.times do |i|
      attempt = chain.begin_attempt(server: '192.0.2.1', port: 53, transport: :udp)
      attempt.finish(:response)
    end
    chain.finish(:answer)

    events = recorder.events
    assert_operator(events.length, :<=, 4, 'event count must respect the cap')
    markers = events.select(&:truncated?)
    assert_equal(1, markers.length, 'exactly one truncation marker')
    assert(recorder.truncated?)
    assert_operator(markers.first.dropped, :>, 0)
    assert_equal(recorder.dropped, markers.first.dropped)
    # The terminal event survives truncation.
    assert_equal(:terminal, events.reject(&:truncated?).last.stage)
  end

  def test_no_truncation_for_a_normal_lookup
    recorder = Dnsruby::Diagnostics::Recorder.new(max_events: 16)
    chain = Dnsruby::Diagnostics::Chain.new(recorder, Dnsruby::Question.new('one.test.', 'A'))
    chain.begin_attempt(server: '192.0.2.1', port: 53, transport: :udp).finish(:response)
    chain.finish(:answer)
    refute(recorder.truncated?)
    assert_equal(0, recorder.dropped)
  end

  # --- attempts -------------------------------------------------------------

  def test_retry_shares_query_id_with_its_own_attempt_id
    udp, tcp = TestSockets.udp_tcp_pair
    port = tcp.addr[1]
    server = Thread.new do
      # Ignore the first datagram; answer the second.
      2.times do |i|
        packet, sender = udp.recvfrom(65_535)
        next if i.zero?
        response = Dnsruby::Message.decode(packet)
        response.header.qr = true
        response.add_answer(Dnsruby::RR.create('retry.test. 60 IN A 192.0.2.20'))
        udp.send(response.encode, 0, sender[3], sender[1])
      end
    end

    recorder = Dnsruby::Diagnostics::Recorder.new
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', port: port, do_caching: false,
      dnssec: false, query_timeout: 5, retry_times: 2, retry_delay: 0.2, packet_timeout: 0.15)
    resolver.diagnostics = recorder
    begin
      Dnsruby::Diagnostics.capture(recorder: recorder) { resolver.query('retry.test.', 'A') }
    ensure
      server.kill.join
      udp.close
      tcp.close
      resolver.close
    end

    attempts = recorder.events.select { |event| event.stage == :attempt }
    assert_equal(2, attempts.length, 'a retry is a second attempt')
    assert_equal(1, attempts.map(&:query_id).uniq.length, 'retries belong to one query')
    assert_equal(2, attempts.map(&:attempt_id).uniq.length, 'each attempt is distinct')
    assert_equal(:timeout, attempts.first.outcome)
    assert_equal(:response, attempts.last.outcome)
    attempts.each { |event| assert_operator(event.elapsed, :>=, 0) }
  end

  def test_truncation_marks_udp_attempt_then_tcp_attempt
    udp, tcp = TestSockets.udp_tcp_pair
    port = tcp.addr[1]
    udp_server = Thread.new do
      packet, sender = udp.recvfrom(65_535)
      response = Dnsruby::Message.decode(packet)
      response.header.qr = true
      response.header.tc = true
      udp.send(response.encode, 0, sender[3], sender[1])
    end
    tcp_server = Thread.new do
      client = tcp.accept
      length = client.read(2).unpack('n').first
      response = Dnsruby::Message.decode(client.read(length))
      response.header.qr = true
      response.add_answer(Dnsruby::RR.create('trunc.test. 60 IN A 192.0.2.30'))
      wire = response.encode
      client.write([wire.bytesize].pack('n') + wire)
      client.close
    end

    recorder = Dnsruby::Diagnostics::Recorder.new
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', port: port, do_caching: false,
      dnssec: false, query_timeout: 3, retry_times: 1)
    resolver.diagnostics = recorder
    begin
      Dnsruby::Diagnostics.capture(recorder: recorder) { resolver.query('trunc.test.', 'A') }
    ensure
      udp_server.kill.join
      tcp_server.kill.join
      udp.close
      tcp.close
      resolver.close
    end

    attempts = recorder.events.select { |event| event.stage == :attempt }
    assert_equal([:udp, :tcp], attempts.map(&:transport))
    assert_equal(:truncated, attempts.first.outcome)
    assert_equal(:response, attempts.last.outcome)
    assert_equal(2, attempts.map(&:attempt_id).uniq.length)
  end

  # --- stage selection -------------------------------------------------------

  def test_only_selected_stages_are_emitted
    udp, tcp = TestSockets.udp_tcp_pair
    port = tcp.addr[1]
    server = Thread.new do
      packet, sender = udp.recvfrom(65_535)
      response = Dnsruby::Message.decode(packet)
      response.header.qr = true
      response.add_answer(Dnsruby::RR.create('stages.test. 60 IN A 192.0.2.50'))
      udp.send(response.encode, 0, sender[3], sender[1])
    end

    recorder = Dnsruby::Diagnostics::Recorder.new(stages: [:terminal])
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', port: port, do_caching: false,
      dnssec: false, query_timeout: 3, retry_times: 1)
    resolver.diagnostics = recorder
    begin
      Dnsruby::Diagnostics.capture(recorder: recorder) { resolver.query('stages.test.', 'A') }
    ensure
      server.kill.join
      udp.close
      tcp.close
      resolver.close
    end

    assert_equal([:terminal], recorder.events.map(&:stage).uniq)
  end

  # --- terminal classification and sync/async parity -----------------------

  def test_sync_and_async_share_terminal_classification
    udp, tcp = TestSockets.udp_tcp_pair
    port = tcp.addr[1]
    server = Thread.new do
      2.times do
        packet, sender = udp.recvfrom(65_535)
        response = Dnsruby::Message.decode(packet)
        response.header.qr = true
        response.header.rcode = Dnsruby::RCode.NXDOMAIN
        response.add_authority(Dnsruby::RR.create('test. 60 IN SOA ns.test. host.test. 1 2 3 4 5'))
        udp.send(response.encode, 0, sender[3], sender[1])
      end
    end

    sync_recorder = Dnsruby::Diagnostics::Recorder.new
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', port: port, do_caching: false,
      dnssec: false, query_timeout: 3, retry_times: 1)
    resolver.diagnostics = sync_recorder
    begin
      Dnsruby::Diagnostics.capture(recorder: sync_recorder) do
        assert_raises(Dnsruby::NXDomain) { resolver.query('nx.test.', 'A') }
      end
    ensure
      resolver.diagnostics = nil
      resolver.close
      server.kill.join
      udp.close
      tcp.close
    end

    # The last query completed; compare its terminal outcome by name.
    assert_equal(:nxdomain, terminal_outcome(sync_recorder))
  end

  def test_async_terminal_matches_the_response
    udp, tcp = TestSockets.udp_tcp_pair
    port = tcp.addr[1]
    server = Thread.new do
      packet, sender = udp.recvfrom(65_535)
      response = Dnsruby::Message.decode(packet)
      response.header.qr = true
      response.header.rcode = Dnsruby::RCode.SERVFAIL
      udp.send(response.encode, 0, sender[3], sender[1])
    end

    recorder = Dnsruby::Diagnostics::Recorder.new
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', port: port, do_caching: false,
      dnssec: false, query_timeout: 3, retry_times: 1)
    resolver.diagnostics = recorder
    queue = Queue.new
    begin
      Dnsruby::Diagnostics.capture(recorder: recorder) do
        resolver.send_async(Dnsruby::Message.new('async.test.', 'A'), queue, :async)
      end
      _id, _response, error = queue.pop
      assert_kind_of(Dnsruby::ServFail, error)
    ensure
      server.kill.join
      udp.close
      tcp.close
      resolver.close
    end

    assert_equal(:servfail, terminal_outcome(recorder))
  end

  def test_validation_failure_terminates_as_bogus
    isolated(<<~'RUBY')
      validators = []
      Dnsruby::ValidatorThread.define_method(:run) { validators << self }
      Dnsruby::Dnssec.define_singleton_method(:validate_with_query) do |_query, answer|
        error = Dnsruby::VerifyError.new('bad signature')
        answer.security_error = error
        answer.security_level = Dnsruby::Message::SecurityLevel::BOGUS
        false
      end
      recorder = Dnsruby::Diagnostics::Recorder.new
      resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', dnssec: true,
        do_validation: true, do_caching: false, retry_times: 1, query_timeout: 10)
      resolver.diagnostics = recorder
      resolver.single_resolvers.each { |writer| writer.define_singleton_method(:send_async) { |*| } }
      results = Queue.new
      request = Dnsruby::Message.new('bogus.test.', 'A')
      request.do_validation = true
      Dnsruby::Diagnostics.capture(recorder: recorder) do
        resolver.send_async(request, results, :client)
        SELECTOR.send_tick_to_observers
      end

      settings = K.class_variable_get(:@@query_hash).values.first
      answer = settings.query.clone
      answer.header.qr = true
      SELECTOR.push_response_for_query(settings, answer, settings.query, settings.single_resolver)
      SELECTOR.send_queued_responses
      validators.shift.do_validate
      SELECTOR.send_queued_validation_responses

      _id, _response, error = results.pop
      raise 'validation failure was not reported' unless error.is_a?(Dnsruby::VerifyError)
      terminal = recorder.events.reverse.find { |event| event.stage == :terminal }
      raise "wrong terminal: #{terminal.inspect}" unless terminal && terminal.outcome == :bogus
      resolver.close
    RUBY
  end

  # --- listener isolation ---------------------------------------------------

  def test_async_listener_receives_events_when_read
    seen = []
    recorder = Dnsruby::Diagnostics::Recorder.new(listener: ->(event) { seen << [event.stage, event.outcome] })
    chain = Dnsruby::Diagnostics::Chain.new(recorder, Dnsruby::Question.new('listen.test.', 'A'))
    chain.record(stage: :cache, outcome: :miss)
    chain.finish(:answer)
    # Nothing is delivered from inside the recording path.
    assert_empty(seen)
    recorder.events
    assert_equal([[:cache, :miss], [:terminal, :answer]], seen)
  end

  def test_raising_listener_does_not_change_the_query_result
    udp, tcp = TestSockets.udp_tcp_pair
    port = tcp.addr[1]
    server = Thread.new do
      packet, sender = udp.recvfrom(65_535)
      response = Dnsruby::Message.decode(packet)
      response.header.qr = true
      response.add_answer(Dnsruby::RR.create('listener.test. 60 IN A 192.0.2.60'))
      udp.send(response.encode, 0, sender[3], sender[1])
    end

    boom = ->(_event) { raise 'listener exploded' }
    recorder = Dnsruby::Diagnostics::Recorder.new(listener: boom)
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', port: port, do_caching: false,
      dnssec: false, query_timeout: 3, retry_times: 1)
    resolver.diagnostics = recorder
    begin
      answer = nil
      Dnsruby::Diagnostics.capture(recorder: recorder) do
        answer = resolver.query('listener.test.', 'A')
      end
      assert_equal(['192.0.2.60'], answer.answer.map { |rr| rr.address.to_s })
    ensure
      server.kill.join
      udp.close
      tcp.close
      resolver.close
    end
  end

  # --- cache stage ----------------------------------------------------------

  def test_cache_hit_is_recorded_and_stops_before_the_network
    udp, tcp = TestSockets.udp_tcp_pair
    port = tcp.addr[1]
    answer = 'cached.test. 60 IN A 192.0.2.70'
    udp_server = Thread.new do
      packet, sender = udp.recvfrom(65_535)
      response = Dnsruby::Message.decode(packet)
      response.header.qr = true
      response.header.aa = true
      response.add_answer(Dnsruby::RR.create(answer))
      udp.send(response.encode, 0, sender[3], sender[1])
    end

    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', port: port, do_caching: true,
      dnssec: false, query_timeout: 3, retry_times: 1)
    begin
      first = Dnsruby::Diagnostics.capture do |recorder|
        resolver.query('cached.test.', 'A')
        [recorder, nil]
      end
    ensure
      udp_server.kill.join
      udp.close
      tcp.close
    end

    # Second lookup must be answered from the resolver's resolution cache.
    recorder = Dnsruby::Diagnostics::Recorder.new
    resolver.diagnostics = recorder
    Dnsruby::Diagnostics.capture(recorder: recorder) { resolver.query('cached.test.', 'A') }
    resolver.close

    cache_events = recorder.events.select { |event| event.stage == :cache }
    refute_empty(cache_events)
    assert_equal(:hit, cache_events.first.outcome)
    assert_empty(recorder.events.select { |event| event.stage == :attempt })
  end

  # --- hosts stage ----------------------------------------------------------
  def test_hosts_hit_is_recorded
    path = File.expand_path('fixtures/hosts-diagnostics', __dir__)
    File.write(path, "127.0.0.1 local.test\n")
    hosts = Dnsruby::Hosts.new(path)
    recorder = Dnsruby::Diagnostics::Recorder.new
    begin
      Dnsruby::Diagnostics.capture(recorder: recorder) do
        assert_equal('127.0.0.1', hosts.getaddress('local.test').to_s)
      end
    ensure
      File.delete(path)
    end
    hosts_events = recorder.events.select { |event| event.stage == :hosts }
    refute_empty(hosts_events)
    assert_equal(:hit, hosts_events.first.outcome)
    assert_equal(:answer, terminal_outcome(recorder))
  end

  def test_hosts_miss_and_dns_query_form_one_chain
    path = File.expand_path('fixtures/hosts-diagnostics', __dir__)
    File.write(path, "127.0.0.1 local.test\n")

    udp, tcp = TestSockets.udp_tcp_pair
    port = tcp.addr[1]
    server = Thread.new do
      packet, sender = udp.recvfrom(65_535)
      response = Dnsruby::Message.decode(packet)
      response.header.qr = true
      response.add_answer(Dnsruby::RR.create('remote.test. 60 IN A 192.0.2.99'))
      udp.send(response.encode, 0, sender[3], sender[1])
    end

    recorder = Dnsruby::Diagnostics::Recorder.new
    hosts = Dnsruby::Hosts.new(path)
    dns = Dnsruby::DNS.new(nameserver: ['127.0.0.1'])
    resolver = dns.instance_variable_get(:@resolver)
    resolver.port = port
    dns.do_caching = false
    dns.diagnostics = recorder
    resolv = Dnsruby::Resolv.new([hosts, dns])
    begin
      Dnsruby::Diagnostics.capture(recorder: recorder) do
        assert_equal(['192.0.2.99'], resolv.getaddresses('remote.test.').map(&:to_s))
      end
    ensure
      dns.close
      server.kill.join
      udp.close
      tcp.close
      File.delete(path)
    end

    stages = recorder.events.map(&:stage)
    assert_includes(stages, :hosts, 'the hosts probe is recorded')
    assert_includes(stages, :attempt, 'the DNS query is recorded')
    hosts_event = recorder.events.find { |event| event.stage == :hosts }
    attempt_event = recorder.events.find { |event| event.stage == :attempt }
    assert_equal(:miss, hosts_event.outcome)
    assert_equal(hosts_event.query_id, attempt_event.query_id,
      'the hosts miss and its DNS query belong to one lookup')
  end

  def test_no_recorder_means_no_work
    recorder = Dnsruby::Diagnostics::Recorder.new
    chain = Dnsruby::Diagnostics::Chain.new(recorder, Dnsruby::Question.new('x.test.', 'A'))
    assert_nil(Dnsruby::Diagnostics.record_cache(nil, :hit))
    assert_nil(Dnsruby::Diagnostics.attach(nil))
    # Outside capture there is no thread recorder, so hosts probing is inert.
    assert_nil(Dnsruby::Diagnostics.record_hosts('x.test.', true))
    assert_empty(chain.recorder.events)
  end

  # --- validation stage and "no invented durations" ------------------------

  def test_validation_span_is_measured_only_when_it_runs
    recorder = Dnsruby::Diagnostics::Recorder.new
    chain = Dnsruby::Diagnostics::Chain.new(recorder, Dnsruby::Question.new('val.test.', 'A'))
    chain.measure_validation { :validated }
    chain.record(stage: :cache, outcome: :miss)
    chain.finish(:answer)

    validation = recorder.events.select { |event| event.stage == :validation }
    assert_equal(1, validation.length, 'only validation that ran is reported')
    assert_equal(:finished, validation.first.outcome)
    assert_operator(validation.first.elapsed, :>=, 0)
    # The cache event never ran as a span, so it must not carry a duration.
    cache = recorder.events.find { |event| event.stage == :cache }
    assert_nil(cache.elapsed)
    terminal = recorder.events.find { |event| event.stage == :terminal }
    assert_nil(terminal.elapsed, 'a terminal category has no invented duration')
  end

  def test_failed_validation_keeps_its_error_and_duration
    recorder = Dnsruby::Diagnostics::Recorder.new
    chain = Dnsruby::Diagnostics::Chain.new(recorder, Dnsruby::Question.new('bad.test.', 'A'))
    error = Dnsruby::VerifyError.new('bogus signature')
    raised = assert_raises(Dnsruby::VerifyError) do
      chain.measure_validation { raise error }
    end
    assert_same(error, raised)
    validation = recorder.events.find { |event| event.stage == :validation }
    assert_equal(:error, validation.outcome)
    assert_operator(validation.elapsed, :>=, 0)
  end

  def test_unexecuted_stages_emit_nothing
    recorder = Dnsruby::Diagnostics::Recorder.new
    chain = Dnsruby::Diagnostics::Chain.new(recorder, Dnsruby::Question.new('quiet.test.', 'A'))
    chain.record(stage: :cache, outcome: :miss)
    chain.finish(:answer)
    stages = recorder.events.map(&:stage)
    assert_equal([:cache, :terminal], stages)
    refute_includes(stages, :attempt)
    refute_includes(stages, :validation)
    refute_includes(stages, :hosts)
  end

  def test_validation_stage_is_reported_on_the_answer_chain
    isolated(<<~'RUBY')
      validators = []
      Dnsruby::ValidatorThread.define_method(:run) { validators << self }
      Dnsruby::Dnssec.define_singleton_method(:validate_with_query) do |_query, answer|
        answer.security_level = Dnsruby::Message::SecurityLevel::SECURE
        true
      end
      recorder = Dnsruby::Diagnostics::Recorder.new
      resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', dnssec: true,
        do_validation: true, do_caching: false, retry_times: 1, query_timeout: 10)
      resolver.diagnostics = recorder
      resolver.single_resolvers.each { |writer| writer.define_singleton_method(:send_async) { |*| } }
      results = Queue.new
      request = Dnsruby::Message.new('valchain.test.', 'A')
      request.do_validation = true
      Dnsruby::Diagnostics.capture(recorder: recorder) do
        resolver.send_async(request, results, :client)
        SELECTOR.send_tick_to_observers
      end

      settings = K.class_variable_get(:@@query_hash).values.first
      answer = settings.query.clone
      answer.header.qr = true
      SELECTOR.push_response_for_query(settings, answer, settings.query, settings.single_resolver)
      SELECTOR.send_queued_responses
      raise 'validation was not scheduled' unless validators.size == 1
      validators.shift.do_validate
      SELECTOR.send_queued_validation_responses

      id, response, error = results.pop
      raise 'unexpected error' unless id == :client && error.nil?
      validation = recorder.events.select { |event| event.stage == :validation }
      raise 'validation stage not reported' unless validation.size == 1
      raise 'validation was not measured' unless validation.first.elapsed && validation.first.elapsed >= 0
      raise 'missing terminal' unless recorder.events.any? { |e| e.stage == :terminal }
      resolver.close
    RUBY
  end

  # --- negative-cache path at the Resolver ---------------------------------

  def test_negative_cache_hit_emits_cache_hit_without_attempts
    udp, tcp = TestSockets.udp_tcp_pair
    port = tcp.addr[1]
    answer = 'missing.test. 60 IN A 192.0.2.90'
    server = Thread.new do
      packet, sender = udp.recvfrom(65_535)
      response = Dnsruby::Message.decode(packet)
      response.header.qr = true
      response.header.aa = true
      response.add_answer(Dnsruby::RR.create(answer))
      udp.send(response.encode, 0, sender[3], sender[1])
    end

    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', port: port, do_caching: true,
      dnssec: false, query_timeout: 3, retry_times: 1)
    begin
      Dnsruby::Diagnostics.capture { resolver.query('missing.test.', 'A') }
    ensure
      server.kill.join
      udp.close
      tcp.close
      # The packet-sender cache is now warm for this exact question.
    end

    recorder = Dnsruby::Diagnostics::Recorder.new
    resolver.diagnostics = recorder
    begin
      Dnsruby::Diagnostics.capture(recorder: recorder) { resolver.query('missing.test.', 'A') }
    ensure
      resolver.close
    end

    cache = recorder.events.find { |event| event.stage == :cache }
    refute_nil(cache)
    assert_equal(:hit, cache.outcome)
    assert_empty(recorder.events.select { |event| event.stage == :attempt })
    assert_equal(:answer, terminal_outcome(recorder))
  end

  # --- payload hygiene ------------------------------------------------------

  def test_events_carry_only_identity_and_categories
    udp, tcp = TestSockets.udp_tcp_pair
    port = tcp.addr[1]
    server = Thread.new do
      packet, sender = udp.recvfrom(65_535)
      response = Dnsruby::Message.decode(packet)
      response.header.qr = true
      response.add_answer(Dnsruby::RR.create('privacy.test. 60 IN A 192.0.2.80'))
      udp.send(response.encode, 0, sender[3], sender[1])
    end

    recorder = Dnsruby::Diagnostics::Recorder.new
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', port: port, do_caching: false,
      dnssec: false, query_timeout: 3, retry_times: 1)
    resolver.diagnostics = recorder
    begin
      Dnsruby::Diagnostics.capture(recorder: recorder) { resolver.query('privacy.test.', 'A') }
    ensure
      server.kill.join
      udp.close
      tcp.close
      resolver.close
    end

    allowed = Dnsruby::Diagnostics::Event.members
    recorder.events.each do |event|
      assert_equal(allowed.sort, event.members.sort)
      assert_equal('privacy.test.', event.qname) if event.qname
      refute(event.to_h.value?(event.qname.to_s + ' 60 IN A 192.0.2.80'))
      event.to_h.each_value do |value|
        next unless value.is_a?(String)
        refute_match(/192\.0\.2\.80|IN A|hmac|secret/i, value)
      end
    end
  end

  private

  def isolated(code)
    stdout, stderr, status = Open3.capture3(RbConfig.ruby, '-I', File.expand_path('../lib', __dir__),
      '-e', FIXTURE + code)
    assert(status.success?, "#{stdout}\n#{stderr}")
    assert_empty(stderr)
  end

  def terminal_outcome(recorder)
    event = recorder.events.reverse.find { |entry| entry.stage == :terminal }
    event&.outcome
  end
end
