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

  # Fulfills each key with "<key>-<batch number>" and supports one-shot
  # hooks that run inside #perform, before the keys are fulfilled. Hooks
  # are instance_exec'd on the loader, so they can call #clear, #load,
  # #fulfill and #reject to control how clearing interleaves with the
  # resolution of a batch.
  class ScriptedLoader < GraphQL::Batch::Loader
    attr_reader :batches

    def initialize
      @batches = []
      @during_perform = []
    end

    def during_perform(&block)
      @during_perform << block
    end

    def perform(keys)
      @batches << keys.dup
      hook = @during_perform.shift
      instance_exec(&hook) if hook
      fulfill_keys(keys)
    end

    private

    def fulfill_keys(keys)
      keys.each { |key| fulfill(key, "#{key}-#{@batches.size}") }
    end
  end

  class ScriptedCacheKeyLoader < ScriptedLoader
    def cache_key(load_key)
      load_key.to_s
    end
  end

  class ExplodingOnceLoader < ScriptedLoader
    private

    def fulfill_keys(keys)
      raise 'perform failed' if batches.size == 1

      super
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

  def test_clear_returns_new_promise_and_reloads
    loader = ScriptedLoader.for
    promise1 = loader.load(:a)
    assert_same promise1, loader.load(:a)
    assert_equal "a-1", promise1.sync

    loader.clear(:a)

    promise2 = loader.load(:a)
    refute_same promise1, promise2
    assert_equal "a-2", promise2.sync
    assert_equal [[:a], [:a]], loader.batches
  end

  def test_clear_without_matching_key_does_nothing
    loader = ScriptedLoader.for
    promise = loader.load(:a)

    loader.clear(:b)

    assert_same promise, loader.load(:a)
    assert_equal "a-1", promise.sync
    assert_equal [[:a]], loader.batches
  end

  def test_clear_uses_cache_key_normalization
    loader = ScriptedCacheKeyLoader.for
    assert_equal "a-1", loader.load(:a).sync

    loader.clear("a") # different load key, same cache key

    assert_equal "a-2", loader.load(:a).sync
    assert_equal [[:a], [:a]], loader.batches
  end

  def test_clear_and_load_during_perform_attributes_results_to_the_right_generation
    loader = ScriptedLoader.for
    old_promise = loader.load(:a)
    new_promise = nil
    loader.during_perform do
      clear(:a)
      new_promise = load(:a)
    end

    # The old batch's fulfill arrives after the clear, but still settles
    # the promise it was queued for, not the new generation.
    assert_equal "a-1", old_promise.sync
    refute_same old_promise, new_promise
    assert_equal "a-2", new_promise.sync
    assert_equal [[:a], [:a]], loader.batches
  end

  def test_late_reject_from_old_batch_does_not_affect_new_generation
    loader = ScriptedLoader.for
    old_promise = loader.load(:a)
    new_promise = nil
    error = StandardError.new("old batch failed")
    loader.during_perform do
      clear(:a)
      new_promise = load(:a)
      reject(:a, error)
    end

    err = assert_raises(StandardError) { old_promise.sync }
    assert_equal "old batch failed", err.message
    assert_equal "a-2", new_promise.sync
    assert_equal [[:a], [:a]], loader.batches
  end

  def test_failed_perform_rejects_old_generation_only
    loader = ExplodingOnceLoader.for
    old_promise = loader.load(:a)
    new_promise = nil
    loader.during_perform do
      clear(:a)
      new_promise = load(:a)
    end

    err = assert_raises(RuntimeError) { old_promise.sync }
    assert_equal "perform failed", err.message
    assert_equal "a-2", new_promise.sync
    assert_equal [[:a], [:a]], loader.batches
  end

  def test_clear_before_resolve_settles_both_promises_in_one_batch
    loader = ScriptedLoader.for
    old_promise = loader.load(:a)
    loader.clear(:a)
    new_promise = loader.load(:a)

    refute_same old_promise, new_promise
    assert_equal "a-1", old_promise.sync
    assert_equal "a-1", new_promise.sync
    assert_equal [[:a]], loader.batches
  end

  def test_prime_after_clear_replaces_value
    loader = EchoLoader.for
    assert_equal :a, loader.load(:a).sync

    loader.clear(:a)
    loader.prime(:a, :primed)

    assert_equal :primed, loader.load(:a).sync
  end

  def test_clear_invalidates_primed_value
    loader = EchoLoader.for
    loader.prime(:a, :primed)

    loader.clear(:a)

    assert_equal :a, loader.load(:a).sync
  end

  def test_prime_does_not_replace_pending_promise_until_cleared
    loader = ScriptedLoader.for
    pending_promise = loader.load(:a)

    loader.prime(:a, :primed) # no-op: an entry already exists
    assert_same pending_promise, loader.load(:a)

    loader.clear(:a)
    loader.prime(:a, :primed)
    assert_equal :primed, loader.load(:a).sync

    # The in-flight promise is still settled by its own batch.
    assert_equal "a-1", pending_promise.sync
    assert_equal [[:a]], loader.batches
  end
end
