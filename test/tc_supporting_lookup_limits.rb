require_relative 'spec_helper'
require 'minitest/mock'

class SupportingLookupLimitsTest < Minitest::Test
  def test_missing_glue_keeps_exact_context_without_configuration_threads
    control = Dnsruby::QueryControl.new
    transmissions = Dnsruby::SelectThread::TransmissionBudget.new
    seen, resolver = [], nil
    resolve = ->(*) { seen << [Dnsruby::QueryControl.current, Dnsruby::SelectThread::TransmissionBudget.current, Thread.current]; '192.0.2.53' }
    verifier = Dnsruby::SingleVerifier.new(Dnsruby::SingleVerifier::VerifierType::ANCHOR)
    name = Dnsruby::Name.create('example.')
    response = Dnsruby::Message.new(name, 'NS')
    response.add_answer(Dnsruby::RR.create('example. 60 IN NS ns.example.'))
    Dnsruby::Dnssec.stub(:default_resolver, nil) do
      Dnsruby::Config.stub(:resolve_server, resolve) do
        without_configuration_threads do
          Dnsruby::QueryControl.with(control) do
            Dnsruby::SelectThread::TransmissionBudget.with(transmissions) do
              resolver = verifier.get_nameservers_from_message(name, response)
            end
          end
        end
      end
    end
    assert_equal(['192.0.2.53'], resolver.single_resolvers.map(&:server))
    refute_empty(seen)
    seen.each do |actual_control, actual_transmissions, thread|
      assert_same(control, actual_control)
      assert_same(transmissions, actual_transmissions)
      assert_same(Thread.current, thread)
    end
    assert_nil(Dnsruby::QueryControl.current)
    assert_nil(Dnsruby::SelectThread::TransmissionBudget.current)
  ensure
    resolver&.close
  end

  def test_either_scope_keeps_configuration_on_the_calling_thread
    [:control, :transmissions].each do |scope|
      seen, resolver = [], nil
      resolve = ->(*) { seen << Thread.current; '192.0.2.53' }
      construct = -> { resolver = Dnsruby::Resolver.new(nameserver: 'ns.example.') }
      Dnsruby::Config.stub(:resolve_server, resolve) do
        without_configuration_threads do
          if scope == :control
            Dnsruby::QueryControl.with(Dnsruby::QueryControl.new, &construct)
          else
            Dnsruby::SelectThread::TransmissionBudget.with(Dnsruby::SelectThread::TransmissionBudget.new, &construct)
          end
        end
      end
      assert_equal(['192.0.2.53'], resolver.single_resolvers.map(&:server))
      refute_empty(seen)
      assert(seen.all? { |thread| thread.equal?(Thread.current) })
    ensure
      resolver&.close
    end
  end

  def test_partial_construction_is_closed_without_replacing_existing_senders
    error = Dnsruby::RecursionLimitError.new('Supporting lookup budget exhausted')
    resolver = Dnsruby::Resolver.new
    retained, created = sender_double('192.0.2.1'), sender_double('192.0.2.2')
    resolver.single_resolvers = [retained]
    candidates = []
    construct = ->(options) { candidates << options.fetch(:server); raise error if candidates.length == 2; created }
    Dnsruby::PacketSender.stub(:new, construct) do
      without_configuration_threads do
        actual = assert_raises(Dnsruby::RecursionLimitError) do
          Dnsruby::QueryControl.with(Dnsruby::QueryControl.new) do
            resolver.nameserver = %w[first.example. second.example. third.example.]
          end
        end
        assert_same(error, actual)
      end
    end
    assert_equal(%w[first.example. second.example.], candidates)
    assert_equal(1, created.close_count)
    assert_equal(0, retained.close_count)
    assert_equal([retained], resolver.single_resolvers)
  ensure
    resolver&.close
  end

  def test_deferred_configuration_retries_with_fresh_context_after_exhaustion
    error = Dnsruby::RecursionLimitError.new('Supporting lookup budget exhausted')
    resolver = Dnsruby::Resolver.new
    resolver.config.nameserver = ['ns.example.']
    attempts = 0
    resolve = ->(*) { attempts += 1; raise error if attempts == 1; '192.0.2.53' }
    Dnsruby::Config.stub(:resolve_server, resolve) do
      without_configuration_threads do
        actual = assert_raises(Dnsruby::RecursionLimitError) do
          Dnsruby::QueryControl.with(Dnsruby::QueryControl.new) { resolver.add_config_nameservers }
        end
        assert_same(error, actual)
        Dnsruby::QueryControl.with(Dnsruby::QueryControl.new) { resolver.add_config_nameservers }
      end
    end
    assert_equal(['192.0.2.53'], resolver.single_resolvers.map(&:server))
    assert_operator(attempts, :>, 1)
  ensure
    resolver&.close
  end

  def test_config_preserves_limits_from_primary_and_recursive_lookups
    [:primary, :recursive].each do |stage|
      expected = Dnsruby::RecursionLimitError.new('Supporting lookup budget exhausted')
      with_config_lookup(stage, -> { raise expected }) do |state|
        actual = assert_raises(Dnsruby::RecursionLimitError) { Dnsruby::Config.resolve_server('ns.example.') }
        assert_same(expected, actual)
        assert_equal(stage == :primary ? [:primary] : [:primary, :recursive], state[:calls])
        assert_equal(1, state[:closed])
      end
    end
  end

  def test_query_control_cancellation_stops_configuration_without_fallback
    control = Dnsruby::QueryControl.new
    cancelled = nil
    cancel = lambda do
      control.cancel
      begin
        control.check!
      rescue Dnsruby::QueryControl::Cancelled => error
        cancelled = error
        raise
      end
    end
    with_config_lookup(:primary, cancel) do |state|
      actual = assert_raises(Dnsruby::QueryControl::Cancelled) do
        Dnsruby::QueryControl.with(control) { Dnsruby::Config.resolve_server('ns.example.') }
      end
      assert_same(cancelled, actual)
      assert_equal([:primary], state[:calls])
      assert_equal(1, state[:closed])
    end
    assert_nil(Dnsruby::QueryControl.current)
  end

  def test_config_preserves_internal_validation_abort_from_either_lookup
    [:primary, :recursive].each do |stage|
      budget = Dnsruby::ValidationBudget.new(limits: Dnsruby::Dnssec.validation_limits.merge(max_work: 1))
      abort = nil
      exhaust = lambda do
        begin
          budget.check!(2)
        rescue StandardError => error
          abort = error
          raise
        end
      end
      with_config_lookup(stage, exhaust) do |state|
        error = assert_raises(Dnsruby::ValidationLimitError) do
          Dnsruby::ValidationBudget.with(budget) { Dnsruby::Config.resolve_server('ns.example.') }
        end
        assert_same(abort, error.cause)
        assert_same(error, abort.public_error)
        assert_equal(stage == :primary ? [:primary] : [:primary, :recursive], state[:calls])
        assert_equal(1, state[:closed])
      end
      assert_nil(Dnsruby::ValidationBudget.current)
      assert_nil(Dnsruby::QueryControl.current)
    end
  end

  def test_constructors_preserve_terminal_errors_without_retrying
    [Dnsruby::RecursionLimitError, Dnsruby::QueryControl::Cancelled, Dnsruby::ValidationLimitError].each do |error_class|
      constructors.each do |construct|
        expected = error_class.new('Supporting lookup stopped')
        calls = []
        resolve = ->(name) { calls << name; raise expected }
        Dnsruby::Config.stub(:resolve_server, resolve) do
          without_configuration_threads do
            actual = assert_raises(error_class) do
              Dnsruby::QueryControl.with(Dnsruby::QueryControl.new, &construct)
            end
            assert_same(expected, actual)
          end
        end
        assert_equal(['ns.example.'], calls)
      end
    end
  end

  def test_constructors_preserve_internal_validation_abort
    constructors.each do |construct|
      budget = Dnsruby::ValidationBudget.new(limits: Dnsruby::Dnssec.validation_limits.merge(max_work: 1))
      abort, calls = nil, []
      resolve = lambda do |name|
        calls << name
        begin
          budget.check!(2)
        rescue StandardError => error
          abort = error
          raise
        end
      end
      Dnsruby::Config.stub(:resolve_server, resolve) do
        without_configuration_threads do
          error = assert_raises(Dnsruby::ValidationLimitError) do
            Dnsruby::ValidationBudget.with(budget, &construct)
          end
          assert_same(abort, error.cause)
          assert_same(error, abort.public_error)
        end
      end
      assert_equal(['ns.example.'], calls)
      assert_nil(Dnsruby::ValidationBudget.current)
      assert_nil(Dnsruby::QueryControl.current)
    end
  end

  private

  def constructors
    [->(*) { Dnsruby::PacketSender.new(server: 'ns.example.', ignore_config_resolv_errors: true) },
      ->(*) { Dnsruby::Resolver.new(nameserver: 'ns.example.') }]
  end

  def without_configuration_threads(&block)
    Thread.stub(:new, ->(*) { flunk('Scoped nameserver configuration must not create a thread') }, &block)
  end

  def sender_double(address)
    Struct.new(:server, :close_count) do
      def close
        self.close_count += 1
      end
    end.new(address, 0)
  end

  def with_config_lookup(stage, stopped)
    state = {calls: [], closed: 0}
    lookup = Object.new
    lookup.define_singleton_method(:query) do |*|
      state[:calls] << :primary
      stage == :primary ? stopped.call : Dnsruby::Message.new('ns.example.')
    end
    lookup.define_singleton_method(:close) { state[:closed] += 1 }
    recursive = Object.new
    recursive.define_singleton_method(:query) { |*| state[:calls] << :recursive; stopped.call }
    build_recursive = lambda do |resolver|
      flunk('Stopped configuration must not start recursive fallback') unless stage == :recursive
      assert_same(lookup, resolver)
      recursive
    end
    Dnsruby::Config.stub(:nameserver_resolver, lookup) do
      Dnsruby::Recursor.stub(:new, build_recursive) { yield state }
    end
  end
end
