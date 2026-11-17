require_relative 'tc_release_transport'
require 'minitest/mock'

class ReleaseConfigurationFixesTest < Minitest::Test
  def test_rejected_port_pool_preserves_parent_and_sender
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', src_port: [60_001, 60_002])
    assert_raises(ArgumentError) { resolver.src_port = [] }
    assert_equal([60_001, 60_002], resolver.src_port)
    assert_equal([60_001, 60_002], resolver.single_resolvers.first.src_port)
    resolver.port = 1053
    assert_equal(1053, resolver.single_resolvers.first.port)
  ensure
    resolver&.close
  end

  def test_zero_delay_keeps_and_dispatches_all_servers_and_retries
    isolated(<<~'SCRIPT')
      resolver = Dnsruby::Resolver.new(nameserver: ['127.0.0.1', '127.0.0.2'],
        retry_delay: 0, retry_times: 3, query_timeout: 1)
      plan = resolver.generate_timeouts(100.0)
      raise 'different deadlines' unless plan.keys == [100.0]
      raise 'attempt lost' unless plan[100.0].length == 6
      sent = []
      resolver.single_resolvers.each do |sender|
        sender.define_singleton_method(:prepare_send_async) do |_message, _queue, id, *_|
          -> { sent << [server, id.last] }
        end
      end
      resolver.send_async(query, Queue.new, :zero_delay)
      expected = ['127.0.0.1', '127.0.0.2'].product([0, 1, 2])
      raise 'simultaneous attempt overwritten' unless sent.sort == expected.sort
      resolver.close
    SCRIPT
  end

  def test_configured_pool_tries_each_port_and_reports_exhaustion_once
    isolated(<<~'SCRIPT')
      SecureRandom.define_singleton_method(:random_number) { |_| 0 }
      [false, true].product([false, true]).each do |tcp, all_busy|
        attempts, sockets = [], []
        if tcp
          Dnsruby::PacketSender.define_method(:connect_tcp) do |_server, _port, _address, source_port, *_|
            attempts << source_port
            raise Errno::EADDRINUSE if all_busy || source_port == 60_001
            FrameSocket.new.tap { |socket| sockets << socket }
          end
        else
          UDPSocket.define_singleton_method(:new) do |*|
            FrameSocket.new.tap do |socket|
              socket.define_singleton_method(:bind) do |_address, source_port|
                attempts << source_port
                raise Errno::EADDRINUSE if all_busy || source_port == 60_001
              end
              socket.define_singleton_method(:connect) { |*| }
              sockets << socket
            end
          end
        end
        writer = Dnsruby::PacketSender.new(server: '127.0.0.1', use_tcp: tcp,
          src_port: [60_001, 60_002], dnssec: false)
        queue = Queue.new
        writer.send_async(query, queue, :pool)
        drain_errors
        raise 'pool did not try distinct ports' unless attempts == [60_001, 60_002]
        if all_busy
          raise 'missing or duplicate failure' unless queue.length == 1 && queue.pop.last.is_a?(IOError)
        else
          raise 'free candidate did not send' if sockets.last.written.empty?
          raise 'collision completed successful query' unless queue.empty?
          SELECTOR.remove_id(:pool)
        end
        writer.close
        sockets.each { |socket| raise 'socket retained' unless socket.closed?; assert_clean(socket) }
      end
    SCRIPT
  end

  def test_random_port_retry_keeps_its_original_mode_after_configuration_changes
    isolated(<<~'SCRIPT')
      SecureRandom.define_singleton_method(:random_number) { |_| 7 }
      attempts = []
      writer = Dnsruby::PacketSender.new(server: '127.0.0.1', use_tcp: true, dnssec: false)
      socket = FrameSocket.new
      writer.define_singleton_method(:connect_tcp) do |_server, _port, _address, source_port, *_|
        attempts << source_port
        if attempts.length == 1
          self.src_port = 60_001
          raise Errno::EADDRINUSE
        end
        socket
      end
      writer.send_async(query, Queue.new, :snapshot)
      raise 'retry adopted a different port pool' unless attempts == [50_007, 50_007]
      SELECTOR.remove_id(:snapshot)
      writer.close
    SCRIPT
  end

  def test_pipeline_port_metadata_tracks_the_complete_captured_pool
    isolated(<<~'SCRIPT')
      SecureRandom.define_singleton_method(:random_number) { |limit| limit == 15_536 ? 7 : 0 }
      [0, [60_001, 60_002]].product([false, true]).each do |ports, replace|
        attempts, sockets = [], []
        writer = Dnsruby::PacketSender.new(server: '127.0.0.1', use_tcp: true,
          tcp_pipelining: true, src_port: ports, dnssec: false)
        writer.define_singleton_method(:connect_tcp) do |_server, _port, _address, source_port, *_|
          attempts << source_port
          if attempts.length == 1
            self.src_port = 60_003 if replace
            raise Errno::EADDRINUSE
          end
          FrameSocket.new.tap do |socket|
            socket.define_singleton_method(:connect) { |_| raise Errno::EISCONN }
            sockets << socket
          end
        end
        3.times do |id|
          message = query("snapshot#{id}.example.")
          message.header.id = id
          writer.send_async(message, Queue.new, id)
          if id == 0
            expected = ports == 0 ? [50_007, 50_007] : [60_001, 60_002]
            raise 'retry changed its captured pool' unless attempts == expected
          end
        end
        if replace
          raise 'new query reused the old source binding' unless attempts.last == 60_003 && attempts.length == 3
          raise 'replacement pipeline was not reused' unless sockets.length == 2
        else
          raise 'depleted candidates changed pipeline identity' unless attempts.length == 2 && sockets.length == 1
        end
        3.times { |id| SELECTOR.remove_id(id) }
        writer.close
        sockets.each { |socket| raise 'socket retained' unless socket.closed?; assert_clean(socket) }
      end
    SCRIPT
  end

  def test_recursive_hops_inherit_transport_settings_but_not_upstream_tsig
    with_recursion_state do
      parent = Dnsruby::Resolver.new(nameserver: '127.0.0.1', port: 1053, use_tcp: true,
        query_timeout: 0.25, packet_timeout: 0.1, retry_times: 2, retry_delay: 0.02,
        src_address: '127.0.0.1', src_port: [60_001, 60_002], udp_size: 1400)
      parent.tsig = ['client-only.example.', 'c2VjcmV0', 'hmac-sha256']
      recursor = Dnsruby::Recursor.new(parent)
      response = Dnsruby::Message.new('www.example.', 'A')
      response.header.aa = true
      hop = Struct.new(:dnssec, :recurse, :closed, :response) do
        def send_message(_message); response; end
        def close; self.closed = true; end
      end.new(nil, nil, false, response)
      options = nil
      Dnsruby::Resolver.stub(:new, ->(settings) { options = settings; hop }) do
        assert_same(response, recursor._dorecursion('www.example.', 'A', 'IN', '.', root_authorities, 0, true))
      end
      {port: 1053, use_tcp: true, query_timeout: 0.25, packet_timeout: 0.1,
        retry_times: 2, retry_delay: 0.02, src_address: '127.0.0.1',
        src_port: [60_001, 60_002], udp_size: 1400}.each { |key, value| assert_equal(value, options[key]) }
      refute_same(parent.src_port, options[:src_port])
      refute(options.key?(:tsig))
      assert_equal(true, hop.closed)
    ensure
      parent&.close
    end
  end

  def test_recursive_deadline_covers_bootstrap_and_preserves_inherited_deadline
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', query_timeout: 0.02)
    recursor = Dnsruby::Recursor.new(resolver)
    wait = ->(*) { Dnsruby::QueryControl.current.pop(Dnsruby::QueryControl::Queue.new) }
    Dnsruby::Recursor.stub(:ensure_hints, wait) do
      assert_raises(Dnsruby::ResolvTimeout) { recursor.query('www.example.', 'A', 'IN', true) }
      resolver.query_timeout = 1
      parent = Dnsruby::QueryControl.new(deadline: Dnsruby::TransportClock.now + 0.02)
      assert_raises(Dnsruby::QueryControl::DeadlineExceeded) do
        Dnsruby::QueryControl.with(parent) { recursor.query('www.example.', 'A', 'IN', true) }
      end
    end
    assert_nil(Dnsruby::QueryControl.current)
  ensure
    resolver&.close
  end

  def test_no_glue_referral_keeps_address_discovery_in_the_recursive_query
    with_no_glue_referral do |run, calls, hops|
      response = run.call
      assert_equal('192.0.2.1', response.answer.first.address.to_s)
      assert_equal(['www.child.example.', 'ns.external.', 'www.child.example.'], calls.map(&:first))
      assert_equal(1, calls.map { |_, _, control, _| control.deadline }.uniq.length)
      assert_equal(1, calls.map { |_, _, _, budget| budget.object_id }.uniq.length)
      calls.each do |_, settings, control, budget|
        refute_nil(control)
        refute_nil(budget)
        assert_equal(1053, settings[:port])
        assert_equal(true, settings[:use_tcp])
        assert_equal('127.0.0.1', settings[:src_address])
        assert_equal(60_001, settings[:src_port])
        assert(settings[:nameserver].all? { |server| server.start_with?('192.0.2.') })
      end
      assert(hops.all?(&:closed))
    end
  end

  def test_no_glue_address_discovery_obeys_the_overall_deadline
    with_no_glue_referral(wait_for_deadline: true) do |run, calls, hops|
      assert_raises(Dnsruby::ResolvTimeout) { run.call }
      assert_equal(['www.child.example.', 'ns.external.'], calls.map(&:first))
      assert(hops.all?(&:closed))
      assert_nil(Dnsruby::QueryControl.current)
    end
  end

  def test_mixed_referral_discovers_missing_addresses_when_glue_servers_fail
    [Dnsruby::ResolvTimeout, IOError, Dnsruby::ServFail, Dnsruby::Refused,
      Dnsruby::NotImp, Dnsruby::FormErr, Dnsruby::SocketEofResolvError].each do |error|
      with_no_glue_referral(glue_error: error.new) do |run, calls, hops|
        response = run.call
        assert_equal('192.0.2.1', response.answer.first.address.to_s)
        assert_equal([['192.0.2.53'], ['192.0.2.55'], ['192.0.2.53'], ['192.0.2.54']],
          calls.map { |_, settings, _, _| settings[:nameserver] })
        assert(hops.all?(&:closed))
      end
    end
  end

  def test_mixed_referral_retains_original_error_when_missing_addresses_are_unavailable
    original = Dnsruby::Refused.new('addressed server refused')
    with_no_glue_referral(glue_error: original, address_error: Dnsruby::ServFail.new) do |run, calls, hops|
      assert_same(original, assert_raises(Dnsruby::Refused) { run.call })
      assert_equal(['www.child.example.', 'www.child.example.', 'ns.external.'], calls.map(&:first))
      assert(hops.all?(&:closed))
    end
  end

  def test_mixed_referral_does_not_hide_validation_failure_during_address_discovery
    failure = Dnsruby::VerifyError.new('address validation failed')
    with_no_glue_referral(glue_error: Dnsruby::Refused.new, address_error: failure) do |run, calls, hops|
      assert_same(failure, assert_raises(Dnsruby::VerifyError) { run.call })
      assert_equal(['www.child.example.', 'www.child.example.', 'ns.external.'], calls.map(&:first))
      assert(hops.all?(&:closed))
    end
  end

  def test_mixed_referral_keeps_terminal_errors_terminal
    [Dnsruby::NXDomain, Dnsruby::VerifyError, Dnsruby::ValidationLimitError,
      Dnsruby::QueryControl::Cancelled, RuntimeError].each do |error|
      with_no_glue_referral(glue_error: error.new) do |run, calls, hops|
        assert_raises(error) { run.call }
        assert_equal(['www.child.example.', 'www.child.example.'], calls.map(&:first))
        assert(hops.all?(&:closed))
      end
    end
  end

  def test_early_recursive_cancellation_preserves_outer_cache_generation
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1')
    recursor = Dnsruby::Recursor.new(resolver)
    previous = Thread.current[:dnsruby_recursor_generation]
    Thread.current[:dnsruby_recursor_generation] = :outer_query
    control = Dnsruby::QueryControl.new
    control.cancel
    Thread.current[:dnsruby_query_control] = control
    assert_raises(Dnsruby::QueryControl::Cancelled) { recursor.query('www.example.') }
    assert_equal(:outer_query, Thread.current[:dnsruby_recursor_generation])
  ensure
    Thread.current[:dnsruby_recursor_generation] = previous
    Thread.current[:dnsruby_query_control] = nil
    resolver&.close
  end

  def test_hints_reset_keeps_an_active_query_usable_without_repopulating_old_cache
    with_recursion_state do
      klass = Dnsruby::Recursor
      root = root_authorities
      klass.class_variable_set(:@@hints, root)
      klass.class_variable_set(:@@zones_cache, {'.' => root})
      klass.class_variable_set(:@@stale_hints, nil)
      resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', do_caching: false)
      recursor = klass.new(resolver)
      original = recursor.method(:get_closest_known_zone_authorities_for)
      entered, resume = Queue.new, Queue.new
      recursor.define_singleton_method(:get_closest_known_zone_authorities_for) do |name|
        entered << true
        resume.pop
        original.call(name)
      end
      recursor.define_singleton_method(:_dorecursion) do |*|
        klass.cache_zone('old.example.', root, 60)
        klass.cache_authority('old.example.', root.values.first)
        Dnsruby::Message.new('www.example.')
      end
      worker = Thread.new { recursor.query('www.example.', 'A', 'IN', true) }
      entered.pop
      klass.reset_hints
      resume << true
      assert_instance_of(Dnsruby::Message, worker.value)
      refute(klass.class_variable_get(:@@zones_cache).key?('old.example.'))
      refute(klass.class_variable_get(:@@authority_cache).key?('old.example.'))
    ensure
      resume << true if resume
      worker&.join
      resolver&.close
    end
  end

  def test_long_lived_recursor_refreshes_expired_root_addresses
    with_recursion_state do
      klass = Dnsruby::Recursor
      clock = 1_800_000_000
      Time.stub(:now, -> { Time.at(clock) }) do
        addresses = klass::AddressCache.new
        addresses.push(['192.0.2.53', 1])
        root = {'ns.root' => addresses}
        klass.class_variable_set(:@@hints, root)
        klass.class_variable_set(:@@zones_cache, {'.' => root})
        klass.class_variable_set(:@@stale_hints, nil)
        resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', query_timeout: 1, do_caching: false)
        recursor = klass.new(resolver)
        used_addresses = []
        recursor.define_singleton_method(:_dorecursion) do |name, type, _klass, _zone, authorities, *_|
          used_addresses << authorities.values.flat_map(&:values)
          Dnsruby::Message.new(name, type)
        end
        lookups = 0
        bootstrap = lambda do |*|
          lookups += 1
          assert_instance_of(Dnsruby::QueryControl, Dnsruby::QueryControl.current)
          refute(klass.class_variable_get(:@@mutex).owned?)
          packet = Dnsruby::Message.new('.', 'NS')
          packet.add_answer(Dnsruby::RR.create('. 300 IN NS ns.root.'))
          packet.add_additional(Dnsruby::RR.create('ns.root. 300 IN A 192.0.2.54'))
          packet
        end
        resolver.stub(:query_no_validation_or_recursion, bootstrap) do
          recursor.query('www.example.', 'A', 'IN', true)
          clock += 1
          recursor.query('www.example.', 'A', 'IN', true)
          recursor.query('www.example.', 'A', 'IN', true)
        end
        assert_equal(1, lookups)
        assert_equal([['192.0.2.53'], ['192.0.2.54'], ['192.0.2.54']], used_addresses)
      ensure
        resolver&.close
      end
    end
  end

  def test_default_recursor_uses_current_hint_addresses_without_resolving_owner_names
    with_recursion_state do
      klass = Dnsruby::Recursor
      root = root_authorities
      klass.class_variable_set(:@@hints, root)
      klass.class_variable_set(:@@stale_hints, nil)
      klass.class_variable_set(:@@nameservers, root.keys)
      configured = []
      factory = ->(*args) { configured << args; Struct.new(:dnssec).new }
      Dnsruby::Resolver.stub(:new, factory) do
        klass.new
        assert_equal([[{nameserver: ['192.0.2.53']}]], configured)
        klass.reset_hints
        klass.new
        assert_equal([], configured.last)
        # Expired hints also leave the normal Resolver bootstrap in place.
        root.values.first.push(['192.0.2.53', 0])
        klass.class_variable_set(:@@stale_hints, nil)
        klass.new
        assert_equal([], configured.last)
      end
    end
  end

  def test_expired_root_addresses_refresh_during_delegated_server_discovery
    [false, true].each do |reset|
      with_root_expiry_during_lookup(reset: reset) do |run, trace|
        response = run.call
        assert_equal('192.0.2.1', response.answer.first.address.to_s)
        assert_equal(['www.child.example.', 'ns.external.', 'www.child.example.'],
          trace[:calls].map(&:first))
        assert_equal([['192.0.2.53'], ['192.0.2.54'], ['192.0.2.55']],
          trace[:calls].map { |_, settings, *_| settings[:nameserver] })
        assert_equal(1, trace[:bootstraps].length)
        operations = trace[:calls] + trace[:bootstraps]
        assert_equal(1, operations.map { |_, _, control, _| control.deadline }.uniq.length)
        assert_equal(1, operations.map { |_, _, _, budget| budget.object_id }.uniq.length)
        assert_equal([trace[:generation]], operations.map { |_, _, _, _, generation| generation }.uniq)
        trace[:calls].each do |_, settings, *_|
          assert_equal(1053, settings[:port])
          assert_equal(true, settings[:use_tcp])
          assert_equal('127.0.0.1', settings[:src_address])
        end
        assert(trace[:hops].all?(&:closed))
        assert_equal(['.'], Dnsruby::Recursor.class_variable_get(:@@zones_cache).keys)
        assert_empty(Dnsruby::Recursor.class_variable_get(:@@authority_cache))
      end
    end
  end

  def test_root_refresh_failure_stops_address_discovery_without_repeated_bootstrap
    [{bootstrap_error: Dnsruby::Refused.new}, {bootstrap_error: IOError.new},
      {bootstrap_ttl: nil}].each do |options|
      with_root_expiry_during_lookup(**options) do |run, trace|
        assert_nil(run.call)
        assert_equal(1, trace[:bootstraps].length)
        assert_equal(['www.child.example.'], trace[:calls].map(&:first))
        assert(trace[:hops].all?(&:closed))
        refute(Dnsruby::Recursor.class_variable_get(:@@hints_mutex).locked?)
      end
    end
  end

  def test_zero_ttl_root_refresh_completes_the_current_lookup
    with_root_expiry_during_lookup(bootstrap_ttl: 0) do |run, trace|
      assert_equal('192.0.2.1', run.call.answer.first.address.to_s)
      assert_equal(1, trace[:bootstraps].length)
      assert_equal(['www.child.example.', 'ns.external.', 'www.child.example.'], trace[:calls].map(&:first))
      assert_empty(Dnsruby::Recursor.class_variable_get(:@@hints).values.first.values)
      assert(trace[:hops].all?(&:closed))
    end
  end

  def test_root_refresh_preserves_cancellation_and_unexpected_errors
    [Dnsruby::QueryControl::Cancelled.new, Dnsruby::VerifyError.new, RuntimeError.new].each do |error|
      with_root_expiry_during_lookup(bootstrap_error: error) do |run, trace|
        assert_same(error, assert_raises(error.class) { run.call })
        assert_equal(1, trace[:bootstraps].length)
        assert(trace[:hops].all?(&:closed))
        refute(Dnsruby::Recursor.class_variable_get(:@@hints_mutex).locked?)
        assert_nil(Dnsruby::QueryControl.current)
      end
    end
  end

  def test_root_refresh_keeps_the_original_query_deadline
    with_root_expiry_during_lookup(wait_for_deadline: true) do |run, trace|
      assert_raises(Dnsruby::ResolvTimeout) { run.call }
      assert_equal(1, trace[:bootstraps].length)
      assert(trace[:hops].all?(&:closed))
      refute(Dnsruby::Recursor.class_variable_get(:@@hints_mutex).locked?)
      assert_nil(Dnsruby::QueryControl.current)
    end
  end

  def test_hints_replacement_publishes_only_a_complete_bootstrap
    with_recursion_state do
      klass = Dnsruby::Recursor
      root = root_authorities
      klass.class_variable_set(:@@hints, root)
      klass.class_variable_set(:@@zones_cache, {'.' => root, 'example.' => root})
      resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1')
      resolver.stub(:query_no_validation_or_recursion, ->(*) { raise Dnsruby::ResolvTimeout }) do
        assert_raises(Dnsruby::ResolvTimeout) { klass.set_hints(['192.0.2.54'], resolver) }
      end
      assert_same(root, klass.class_variable_get(:@@hints))
      assert(klass.class_variable_get(:@@zones_cache).key?('example.'))
      packet = Dnsruby::Message.new('.', 'NS')
      packet.add_answer(Dnsruby::RR.create('. 300 IN NS ns.root.'))
      packet.add_additional(Dnsruby::RR.create('ns.root. 300 IN A 192.0.2.54'))
      resolver.stub(:query_no_validation_or_recursion, packet) do
        assert_equal(['ns.root'], klass.set_hints([], resolver))
      end
      assert_equal(['192.0.2.54'], klass.class_variable_get(:@@hints)['ns.root'].values)
      assert_equal(['.'], klass.class_variable_get(:@@zones_cache).keys)
      assert_equal(false, resolver.recurse)
      klass.reset_hints
      default = klass.new
      assert_instance_of(Dnsruby::Resolver, default.resolver)
    ensure
      default&.resolver&.close
      resolver&.close
    end
  end

  private

  def with_root_expiry_during_lookup(reset: false, bootstrap_error: nil,
      bootstrap_ttl: 300, wait_for_deadline: false)
    with_recursion_state do
      klass = Dnsruby::Recursor
      clock = 1_800_000_000
      Time.stub(:now, -> { Time.at(clock) }) do
        addresses = klass::AddressCache.new
        addresses.push(['192.0.2.53', 1])
        root = {'ns.root.' => addresses}
        klass.class_variable_set(:@@hints, root)
        klass.class_variable_set(:@@zones_cache, {'.' => root})
        klass.class_variable_set(:@@stale_hints, nil)
        resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', do_caching: false,
          query_timeout: wait_for_deadline ? 0.05 : 1, port: 1053, use_tcp: true,
          src_address: '127.0.0.1')
        recursor = klass.new(resolver)
        trace = {calls: [], bootstraps: [], hops: [], generation: klass.class_variable_get(:@@hints_generation)}
        context = -> { [Dnsruby::QueryControl.current, Dnsruby::SelectThread::TransmissionBudget.current,
          Thread.current[:dnsruby_recursor_generation]] }
        bootstrap = lambda do |*|
          trace[:bootstraps] << ['.', nil, *context.call]
          raise bootstrap_error if bootstrap_error
          if wait_for_deadline
            Dnsruby::QueryControl.current.pop(Dnsruby::QueryControl::Queue.new)
          end
          packet = Dnsruby::Message.new('.', 'NS')
          if bootstrap_ttl
            packet.add_answer(Dnsruby::RR.create('. 300 IN NS ns.root.'))
            packet.add_additional(Dnsruby::RR.create("ns.root. #{bootstrap_ttl} IN A 192.0.2.54"))
          end
          packet
        end
        handler = lambda do |settings, message|
          name = message.question.first.qname.to_s(true)
          trace[:calls] << [name, settings, *context.call]
          packet = Dnsruby::Message.new(name, 'A')
          if settings[:nameserver] == ['192.0.2.53']
            packet.add_authority(Dnsruby::RR.create('child.example. 300 IN NS ns.external.'))
            clock += 1
            klass.reset_hints if reset
          elsif name == 'ns.external.'
            packet.add_answer(Dnsruby::RR.create('ns.external. 300 IN A 192.0.2.55'))
          else
            packet.add_answer(Dnsruby::RR.create('www.child.example. 300 IN A 192.0.2.1'))
          end
          packet
        end
        factory = lambda do |settings|
          hop = Struct.new(:dnssec, :recurse, :closed).new
          hop.define_singleton_method(:send_message) { |message| handler.call(settings, message) }
          hop.define_singleton_method(:close) { self.closed = true }
          trace[:hops] << hop
          hop
        end
        run = -> { recursor.query('www.child.example.', 'A', 'IN', true) }
        resolver.stub(:query_no_validation_or_recursion, bootstrap) do
          Dnsruby::Resolver.stub(:new, factory) { yield run, trace }
        end
      ensure
        resolver&.close
      end
    end
  end

  def with_no_glue_referral(glue_error: nil, address_error: nil, wait_for_deadline: false)
    with_recursion_state do
      klass = Dnsruby::Recursor
      root = root_authorities
      klass.class_variable_set(:@@hints, root)
      klass.class_variable_set(:@@zones_cache, {'.' => root})
      klass.class_variable_set(:@@stale_hints, nil)
      resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', do_caching: false,
        query_timeout: wait_for_deadline ? 0.05 : 1, port: 1053, use_tcp: true,
        src_address: '127.0.0.1', src_port: [60_001])
      recursor = klass.new(resolver)
      calls, hops = [], []
      handler = lambda do |settings, message|
        name = message.question.first.qname.to_s(true)
        calls << [name, settings, Dnsruby::QueryControl.current, Dnsruby::SelectThread::TransmissionBudget.current]
        raise glue_error if settings[:nameserver] == ['192.0.2.55']
        if name == 'ns.external.'
          raise address_error if address_error
          if wait_for_deadline
            Dnsruby::QueryControl.current.pop(Dnsruby::QueryControl::Queue.new)
          end
          answer = Dnsruby::Message.new(name, 'A')
          answer.add_answer(Dnsruby::RR.create('ns.external. 300 IN A 192.0.2.54'))
        elsif settings[:nameserver] == ['192.0.2.53']
          answer = Dnsruby::Message.new(name, 'A')
          answer.add_authority(Dnsruby::RR.create('child.example. 300 IN NS ns.external.'))
          if glue_error
            answer.add_authority(Dnsruby::RR.create('child.example. 300 IN NS ns.child.example.'))
            answer.add_additional(Dnsruby::RR.create('ns.child.example. 300 IN A 192.0.2.55'))
          end
        else
          answer = Dnsruby::Message.new(name, 'A')
          answer.add_answer(Dnsruby::RR.create('www.child.example. 300 IN A 192.0.2.1'))
        end
        answer
      end
      factory = lambda do |settings|
        hop = Struct.new(:dnssec, :recurse, :closed).new
        hop.define_singleton_method(:send_message) { |message| handler.call(settings, message) }
        hop.define_singleton_method(:close) { self.closed = true }
        hops << hop
        hop
      end
      run = -> { recursor.query('www.child.example.', 'A', 'IN', true) }
      Dnsruby::Resolver.stub(:new, factory) { yield run, calls, hops }
    ensure
      resolver&.close
    end
  end

  def root_authorities
    addresses = Dnsruby::Recursor::AddressCache.new
    addresses.push(['192.0.2.53', 300])
    {'ns.root.' => addresses}
  end

  def with_recursion_state
    klass = Dnsruby::Recursor
    previous = %i[@@hints @@zones_cache @@zone_expirations @@authority_cache @@nameservers
      @@stale_hints @@hints_generation].to_h { |key| [key, klass.class_variable_get(key)] }
    klass.class_variable_set(:@@authority_cache, {})
    yield
  ensure
    previous.each { |key, value| klass.class_variable_set(key, value) }
  end

  def isolated(body)
    options = ''.frozen? ? ['--enable-frozen-string-literal'] : []
    stdout, stderr, status = Open3.capture3(RbConfig.ruby, *options, '-I',
      File.expand_path('../lib', __dir__), '-e', "#{TestPipelineSend::FIXTURE}\nTimeout.timeout(10) do\n#{body}\nend")
    assert(status.success?, "#{stdout}\n#{stderr}")
    assert_empty(stderr)
  end
end
