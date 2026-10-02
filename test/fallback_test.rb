require_relative 'test_helper'

class GraphQL::Batch::FallbackTest < Minitest::Test
  # In-memory loader that records the keys handed to every perform call so the
  # batching behavior can be asserted.
  class InMemoryLoader < GraphQL::Batch::Loader
    attr_reader :batches

    def initialize(data = {})
      @data = data
      @batches = []
    end

    def perform(keys)
      @batches << keys.dup
      keys.each do |key|
        if @data.key?(key)
          fulfill(key, @data.fetch(key))
        else
          fulfill_missing(key)
        end
      end
    end
  end

  class ExplodingLoader < GraphQL::Batch::Loader
    def initialize
      @batches = []
    end

    attr_reader :batches

    def perform(keys)
      @batches << keys.dup
      raise 'primary perform failed'
    end
  end

  class RejectingLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each { |key| reject(key, RuntimeError.new('rejected')) }
    end
  end

  def setup
    GraphQL::Batch::Executor.current = GraphQL::Batch::Executor.new
  end

  def teardown
    GraphQL::Batch::Executor.current = nil
  end

  def test_partial_hit_uses_fallback_only_for_missing_keys
    tenant = InMemoryLoader.new(1 => 'tenant-1', 3 => 'tenant-3')
    defaults = InMemoryLoader.new(2 => 'default-2', 3 => 'default-3')
    composite = tenant.fallback(defaults)

    results = composite.load_many([1, 2, 3]).sync

    assert_equal ['tenant-1', 'default-2', 'tenant-3'], results
    assert_equal [[1, 2, 3]], tenant.batches
    assert_equal [[2]], defaults.batches
  end

  def test_all_missing_go_to_fallback_in_a_single_batch
    tenant = InMemoryLoader.new
    defaults = InMemoryLoader.new(20 => 'default-20', 30 => 'default-30')
    composite = tenant.fallback(defaults)

    results = composite.load_many([20, 30, 40]).sync

    assert_equal ['default-20', 'default-30', nil], results
    assert_equal [[20, 30, 40]], defaults.batches
  end

  def test_all_missing_can_raise_via_on_missing
    tenant = InMemoryLoader.new
    defaults = InMemoryLoader.new
    composite = tenant.fallback(defaults) { |key| raise "missing #{key}" }

    err = assert_raises(RuntimeError) { composite.load(42).sync }
    assert_equal 'missing 42', err.message
  end

  def test_legitimate_nil_does_not_trigger_fallback
    tenant = InMemoryLoader.new(1 => nil)
    defaults = InMemoryLoader.new(1 => 'default-1')
    composite = tenant.fallback(defaults)

    results = composite.load_many([1]).sync

    assert_equal [nil], results
    assert_equal [], defaults.batches
  end

  def test_nil_from_fallback_is_preserved
    tenant = InMemoryLoader.new
    defaults = InMemoryLoader.new(1 => nil)
    composite = tenant.fallback(defaults) { flunk 'on_missing should not run' }

    assert_nil composite.load(1).sync
  end

  def test_primary_exception_does_not_trigger_fallback
    primary = ExplodingLoader.for
    defaults = InMemoryLoader.new(1 => 'default-1')
    composite = primary.fallback(defaults)

    err = assert_raises(RuntimeError) do
      composite.load_many([1, 2]).sync
    end
    assert_equal 'primary perform failed', err.message
    assert_equal [], defaults.batches
  end

  def test_primary_rejection_does_not_trigger_fallback
    primary = RejectingLoader.for
    defaults = InMemoryLoader.new(1 => 'default-1')
    composite = primary.fallback(defaults)

    err = assert_raises(RuntimeError) { composite.load(1).sync }
    assert_equal 'rejected', err.message
    assert_equal [], defaults.batches
  end

  def test_duplicate_keys_are_coalesced_in_each_layer
    tenant = InMemoryLoader.new(1 => 'tenant-1')
    defaults = InMemoryLoader.new(2 => 'default-2')
    composite = tenant.fallback(defaults)

    results = composite.load_many([1, 2, 2, 1, 2]).sync

    assert_equal ['tenant-1', 'default-2', 'default-2', 'tenant-1', 'default-2'], results
    assert_equal [[1, 2]], tenant.batches
    assert_equal [[2]], defaults.batches
  end

  def test_fallback_value_is_not_written_back_to_primary_cache
    tenant_loader = InMemoryLoader.new
    defaults = InMemoryLoader.new(1 => 'default-1')
    composite = tenant_loader.fallback(defaults)

    assert_equal 'default-1', composite.load(1).sync

    # The primary loader still considers the key missing.
    assert_equal GraphQL::Batch::Loader::Missing, tenant_loader.load(1).sync
    assert_equal [[1]], tenant_loader.batches
  end

  def test_either_layer_lands_values_back_at_their_original_positions
    tenant = InMemoryLoader.new('a' => 'T-a', 'c' => nil)
    defaults = InMemoryLoader.new('b' => 'D-b', 'd' => nil)
    composite = tenant.fallback(defaults) { raise 'should not be reached' }

    results = composite.load_many(['d', 'c', 'b', 'a']).sync

    assert_equal [nil, nil, 'D-b', 'T-a'], results
  end

  def test_chained_fallbacks
    first = InMemoryLoader.new
    second = InMemoryLoader.new
    third = InMemoryLoader.new(2 => 'third-2', 3 => 'third-3')
    composite = first.fallback(second).fallback(third) { |key| "none-#{key}" }

    results = composite.load_many([1, 2, 3]).sync

    assert_equal ['none-1', 'third-2', 'third-3'], results
    assert_equal [[1, 2, 3]], first.batches
    assert_equal [[1, 2, 3]], second.batches
    assert_equal [[1, 2, 3]], third.batches
  end
end
