require_relative 'spec_helper'
require 'timeout'

class RecursorAddressFallbackTest < Minitest::Test
  def test_failed_family_keeps_the_other_address_family
    with_address_lookup(['ns.example.']) do |run, calls, _recursor|
      result = run.call do |name, type|
        raise Dnsruby::ServFail, 'AAAA unavailable' if type == 'AAAA'
        address_answer(name, type)
      end
      assert_equal(['192.0.2.53'], result)
      assert_equal(%w[A AAAA], drain(calls).map(&:last).sort)
    end
  end

  def test_failed_nameserver_does_not_hide_another_nameserver
    with_address_lookup(%w[bad.example. ns.example.]) do |run, calls, _recursor|
      result = run.call do |name, type|
        raise Dnsruby::NXDomain, 'name unavailable' if name == 'bad.example.'
        address_answer(name, type)
      end
      assert_equal(['192.0.2.53', '2001:db8::53'], result)
      assert_equal(4, drain(calls).length)
    end
  end

  def test_ordinary_lookup_failures_return_no_authority
    with_address_lookup(['ns.example.']) do |run, _calls, _recursor|
      assert_nil(run.call { |_name, _type| raise Dnsruby::Refused, 'lookup refused' })
    end
  end

  def test_validation_limit_is_preserved_and_cancels_a_waiting_sibling
    with_address_lookup(['ns.example.']) do |run, _calls, _recursor|
      waiting = Queue.new
      finished = Queue.new
      limit = Dnsruby::ValidationLimitError.new('validation budget exhausted')
      error = assert_raises(Dnsruby::ValidationLimitError) do
        run.call do |_name, type|
          if type == 'A'
            waiting.pop
            raise limit
          end
          begin
            waiting << true
            Dnsruby::QueryControl.current.pop(Dnsruby::QueryControl::Queue.new)
          ensure
            finished << true
          end
        end
      end
      assert_same(limit, error)
      assert_equal(true, finished.pop(true))
    end
  end

  def test_parent_cancellation_stops_all_address_workers
    with_address_lookup(['ns.example.']) do |run, _calls, _recursor|
      parent = Dnsruby::QueryControl.new
      started = Queue.new
      finished = Queue.new
      canceller = Thread.new { 2.times { started.pop }; parent.cancel }
      assert_raises(Dnsruby::QueryControl::Cancelled) do
        Dnsruby::QueryControl.with(parent) do
          run.call do |_name, _type|
            begin
              started << true
              Dnsruby::QueryControl.current.pop(Dnsruby::QueryControl::Queue.new)
            ensure
              finished << true
            end
          end
        end
      end
      assert_equal([true, true], 2.times.map { finished.pop(true) })
    ensure
      canceller&.kill
      canceller&.join
    end
  end

  def test_one_worker_runs_the_other_family_inline_without_deadlock
    with_address_lookup(['ns.example.']) do |run, _calls, _recursor|
      parent_thread = Thread.current
      family_threads = Queue.new
      release_worker = Queue.new
      limits = Dnsruby::Recursor::DEFAULT_RECURSION_LIMITS.merge(max_workers: 1)
      Dnsruby::Recursor.send(:with_limits, nil, limits) do |budget|
        result = run.call do |name, type|
          family_threads << [type, Thread.current]
          type == 'AAAA' ? release_worker.pop : release_worker.push(true)
          address_answer(name, type)
        end
        assert_equal(['192.0.2.53', '2001:db8::53'], result)
        threads = drain(family_threads).to_h
        assert_same(parent_thread, threads['A'])
        refute_same(parent_thread, threads['AAAA'])
        assert(budget.reserve_worker, 'Completed discovery must release its worker slot')
        budget.release_worker
      end
    end
  end

  def test_worker_creation_failure_releases_its_slot
    with_address_lookup(['ns.example.']) do |run, _calls, _recursor|
      limits = Dnsruby::Recursor::DEFAULT_RECURSION_LIMITS.merge(max_workers: 1)
      Dnsruby::Recursor.send(:with_limits, nil, limits) do |budget|
        Thread.stub(:new, ->(*) { raise ThreadError, 'Worker unavailable' }) do
          assert_raises(ThreadError) { run.call { |name, type| address_answer(name, type) } }
        end
        assert(budget.reserve_worker)
        budget.release_worker
      end
    end
  end

  def test_recursion_limit_does_not_try_another_nameserver
    with_address_lookup(%w[first.example. second.example.]) do |run, calls, _recursor|
      error = Dnsruby::RecursionLimitError.new('Lookup work limit reached')
      actual = assert_raises(Dnsruby::RecursionLimitError) { run.call { |*| raise error } }
      assert_same(error, actual)
      assert_equal(['first.example.'], drain(calls).map(&:first).uniq)
    end
  end

  private

  def address_answer(name, type)
    answer = Dnsruby::Message.new(name, type)
    address = type == 'AAAA' ? '2001:db8::53' : '192.0.2.53'
    answer.add_answer(Dnsruby::RR.create("#{name} 300 IN #{type} #{address}"))
    answer
  end

  def drain(queue)
    Array.new(queue.length) { queue.pop(true) }
  end

  def with_address_lookup(names)
    klass = Dnsruby::Recursor
    original_hints = klass.class_variable_get(:@@hints)
    original_cache = klass.class_variable_get(:@@authority_cache)
    klass.class_variable_set(:@@hints, {})
    klass.class_variable_set(:@@authority_cache, {})
    recursor = klass.allocate
    recursor.ipv6_ok = true
    original_recursion = recursor.method(:_dorecursion)
    authorities = names.to_h { |name| [name, klass::AddressCache.new] }
    calls = Queue.new
    run = lambda do |&lookup|
      recursor.define_singleton_method(:_dorecursion) do |name, type, *_args|
        if name == 'www.example.'
          authorities.values.flat_map(&:values).map(&:downcase).sort
        else
          calls << [name, type]
          lookup.call(name, type)
        end
      end
      Timeout.timeout(3) do
        original_recursion.call('www.example.', 'A', Dnsruby::Classes::IN,
          'example.', authorities, 0, true)
      end
    end
    yield run, calls, recursor
  ensure
    klass.class_variable_set(:@@hints, original_hints)
    klass.class_variable_set(:@@authority_cache, original_cache)
  end
end
