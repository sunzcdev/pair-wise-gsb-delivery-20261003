require_relative 'spec_helper'

class RecursorLimitsTest < Minitest::Test
  def setup
    @resolver = Dnsruby::Resolver.new(nameserver: '127.0.0.1', query_timeout: 0)
    @recursor = Dnsruby::Recursor.new(@resolver)
  end

  def teardown
    @resolver.close
  end

  def test_defaults_are_finite_and_invalid_replacements_are_atomic
    original = @recursor.recursion_limits
    assert_equal(30.0, original[:timeout])
    assert(original.frozen?)
    [{timeout: 0}, {max_queries: -1}, {max_workers: 0}, {unknown: 1}].each do |settings|
      assert_raises(ArgumentError) { @recursor.recursion_limits = settings }
      assert_same(original, @recursor.recursion_limits)
    end
    @recursor.recursion_limits = {timeout: 10, max_queries: 20}
    assert_equal(20, @recursor.recursion_limits[:max_queries])
    assert_equal(8, @recursor.recursion_limits[:max_workers])
  end

  def test_default_zero_resolver_timeout_still_has_a_recursion_deadline
    @recursor.define_singleton_method(:query_internal) do |*|
      Dnsruby::QueryControl.current.deadline - Dnsruby::TransportClock.now
    end
    remaining = @recursor.query('example.', 'A', 'IN', true)
    assert_operator(remaining, :>, 29)
    assert_operator(remaining, :<=, 30)
  end

  def test_recursion_deadline_stops_a_wait_and_restores_thread_context
    @recursor.recursion_limits = {timeout: 0.02}
    @recursor.define_singleton_method(:query_internal) do |*|
      Dnsruby::QueryControl.current.pop(Dnsruby::QueryControl::Queue.new)
    end
    assert_raises(Dnsruby::ResolvTimeout) { @recursor.query('example.', 'A', 'IN', true) }
    assert_nil(Dnsruby::QueryControl.current)
    assert_nil(Dnsruby::SelectThread::TransmissionBudget.current)
  end

  def test_shorter_parent_deadline_keeps_parent_error
    parent = Dnsruby::QueryControl.new(deadline: Dnsruby::TransportClock.now + 0.02)
    @recursor.define_singleton_method(:query_internal) do |*|
      Dnsruby::QueryControl.current.pop(Dnsruby::QueryControl::Queue.new)
    end
    assert_raises(Dnsruby::QueryControl::DeadlineExceeded) do
      Dnsruby::QueryControl.with(parent) { @recursor.query('example.', 'A', 'IN', true) }
    end
  end

  def test_supporting_queries_share_transmission_and_work_limits
    @recursor.recursion_limits = {max_queries: 2, max_steps: 2}
    Dnsruby::Recursor.send(:with_limits, @resolver, @recursor.recursion_limits) do |budget|
      transmissions = Dnsruby::SelectThread::TransmissionBudget.current
      2.times do |i|
        Dnsruby::Recursor.send(:with_limits, @resolver) do |nested|
          assert_same(budget, nested)
          nested.step!
          assert(transmissions.admit('192.0.2.53', 53, false, Dnsruby::Question.new("host#{i}.example.", 'A', 'IN')))
        end
      end
      assert_raises(Dnsruby::RecursionLimitError) { budget.step! }
      assert_raises(Dnsruby::RecursionLimitError) do
        transmissions.admit('192.0.2.54', 53, false, Dnsruby::Question.new('another.example.', 'A', 'IN'))
      end
    end
  end

  def test_worker_slots_are_released_and_dependency_paths_restore
    Dnsruby::Recursor.send(:with_limits, @resolver, @recursor.recursion_limits.merge(max_workers: 1)) do |budget|
      assert(budget.reserve_worker)
      refute(budget.reserve_worker)
      budget.release_worker
      assert(budget.reserve_worker)
      budget.release_worker
      assert_equal(:done, budget.with_dependency('ns.example.', 'A', 'IN') { :done })
      budget.with_dependency('ns.example.', 'A', 'IN') do
        refute(budget.with_dependency('NS.EXAMPLE.', 'A', 'IN') { flunk 'Repeated dependency ran' })
        assert_equal(:other_family, budget.with_dependency('ns.example.', 'AAAA', 'IN') { :other_family })
      end
      assert_empty(Thread.current[:dnsruby_recursion_dependencies])
    end
  end

  def test_transmission_limit_ends_the_resolvers_remaining_retry_plan
    engine = Dnsruby::ResolverRuby.allocate
    message = Dnsruby::Message.new('example.')
    client_queue, select_queue = Queue.new, Queue.new
    sender = Object.new
    engine.instance_variable_set(:@query_list, {request: [message, client_queue, select_queue, [:another_attempt]]})
    completion = nil
    engine.define_singleton_method(:send_result_and_stop_querying) { |*args| completion = args }
    error = Dnsruby::RecursionLimitError.new('Transmission limit reached')
    engine.handle_error_response(select_queue, [sender, message, :request, 0], error, nil)
    assert_equal([client_queue, :request, select_queue, nil, error], completion)
  end

  def test_root_bootstrap_batches_share_the_enclosing_transmission_limit
    klass = Dnsruby::Recursor
    saved = %i[@@hints @@stale_hints @@hints_generation @@nameservers @@zones_cache
      @@zone_expirations @@authority_cache].to_h { |key| [key, klass.class_variable_get(key)] }
    root = Dnsruby::Message.new('.', 'NS')
    root.add_answer(Dnsruby::RR.create('. 300 IN NS ns.example.'))
    budgets, queued = [], []
    @resolver.define_singleton_method(:query_no_validation_or_recursion) do |*|
      transmission = Dnsruby::SelectThread::TransmissionBudget.current
      budgets << transmission
      transmission.admit('192.0.2.53', 53, false, root.question.first)
      root
    end
    @resolver.define_singleton_method(:send_async) do |message, queue|
      queued << queue.length
      transmission = Dnsruby::SelectThread::TransmissionBudget.current
      budgets << transmission
      transmission.admit('192.0.2.53', 53, false, message.question.first)
      answer = Dnsruby::Message.new
      type = message.question.first.qtype.to_s
      address = type == 'A' ? '192.0.2.53' : '2001:db8::53'
      answer.add_answer(Dnsruby::RR.create("ns.example. 300 IN #{type} #{address}"))
      queue << [type, answer, nil]
      type
    end
    limits = @recursor.recursion_limits.merge(max_queries: 3, max_workers: 1)
    klass.send(:with_limits, @resolver, limits) do
      klass.set_hints([], @resolver)
      assert_equal([0, 0], queued, 'Each address batch must finish before the next begins')
      assert_equal(1, budgets.map(&:object_id).uniq.length)
      assert_raises(Dnsruby::RecursionLimitError) do
        budgets.first.admit('192.0.2.53', 53, false, Dnsruby::Question.new('example.', 'A', 'IN'))
      end
    end
  ensure
    saved&.each { |key, value| klass.class_variable_set(key, value) }
  end
end
