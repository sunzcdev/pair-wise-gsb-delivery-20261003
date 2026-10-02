require_relative 'test_helper'

class GraphQL::Batch::FallbackLoaderTest < Minitest::Test
  # In-memory loader which records every batch of keys it is asked to
  # perform and fulfills keys absent from its data with MISSING. A value of
  # nil in the data is a legitimate loaded value.
  class RecordingLoader < GraphQL::Batch::Loader
    attr_reader :batches

    def initialize(data, raise_error: nil, reject_with: nil)
      @data = data
      @raise_error = raise_error
      @reject_with = reject_with
      @batches = []
    end

    def perform(keys)
      @batches << keys.dup
      raise @raise_error if @raise_error

      keys.each do |key|
        if @reject_with
          reject(key, @reject_with)
        elsif @data.key?(key)
          fulfill(key, @data[key])
        else
          fulfill(key, GraphQL::Batch::MISSING)
        end
      end
    end
  end

  DEFAULT = ->(key) { :"default_for_#{key}" }

  def setup
    GraphQL::Batch::Executor.current = GraphQL::Batch::Executor.new
  end

  def teardown
    GraphQL::Batch::Executor.current = nil
  end

  def test_partial_hit_falls_back_in_one_batch_and_preserves_positions
    tenant = RecordingLoader.new({ a: 'ta', c: 'tc' })
    defaults = RecordingLoader.new({ b: 'db', d: 'dd' })
    composed = tenant.with_fallback(defaults)

    values = Promise.all([:a, :b, :c, :d].map { |key| composed.load(key) }).sync

    assert_equal ['ta', 'db', 'tc', 'dd'], values
    assert_equal [[:a, :b, :c, :d]], tenant.batches
    assert_equal [[:b, :d]], defaults.batches
  end

  def test_all_missing_go_to_fallback_in_one_batch
    tenant = RecordingLoader.new({})
    defaults = RecordingLoader.new({ a: 1, b: 2 })
    composed = tenant.with_fallback(defaults)

    values = Promise.all([:a, :b].map { |key| composed.load(key) }).sync

    assert_equal [1, 2], values
    assert_equal [[:a, :b]], tenant.batches
    assert_equal [[:a, :b]], defaults.batches

    assert_raises(GraphQL::Batch::KeyNotFoundError) { composed.load(:x).sync }
    assert_equal [[:a, :b], [:x]], tenant.batches
    assert_equal [[:a, :b], [:x]], defaults.batches
  end

  def test_nil_is_a_valid_value_in_primary_and_fallback
    tenant = RecordingLoader.new({ a: nil })
    defaults = RecordingLoader.new({ b: nil, c: 3 })
    composed = tenant.with_fallback(defaults)

    values = Promise.all([:a, :b, :c].map { |key| composed.load(key) }).sync

    assert_equal [nil, nil, 3], values
    assert_equal [[:a, :b, :c]], tenant.batches
    assert_equal [[:b, :c]], defaults.batches
  end

  def test_primary_raise_does_not_trigger_fallback_and_rejects_all
    error = StandardError.new('tenant store down')
    tenant = RecordingLoader.new({}, raise_error: error)
    defaults = RecordingLoader.new({ a: 1 })
    composed = tenant.with_fallback(defaults)

    promises = [:a, :b].map { |key| composed.load(key) }

    assert_raises(StandardError) { Promise.all(promises).sync }
    promises.each do |promise|
      assert_predicate promise, :rejected?
      assert_same error, promise.reason
    end
    assert_equal [], defaults.batches
  end

  def test_primary_rejection_does_not_trigger_fallback
    error = StandardError.new('rejected row')
    tenant = RecordingLoader.new({ b: 2 }, reject_with: error)
    defaults = RecordingLoader.new({ a: 1 })
    composed = tenant.with_fallback(defaults)

    promise_a = composed.load(:a)
    promise_b = composed.load(:b)

    assert_raises(StandardError) { promise_a.sync }
    assert_raises(StandardError) { promise_b.sync }
    assert_equal [], defaults.batches
  end

  def test_fallback_failure_only_rejects_keys_missing_from_primary
    error = StandardError.new('defaults down')
    tenant = RecordingLoader.new({ a: 1 })
    defaults = RecordingLoader.new({}, raise_error: error)
    composed = tenant.with_fallback(defaults)

    assert_equal 1, composed.load(:a).sync
    assert_same error, assert_raises(StandardError) { composed.load(:b).sync }
  end

  def test_missing_everywhere_rejects_by_default
    tenant = RecordingLoader.new({ a: 1 })
    defaults = RecordingLoader.new({ b: 2 })
    composed = tenant.with_fallback(defaults)

    error = assert_raises(GraphQL::Batch::KeyNotFoundError) do
      composed.load(:c).sync
    end
    assert_match(/:c/, error.message)
    assert_equal 1, composed.load(:a).sync
    assert_equal 2, composed.load(:b).sync
  end

  def test_missing_everywhere_uses_static_default
    tenant = RecordingLoader.new({ a: 1 })
    defaults = RecordingLoader.new({})
    composed = tenant.with_fallback(defaults, on_missing: 42)

    assert_equal [1, 42, 42], Promise.all([:a, :b, :c].map { |key| composed.load(key) }).sync
  end

  def test_missing_everywhere_uses_callable_default
    tenant = RecordingLoader.new({})
    defaults = RecordingLoader.new({ b: 2 })
    composed = tenant.with_fallback(defaults, on_missing: DEFAULT)

    values = Promise.all([:a, :b, :c].map { |key| composed.load(key) }).sync
    assert_equal [:default_for_a, 2, :default_for_c], values
  end

  def test_on_missing_callable_may_return_nil
    tenant = RecordingLoader.new({})
    defaults = RecordingLoader.new({ a: 1 })
    composed = tenant.with_fallback(defaults, on_missing: ->(_key) { nil })

    assert_nil composed.load(:b).sync
  end

  def test_duplicate_keys_keep_merging_semantics
    tenant = RecordingLoader.new({ a: 'ta' })
    defaults = RecordingLoader.new({ b: 'db' })
    composed = tenant.with_fallback(defaults)

    values = Promise.all([:a, :b, :a, :b].map { |key| composed.load(key) }).sync

    assert_equal ['ta', 'db', 'ta', 'db'], values
    assert_equal [[:a, :b]], tenant.batches
    assert_equal [[:b]], defaults.batches
  end

  def test_results_come_back_at_original_key_positions
    tenant = RecordingLoader.new({ c: 'tc' })
    defaults = RecordingLoader.new({ a: 'da', b: 'db' })
    composed = tenant.with_fallback(defaults)

    p1 = composed.load(:a)
    p2 = composed.load(:b)
    p3 = composed.load(:c)

    assert_equal 'da', p1.sync
    assert_equal 'db', p2.sync
    assert_equal 'tc', p3.sync
  end

  def test_does_not_mutate_underlying_loader_caches
    tenant = RecordingLoader.new({ a: 'ta' })
    defaults = RecordingLoader.new({ b: 'db' })
    composed = tenant.with_fallback(defaults)

    assert_equal 'db', composed.load(:b).sync

    # The fallback value is not written back into the primary loader cache:
    # the primary loader still reports MISSING for :b and performs no new batch.
    assert_equal GraphQL::Batch::MISSING, tenant.load(:b).sync
    assert_equal [[:b]], tenant.batches
    assert_equal [[:b]], defaults.batches

    # The composed loader keeps its own independent cache.
    assert_same composed.load(:b), composed.load(:b)
    assert_equal 1, defaults.batches.size
  end

  def test_reuses_the_same_composition_for_the_same_loader_pair
    tenant = RecordingLoader.new({ a: 'ta' })
    defaults = RecordingLoader.new({ b: 'db' })

    composed1 = tenant.with_fallback(defaults)
    composed2 = tenant.with_fallback(defaults)

    assert_same composed1, composed2
  end

  def test_compose_class_method
    tenant = RecordingLoader.new({ a: 1 })
    defaults = RecordingLoader.new({ b: 2 })
    composed = GraphQL::Batch::FallbackLoader.compose(tenant, defaults, on_missing: 0)

    assert_equal [1, 2, 0], Promise.all([:a, :b, :c].map { |key| composed.load(key) }).sync
  end

  def test_subsequent_wave_reuses_underlying_caches_without_new_batches
    tenant = RecordingLoader.new({ a: 'ta' })
    defaults = RecordingLoader.new({ b: 'db' })
    composed = tenant.with_fallback(defaults)

    assert_equal ['ta', 'db'], Promise.all([:a, :b].map { |key| composed.load(key) }).sync
    assert_equal ['ta', 'db'], Promise.all([:a, :b].map { |key| composed.load(key) }).sync

    assert_equal [[:a, :b]], tenant.batches
    assert_equal [[:b]], defaults.batches
  end
end
