require_relative 'tc_pipeline_send'

class ReviewControlDeadlinesTest < Minitest::Test
  def test_validation_budget_interrupts_initial_connect_write_and_pipeline_wait
    [:connect, :write, :pipeline].each do |stage|
      [:deadline, :cancel].each do |reason|
        isolated("stage = #{stage.inspect}; reason = #{reason.inspect}\n" + <<~'RUBY')
          SELECTOR.singleton_class.remove_method(:notify_queue_observers)
          resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', dnssec: false,
            use_tcp: true, tcp_pipelining: stage == :pipeline, packet_timeout: 1,
            query_timeout: 2, retry_times: 1, do_caching: false)
          writer = resolver.single_resolvers.first
          waiting, finished = Queue.new, Queue.new
          sockets = []
          Socket.define_singleton_method(:new) do |*|
            socket = FrameSocket.new
            sockets << socket
            socket.define_singleton_method(:bind) { |*| 0 }
            socket.define_singleton_method(:connect_nonblock) do |*, **|
              waiting << true if stage == :connect
              stage == :connect ? :wait_writable : 0
            end
            socket.define_singleton_method(:write_nonblock) do |*|
              waiting << true
              raise IO::EAGAINWaitWritable
            end
            socket
          end
          IO.define_singleton_method(:select) do |_, _, _, duration|
            raise 'unbounded I/O wait' unless duration > 0 && duration <= 0.05
            sleep(duration)
            nil
          end
          pipeline_mutex = Mutex.new
          if stage == :pipeline
            pipeline_mutex.lock
            writer.instance_variable_set(:@pipeline_mutex, pipeline_mutex)
            original = writer.method(:send_dnsruby)
            writer.define_singleton_method(:send_dnsruby) do |*args|
              waiting << true
              original.call(*args)
            end
          end
          budget = Dnsruby::ValidationBudget.new(limits:
            Dnsruby::Dnssec.validation_limits.merge(timeout: reason == :deadline ? 0.04 : 2))
          started = Dnsruby::TransportClock.now
          worker = Thread.new do
            begin
              Dnsruby::ValidationBudget.with(budget) do
                resolver.query_no_validation_or_recursion('lookup.example.', 'A')
              end
              finished << :unexpected_success
            rescue Dnsruby::ValidationLimitError => error
              finished << error
            end
          end
          Timeout.timeout(2) { waiting.pop }
          budget.cancel if reason == :cancel
          result = Timeout.timeout(0.5) { finished.pop }
          worker.join
          expected = reason == :cancel ? Dnsruby::ValidationCancelled : Dnsruby::ValidationLimitError
          raise "wrong result: #{result.inspect}" unless result.instance_of?(expected)
          elapsed = Dnsruby::TransportClock.now - started
          raise "initial send ignored control: #{elapsed}" unless elapsed < 0.3
          raise 'unconnected or blocked socket leaked' unless sockets.all?(&:closed?)
          engine = resolver.instance_variable_get(:@resolver_ruby)
          raise 'logical query leaked' unless engine.instance_variable_get(:@query_list).empty?
          raise 'logical deadline leaked' unless engine.next_tick_at.nil?
          raise 'packet attempt leaked' unless K.class_variable_get(:@@query_hash).empty?
          raise 'observer leaked' unless K.class_variable_get(:@@observers).empty?
          pipeline_mutex.unlock if stage == :pipeline
          resolver.close
        RUBY
      end
    end
  end

  def test_scheduled_and_protocol_retries_keep_the_original_control
    isolated(<<~'RUBY')
      resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', dnssec: false,
        packet_timeout: 10, query_timeout: 20, retry_times: 2)
      writer = resolver.single_resolvers.first
      sent = []
      writer.define_singleton_method(:send_async) { |*| sent << Thread.current[:dnsruby_response_query] }
      control = Dnsruby::QueryControl.new(deadline: Dnsruby::TransportClock.now + 1)
      queue = Queue.new
      Dnsruby::QueryControl.with(control) { resolver.send_async(query, queue, :controlled) }
      engine = resolver.instance_variable_get(:@resolver_ruby)
      plan = engine.instance_variable_get(:@timeouts)[:controlled][1]
      attempt = plan.values.first
      plan.clear
      plan[Dnsruby::TransportClock.now - 1] = attempt
      engine.send(:update_tick_deadline, :controlled)
      engine.tick
      raise 'scheduled retry lost its controlling lookup' unless sent.length == 2 &&
        sent.all? { |settings| settings.send_token.control.equal?(control) && settings.endtime <= control.deadline }
      previous = sent.last
      previous.query_bytes = query.encode
      writer.define_singleton_method(:send_dnsruby_with_socket) { |*| }
      Thread.current[:dnsruby_response_query] = previous
      writer.send_dnsruby(query.encode, query, previous.client_query_id, previous.client_queue, true)
      Thread.current[:dnsruby_response_query] = nil
      replacement = SELECTOR.query_settings_for_id(previous.client_query_id)
      raise 'protocol retry extended the controlling deadline' unless replacement.endtime <= control.deadline
      raise 'protocol retry lost cancellation' unless replacement.send_token.equal?(previous.send_token)
      control.cancel
      raise 'retry ignored parent cancellation' unless replacement.send_token.cancelled?
      resolver.close
    RUBY
  end

  def test_deadline_heap_matches_live_deadlines_without_retaining_removed_entries
    deadlines = Dnsruby::DeadlineQueue.new
    expected = {}
    random = Random.new(73)
    5_000.times do
      key = random.rand(100)
      if random.rand(3).zero?
        assert(expected.delete(key) == deadlines.delete(key), "removed deadline for #{key}")
      else
        deadlines[key] = expected[key] = random.rand(1000)
      end
      assert(expected.values.min == deadlines.first_deadline, 'minimum live deadline')
      assert_equal(expected.length, deadlines.instance_variable_get(:@heap).length)
    end
    expected.keys.each { |key| deadlines.delete(key) }
    assert_nil(deadlines.first)
    assert_empty(deadlines.instance_variable_get(:@positions))
  end

  def test_logical_deadline_index_tracks_retries_validation_cancellation_and_reset
    isolated(<<~'RUBY')
      resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', dnssec: false,
        retry_times: 3, retry_delay: 0.02, query_timeout: 0.5)
      resolver.single_resolvers.each { |sender| sender.define_singleton_method(:send_async) { |*| } }
      queues = [Queue.new, Queue.new]
      queues.each_with_index { |queue, id| resolver.send_async(query, queue, id) }
      engine = resolver.instance_variable_get(:@resolver_ruby)
      check_minimum = -> do
        all = engine.instance_variable_get(:@timeouts).values.flat_map { |deadline, retries, _| [deadline, *retries.keys] }
        raise 'deadline index differs from live plans' unless engine.next_tick_at == all.min
      end
      check_minimum.call
      sleep 0.025
      engine.tick
      check_minimum.call
      resolver.single_res_mutex.synchronize do
        current = engine.instance_variable_get(:@query_list)[0]
        engine.begin_validation(0, current[3].first, current[2])
      end
      check_minimum.call
      engine.cancel_query(1, queues[1])
      check_minimum.call
      resolver.close
      check_minimum.call
      engine.reset_attributes
      check_minimum.call
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
