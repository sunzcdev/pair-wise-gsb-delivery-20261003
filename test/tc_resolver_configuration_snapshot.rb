require_relative 'tc_pipeline_send'

# Exercises the published ResolverConfiguration snapshot: in-flight queries keep
# their admitted nameserver, retry and validation settings, new queries observe
# the next publication, and snapshots carry plain values only.
class ResolverConfigurationSnapshotTest < Minitest::Test
  def test_snapshot_is_published_and_holds_only_plain_values
    resolver = Dnsruby::Resolver.new(nameserver: ['127.0.0.1', '127.0.0.2'],
      port: 1053, retry_times: 2, retry_delay: 4, query_timeout: 9, dnssec: false)
    snapshot = resolver.current_configuration
    assert_kind_of(Dnsruby::Resolver::ResolverConfiguration, snapshot)
    assert(snapshot.frozen?)
    assert_equal([1053, 1053], snapshot.servers.map(&:port))
    assert_equal(['127.0.0.1', '127.0.0.2'], snapshot.nameservers)
    assert_equal(2, snapshot.retry_times)
    assert_equal(4, snapshot.retry_delay)
    assert_equal(9, snapshot.query_timeout)
    # Every nested value is a primitive/array/hash/struct, never an I/O object.
    dump = Marshal.dump(snapshot.to_h)
    restored = Marshal.load(dump)
    assert_equal(snapshot.servers.map(&:to_h), restored[:servers])
    refute_includes(restored.inspect, 'Socket')
    refute_includes(restored.inspect, 'PacketSender')
    refute_includes(restored.inspect, 'OpenSSL')
  ensure
    resolver&.close
  end

  def test_tsig_policy_is_captured_as_plain_values
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1')
    resolver.tsig = ['key.example.', 'c2VjcmV0', 'hmac-sha256']
    snapshot = resolver.current_configuration
    assert_equal('key.example', snapshot.tsig.name)
    assert_equal('hmac-sha256', snapshot.tsig.algorithm)
    assert_equal('c2VjcmV0', snapshot.tsig.key)
    assert(snapshot.tsig.frozen?)
    assert_nil(Dnsruby::Resolver::ResolverConfiguration.new(
      resolver.send(:configuration_attribute_values),
      servers: [{server: '127.0.0.1', port: 53}], tsig: nil,
      generation: 99, publisher: resolver).tsig)
  ensure
    resolver&.close
  end

  def test_in_flight_query_keeps_its_snapshot_after_a_running_change
    isolated(<<~'RUBY')
      SELECTOR.singleton_class.remove_method(:notify_queue_observers)
      resolver = Dnsruby::Resolver.new(nameserver: ['127.0.0.1'], dnssec: false,
        retry_times: 1, retry_delay: 1, query_timeout: 60, do_caching: false)
      writer = resolver.single_resolvers.first
      dispatched = []
      writer.define_singleton_method(:prepare_send_async) do |message, queue, id, *_|
        dispatched << [id, self]
        -> { }
      end
      queue = Queue.new
      resolver.send_async(query('old.example.'), queue, :old)
      engine = resolver.instance_variable_get(:@resolver_ruby)
      admitted = engine.instance_variable_get(:@query_list).fetch(:old).last
      raise 'missing admitted snapshot' unless admitted.is_a?(Dnsruby::Resolver::ResolverConfiguration)

      # Reconfigure while the first query is outstanding.
      resolver.nameserver = '127.0.0.2'
      resolver.retry_times = 3
      resolver.retry_delay = 7
      resolver.query_timeout = 11
      newer = resolver.current_configuration
      raise 'new query saw the old snapshot' if newer.generation == admitted.generation
      raise 'old query lost its snapshot' unless
        engine.instance_variable_get(:@query_list).fetch(:old).last.equal?(admitted)
      raise 'old retry plan changed' unless admitted.retry_times == 1 && admitted.retry_delay == 1

      resolver.send_async(query('new.example.'), Queue.new, :new)
      new_entry = engine.instance_variable_get(:@query_list).fetch(:new)
      raise 'new query did not capture the published snapshot' unless new_entry.last.equal?(newer)
      raise 'new query used a retired sender' unless
        new_entry.last.servers.map(&:server) == ['127.0.0.2']
      resolver.close
    RUBY
  end

  def test_retry_plan_keeps_captured_senders_after_nameserver_replacement
    isolated(<<~'RUBY')
      SELECTOR.singleton_class.remove_method(:notify_queue_observers)
      resolver = Dnsruby::Resolver.new(nameserver: ['127.0.0.1'], dnssec: false,
        retry_times: 2, retry_delay: 1, do_caching: false)
      old_sender = resolver.single_resolvers.first
      plan = nil
      old_sender.define_singleton_method(:prepare_send_async) do |_message, _queue, id, *_|
        plan ||= id
        -> { }
      end
      resolver.send_async(query('retry.example.'), Queue.new, :retry)
      resolver.nameserver = '127.0.0.2'
      new_sender = resolver.single_resolvers.first
      raise 'sender was not replaced' if new_sender.equal?(old_sender)
      engine = resolver.instance_variable_get(:@resolver_ruby)
      timeouts = engine.instance_variable_get(:@timeouts).fetch(:retry)[1]
      # Buckets are either one [sender, retry_count] pair or an array of them.
      attempts = timeouts.values.flat_map { |bucket| bucket.first.is_a?(Array) ? bucket : [bucket] }
      senders = attempts.map(&:first)
      raise 'retry plan adopted the new nameserver' unless senders.all? { |sender| sender.equal?(old_sender) }
      # The old sender remains published to the query even though the live
      # nameserver list now contains only its replacement.
      raise 'captured sender vanished from the plan' unless senders.length == attempts.length
      resolver.close
    RUBY
  end

  def test_invalid_configuration_is_not_published
    resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', src_port: [60_001, 60_002])
    before = resolver.current_configuration
    assert_raises(ArgumentError) { resolver.src_port = [] }
    assert_same(before, resolver.current_configuration)
    assert_equal([60_001, 60_002], resolver.src_port)
    assert_raises(ArgumentError) { resolver.tcp_idle_timeout = 0 }
    assert_same(before, resolver.current_configuration)
    assert_equal(10, resolver.tcp_idle_timeout)
    assert_raises(ArgumentError) { resolver.tsig = 42 }
    assert_nil(resolver.tsig)
    assert_same(before, resolver.current_configuration)
  ensure
    resolver&.close
  end

  def test_publish_configuration_returns_the_live_snapshot_and_reports_failure
    Dnsruby::Config.stub(:default_config_hash, {nameserver: []}) do
      resolver = Dnsruby::Resolver.new
      error = assert_raises(ArgumentError) { resolver.publish_configuration! }
      assert_match(/nameserver/i, error.message)
      resolver.single_resolvers = [Dnsruby::PacketSender.new(server: '127.0.0.1')]
      snapshot = resolver.publish_configuration!
      assert_same(resolver.current_configuration, snapshot)
      assert_equal(['127.0.0.1'], snapshot.nameservers)
    ensure
      resolver&.close
    end
  end

  def test_setter_does_not_require_the_global_configuration_mutex
    isolated(<<~'RUBY')
      resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', dnssec: false)
      mutex = resolver.single_res_mutex
      entered, release = Queue.new, Queue.new
      holder = Thread.new do
        mutex.synchronize do
          entered << true
          release.pop
        end
      end
      entered.pop
      setter = Thread.new { resolver.retry_times = 5 }
      Thread.pass until setter.status == 'sleep' || !setter.alive?
      raise 'setter blocked on the query lock' unless setter.alive?
      # Configuration publication never takes configuration_mutex: a setter
      # blocked on query-state bookkeeping must not hold that global lock.
      configuration_mutex = resolver.send(:configuration_mutex)
      raise 'global configuration mutex held by a blocked setter' if
        configuration_mutex.instance_variable_get(:@locked)
      release << true
      holder.value
      setter.value
      raise 'setter lost its value' unless resolver.retry_times == 5
      resolver.close
    RUBY
  end

  def test_tcp_fallback_inherits_the_admitted_snapshot
    isolated(<<~'RUBY')
      resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', dnssec: false,
        do_caching: false, packet_timeout: 60)
      writer = resolver.single_resolvers.first
      fallback = Queue.new
      writer.define_singleton_method(:send_async) do |message, queue, id, use_tcp = false, *_|
        fallback << [use_tcp, Thread.current[Dnsruby::Resolver::ConfigurationSnapshotContext::THREAD_KEY]]
      end
      message = query('fallback.example.')
      id = [:fallback, message, :fallback, 0]
      snapshot = resolver.current_configuration
      operation = Dnsruby::Resolver::ConfigurationSnapshotContext.with(snapshot) do
        writer.prepare_send_async(message, Queue.new, id)
      end
      operation&.call
      settings = SELECTOR.query_settings_for_id(id)
      raise 'attempt lost its snapshot binding' unless settings.configuration_snapshot.equal?(snapshot)
      Thread.current[:dnsruby_response_query] = settings
      begin
        truncated = message.clone
        truncated.header.qr = truncated.header.tc = true
        accepted = Dnsruby::Resolver::ConfigurationSnapshotContext.with(snapshot) do
          writer.check_response(truncated, truncated.encode, message,
            settings.client_queue, :fallback, false)
        end
        raise 'check_response rejected truncation' unless accepted == false
      ensure
        Thread.current[:dnsruby_response_query] = nil
      end
      use_tcp, inherited = nil
      # The same exchange may first run an EDNS FORMERR retry; wait for the
      # truncation-driven TCP fallback which this test is asserting.
      until use_tcp == true
        use_tcp, inherited = fallback.pop
      end
      raise 'fallback lost the request snapshot' unless inherited.equal?(snapshot)
      resolver.close
    RUBY
  end

  def test_dnssec_validation_subquery_inherits_the_admitted_snapshot
    isolated(<<~'RUBY')
      resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', dnssec: true,
        do_validation: true, do_caching: false, packet_timeout: 60, query_timeout: 60)
      writer = resolver.single_resolvers.first
      captured = Queue.new
      original = Dnsruby::ValidatorThread.instance_method(:initialize)
      Dnsruby::ValidatorThread.define_method(:initialize) do |*args|
        original.bind(self).call(*args)
        captured << instance_variable_get(:@configuration_snapshot)
      end
      writer.define_singleton_method(:send_async) do |message, queue, id|
        answer = message.clone
        answer.header.qr = true
        SELECTOR.push_response_to_select(id, queue, answer, message, self)
      end
      message = query('validated.example.')
      message.do_validation = true
      queue = Queue.new
      resolver.send_async(message, queue, :validated)
      engine = resolver.instance_variable_get(:@resolver_ruby)
      admitted = engine.instance_variable_get(:@query_list).fetch(:validated).last
      SELECTOR.send_queued_responses
      raise 'validator never ran' unless captured.size == 1
      raise 'validation subquery lost the admitted snapshot' unless captured.pop.equal?(admitted)
      resolver.close
    RUBY
  end

  def test_snapshot_context_is_scoped_to_its_publishing_resolver
    parent = Dnsruby::Resolver.new(nameserver: '127.0.0.1')
    other = Dnsruby::Resolver.new(nameserver: '127.0.0.2')
    snapshot = parent.current_configuration
    Dnsruby::Resolver::ConfigurationSnapshotContext.with(snapshot) do
      assert_same(snapshot,
        Dnsruby::Resolver::ConfigurationSnapshotContext.snapshot_for(parent))
      assert_nil(Dnsruby::Resolver::ConfigurationSnapshotContext.snapshot_for(other))
    end
    assert_nil(Dnsruby::Resolver::ConfigurationSnapshotContext.current)
  ensure
    parent&.close
    other&.close
  end

  private

  def isolated(body)
    options = ''.frozen? ? ['--enable-frozen-string-literal'] : []
    stdout, stderr, status = Open3.capture3(RbConfig.ruby, *options, '-I',
      File.expand_path('../lib', __dir__), '-e', "#{TestPipelineSend::FIXTURE}\nTimeout.timeout(10) do\n#{body}\nend")
    assert(status.success?, "#{stdout}\n#{stderr}")
    assert_empty(stderr)
  end
end
