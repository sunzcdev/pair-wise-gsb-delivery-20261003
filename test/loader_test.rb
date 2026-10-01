require_relative 'test_helper'

class GraphQL::Batch::LoaderTest < Minitest::Test
  class GroupCountLoader < GraphQL::Batch::Loader
    def initialize(key)
    end

    def perform(keys)
      keys.each { |key| fulfill(key, keys.size) }
    end
  end

  class EchoLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each { |key| fulfill(key, key) }
    end
  end

  class IncrementLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each { |key| fulfill(key, key + 1) }
    end
  end

  class BrokenLoader < GraphQL::Batch::Loader
    def perform(keys)
    end
  end

  class NestedLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each { |key| fulfill(key, EchoLoader.load(key)) }
    end
  end

  class DerivedCacheKeyLoader < EchoLoader
    def cache_key(load_key)
      load_key.to_s
    end
  end

  class FulfillingStatusLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each do |key|
        fulfill(key, key)
        fulfill(key, fulfilled?(key))
      end
    end
  end

  class FulfillingStatusNestedLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each do |key|
        fulfill(key, EchoLoader.load(key))
        fulfill(key, fulfilled?(key))
      end
    end
  end

  class ExplodingLoader < GraphQL::Batch::Loader
    def perform(_keys)
      raise 'perform failed'
    end
  end

  class RecordingLoader < GraphQL::Batch::Loader
    attr_reader :performed_keys

    def initialize(max_batch_size: nil)
      super
      @performed_keys = []
    end

    def perform(keys)
      @performed_keys << keys
      keys.each { |key| fulfill(key, key) }
    end
  end

  class SecondSliceFailureLoader < RecordingLoader
    def perform(keys)
      @performed_keys << keys

      fulfill(keys.first, keys.first)
      raise 'second slice failed' if @performed_keys.length == 2

      keys.each { |key| fulfill(key, key) unless fulfilled?(key) }
    end
  end

  class KwargsGroupCountLoader < GraphQL::Batch::Loader
    def initialize(group, key: :id, preload: true, **conditions)
    end

    def perform(keys)
      keys.each { |key| fulfill(key, keys.size) }
    end

    # Re-passes kwargs through a method that destructures them. On Ruby 4.0,
    # `...` forwarding shares the kwargs hash across expansion sites, so
    # destructuring in `new(...)` would drain the hash stored in the loader key.
    def self.load_via_intermediary(group, load_key, key: :id, preload: true, **conditions)
      self.for(group, key: key, preload: preload, **conditions).load(load_key)
    end
  end

  def setup
    GraphQL::Batch::Executor.current = GraphQL::Batch::Executor.new
  end

  def teardown
    GraphQL::Batch::Executor.current = nil
  end

  def test_no_executor
    GraphQL::Batch::Executor.current = nil

    assert_raises(GraphQL::Batch::NoExecutorError) do
      EchoLoader.for
    end
  end

  def test_single_query
    assert_equal 1, GroupCountLoader.for('single').load('first').sync
  end

  def test_query_group
    group = Promise.all([
      GroupCountLoader.for('two').load(:a),
      GroupCountLoader.for('one').load(:a),
      GroupCountLoader.for('two').load(:b),
    ])
    assert_equal [2, 1, 2], group.sync
  end

  def test_query_group_with_kwargs_forwarded_via_intermediary
    group = Promise.all([
      KwargsGroupCountLoader.load_via_intermediary('two', :a),
      KwargsGroupCountLoader.load_via_intermediary('one', :a),
      KwargsGroupCountLoader.load_via_intermediary('two', :b),
    ])
    assert_equal [2, 1, 2], group.sync
  end

  def test_query_many
    assert_equal [:a, :b, :c], EchoLoader.load_many([:a, :b, :c]).sync
  end

  def test_max_batch_size_chunks_keys_in_enqueue_order
    loader = RecordingLoader.for(max_batch_size: 2)
    promises = [loader.load(1), loader.load(2), loader.load(3), loader.load(4), loader.load(5)]

    assert_equal [1, 2, 3, 4, 5], loader.load_many([1, 2, 3, 4, 5]).sync
    assert_equal [[1, 2], [3, 4], [5]], loader.performed_keys
    assert_equal promises, promises.map { |promise| loader.load(promise.value) }
  end

  def test_max_batch_size_deduplicates_keys
    loader = RecordingLoader.for(max_batch_size: 2)
    first = loader.load(1)
    second = loader.load(2)
    duplicate_first = loader.load(1)
    third = loader.load(3)
    duplicate_second = loader.load(2)
    results = loader.load_many([1, 2, 1, 3, 2])

    assert_same first, duplicate_first
    assert_same second, duplicate_second
    assert_equal [1, 2, 1, 3, 2], results.sync
    assert_same third, loader.load(3)
    assert_equal [[1, 2], [3]], loader.performed_keys
  end

  def test_max_batch_size_uses_one_batch_by_default
    loader = RecordingLoader.for

    assert_equal [1, 2, 3], loader.load_many([1, 2, 3]).sync
    assert_equal [[1, 2, 3]], loader.performed_keys
  end

  def test_max_batch_size_boundary_sizes
    [1, 2, 3, 4, 5].each do |max_batch_size|
      GraphQL::Batch::Executor.current = GraphQL::Batch::Executor.new
      loader = RecordingLoader.for(max_batch_size: max_batch_size)

      assert_equal [1, 2, 3, 4, 5], loader.load_many([1, 2, 3, 4, 5]).sync
      assert_equal max_batch_size, loader.performed_keys.map(&:length).first
      assert_equal 5, loader.performed_keys.flatten.length
    end
  end

  def test_max_batch_size_queues_callback_loads_for_next_wave
    loader = RecordingLoader.for(max_batch_size: 2)
    first = loader.load(1)
    second = loader.load(2)
    third = loader.load(3)
    fourth = loader.load(4)
    first.then { loader.load(:callback) }

    assert_equal [1, 2, 3, 4], Promise.all([first, second, third, fourth]).sync
    assert_equal :callback, loader.load(:callback).sync
    assert_equal [[1, 2], [3, 4], [:callback]], loader.performed_keys
  end

  def test_max_batch_size_continues_after_a_failed_slice
    loader = SecondSliceFailureLoader.for(max_batch_size: 2)
    first = loader.load(1)
    second = loader.load(2)
    third = loader.load(3)
    fourth = loader.load(4)
    fifth = loader.load(5)

    assert_equal 1, first.sync
    assert_equal 2, second.sync
    assert_equal 3, third.sync
    assert_equal 'second slice failed', fourth.reason.message
    assert_equal 5, fifth.sync
    assert_equal [[1, 2], [3, 4], [5]], loader.performed_keys
  end

  def test_max_batch_size_rejects_non_positive_sizes
    [0, -1].each do |max_batch_size|
      GraphQL::Batch::Executor.current = GraphQL::Batch::Executor.new
      loader = RecordingLoader.for(max_batch_size: max_batch_size)
      promise = loader.load(1)

      promise.wait
      assert_equal ArgumentError, promise.reason.class
      assert_equal 'max_batch_size must be a positive integer', promise.reason.message
      assert_equal [], loader.performed_keys
    end
  end

  def test_empty_group_query
    assert_equal [], Promise.all([]).sync
  end

  def test_group_query_with_non_queries
    assert_equal [1, :a, 'b'], Promise.all([1, :a, 'b']).sync
  end

  def test_group_query_with_some_queries
    group = Promise.all([
      GroupCountLoader.for("two").load(:a),
      'one',
      GroupCountLoader.for("two").load(:b),
    ])
    assert_equal [2, 'one', 2], group.sync
  end

  def test_then
    assert_equal 3, GroupCountLoader.for("single").load(:a).then { |value| value + 2 }.sync
  end

  def test_then_error
    query = GroupCountLoader.for("single").load(:a).then { raise "oops" }
    err = assert_raises(RuntimeError) do
      query.sync
    end
    assert_equal 'oops', err.message
  end

  def test_on_reject_without_error
    assert_equal 3, GroupCountLoader.for("single").load(:a).then { |value| value + 2 }.then(nil, ->(err) { err.message }).sync
  end

  def test_rescue_with_error
    query = GroupCountLoader.for("single").load(:a).then { raise "oops" }.then(nil, ->(err) { err.message })
    assert_equal 'oops', query.sync
  end

  def test_query_in_callback
    assert_equal 5, EchoLoader.load(4).then { |value| EchoLoader.load(value + 1) }.sync
  end

  def test_broken_promise_loader_check
    promise = BrokenLoader.load(1)
    promise.wait
    assert_equal GraphQL::Batch::BrokenPromiseError, promise.reason.class
    assert_equal "#{BrokenLoader.name} didn't fulfill promise for key 1", promise.reason.message
  end

  def test_nested_promise_loader_check
    promise = NestedLoader.load(1)
    promise.wait
    assert_equal false, promise.pending?
    assert_equal false, promise.rejected?
    assert_equal true, promise.fulfilled?
    assert_equal 1, promise.value
  end

  def test_loader_class_grouping
    group = Promise.all([
      EchoLoader.load(:a),
      IncrementLoader.load(1),
    ])
    assert_equal [:a, 2], group.sync
  end

  def test_load_after_perform
    loader = EchoLoader.for
    promise1 = loader.load(:a)
    assert_equal :a, promise1.sync
    assert_equal :b, loader.load(:b).sync
    assert_equal promise1, loader.load(:a)
  end

  def test_load_on_different_loaders
    loader = EchoLoader.for
    assert_equal :a, loader.load(:a).sync
    loader2 = EchoLoader.for

    promise = loader2.load(:b)
    promise2 = loader.load(:c)

    assert_equal :b, promise.sync
    assert_equal :c, promise2.sync
  end

  def test_fulfilling_status
    assert FulfillingStatusLoader.load(1).sync
  end

  def test_nested_loader_fulfilling_status
    assert FulfillingStatusNestedLoader.load(1).sync
  end

  def test_derived_cache_key
    assert_equal [:a, :b, :a], DerivedCacheKeyLoader.load_many([:a, :b, "a"]).sync
  end

  def test_loader_for_without_load
    loader = EchoLoader.for
    GraphQL::Batch::Executor.current.wait_all
  end

  def test_loader_without_executor
    loader1 = GroupCountLoader.new('one')
    loader2 = GroupCountLoader.new('two')
    group = Promise.all([
      loader2.load(:a),
      loader1.load(:a),
      loader2.load(:b),
    ])
    assert_equal [2, 1, 2], group.sync
  end

  def test_loader_with_failing_perform
    error_message = nil
    promise = ExplodingLoader.load([1]).then(nil, ->(err) { error_message = err.message } ).sync
    assert_equal 'perform failed', error_message
  end

  def test_prime
    loader = EchoLoader.for
    loader.prime(:a, :prepared_value)

    assert_equal :prepared_value, loader.load(:a).sync
    assert_equal [:prepared_value, :b, :c], loader.load_many([:a, :b, :c]).sync
  end

  def test_will_not_call_perform_if_fully_primed
    loader = ExplodingLoader.for
    loader.prime(:a, 1)
    loader.prime(:b, 2)

    assert_equal [1, 2], loader.load_many([:a, :b]).sync
  end

  def test_priming_a_key_already_in_queue_does_nothing
    loader = EchoLoader.for

    promise = loader.load(:a)

    loader.prime(:a, :not_a)

    assert_equal :a, promise.sync
  end

  def test_prime_will_not_replace_already_cached_value
    loader = EchoLoader.for

    assert_equal :a, loader.load(:a).sync

    loader.prime(:a, :not_a)

    assert_equal :a, loader.load(:a).sync
  end
end
