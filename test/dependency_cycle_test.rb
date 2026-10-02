require_relative 'test_helper'

class GraphQL::Batch::DependencyCycleTest < Minitest::Test
  class EchoLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each { |key| fulfill(key, key) }
    end
  end

  class MutualWaitLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each do |key|
        next fulfill(key, :independent) if key == 2

        fulfill(key, load(1 - key).sync)
      end
    end
  end

  class CyclicLoaderA < GraphQL::Batch::Loader
    attr_accessor :next_loader

    def perform(_keys)
      fulfill(:a, next_loader.load(:b))
    end
  end

  class CyclicLoaderB < GraphQL::Batch::Loader
    attr_accessor :next_loader

    def perform(_keys)
      fulfill(:b, next_loader.load(:c))
    end
  end

  class CyclicLoaderC < GraphQL::Batch::Loader
    attr_accessor :next_loader

    def perform(_keys)
      fulfill(:c, next_loader.load(:a))
    end
  end

  class ReentrantLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each do |key|
        fulfill(key, key.zero? ? 0 : load(key - 1).sync + 1)
      end
    end
  end

  class FulfilledByIndependentLoaderLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each do |key|
        dependency = key.zero? ? EchoLoader.load(:ready) : load(0).sync
        fulfill(key, dependency)
      end
    end
  end

  class DiamondBranchLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each do |key|
        fulfill(key, EchoLoader.load(:shared).then { |value| [key, value] })
      end
    end
  end

  class DiamondRootLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each do |key|
        fulfill(key, Promise.all([
          DiamondBranchLoader.load(:left),
          DiamondBranchLoader.load(:right),
        ]).sync)
      end
    end
  end

  class GroupCycleLoaderA < GraphQL::Batch::Loader
    attr_accessor :next_loader

    def perform(_keys)
      fulfill(:a, Promise.all([next_loader.load(:b)]))
    end
  end

  class GroupCycleLoaderB < GraphQL::Batch::Loader
    attr_accessor :next_loader

    def perform(_keys)
      fulfill(:b, next_loader.load(:a))
    end
  end

  class SelfGroupCycleLoader < GraphQL::Batch::Loader
    def perform(_keys)
      fulfill(:self, Promise.all([load(:self)]))
    end
  end

  class SelfCycleLoader < GraphQL::Batch::Loader
    def perform(_keys)
      fulfill(:self, load(:self))
    end
  end

  class UnsafeKey
    def inspect
      raise "inspect must not be called"
    end
  end

  class UnsafeMutualWaitLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each do |key|
        dependency = key.is_a?(UnsafeKey) ? 0 : UnsafeKey.new
        fulfill(key, load(dependency).sync)
      end
    end

    def cache_key(key)
      key.is_a?(UnsafeKey) ? :unsafe : key
    end
  end

  def setup
    GraphQL::Batch::Executor.current = GraphQL::Batch::Executor.new
  end

  def teardown
    GraphQL::Batch::Executor.current = nil
  end

  def test_two_keys_waiting_on_each_other
    loader = MutualWaitLoader.for
    promise_a = loader.load(0)
    promise_b = loader.load(1)

    error = assert_raises(GraphQL::Batch::DependencyCycleError) { promise_a.sync }

    assert_equal [loader], error.loaders.uniq
    assert_equal [0, 1, 0], error.keys
    assert_equal [[loader, 0], [loader, 1], [loader, 0]], error.chain
    assert_equal true, promise_a.rejected?
    assert_equal true, promise_b.rejected?
    assert_equal error, promise_a.reason
    assert_equal error, promise_b.reason
  end

  def test_three_loader_dependency_cycle
    loader_a = CyclicLoaderA.for
    loader_b = CyclicLoaderB.for
    loader_c = CyclicLoaderC.for
    loader_a.next_loader = loader_b
    loader_b.next_loader = loader_c
    loader_c.next_loader = loader_a

    promise_a = loader_a.load(:a)
    promise_b = loader_b.load(:b)
    promise_c = loader_c.load(:c)

    error = assert_raises(GraphQL::Batch::DependencyCycleError) { promise_a.sync }

    assert_equal [loader_a, loader_b, loader_c, loader_a], error.loaders
    assert_equal [:a, :b, :c, :a], error.keys
    assert_equal [[loader_a, :a], [loader_b, :b], [loader_c, :c], [loader_a, :a]], error.chain
    assert_equal true, promise_a.rejected?
    assert_equal true, promise_b.rejected?
    assert_equal true, promise_c.rejected?
    assert_equal error, promise_a.reason
    assert_equal error, promise_b.reason
    assert_equal error, promise_c.reason
  end

  def test_legal_reentrant_loader
    assert_equal 2, ReentrantLoader.load(2).sync
  end

  def test_dependency_cycle_through_promise_group
    loader_a = GroupCycleLoaderA.for
    loader_b = GroupCycleLoaderB.for
    loader_a.next_loader = loader_b
    loader_b.next_loader = loader_a

    promise_a = loader_a.load(:a)
    promise_b = loader_b.load(:b)

    GraphQL::Batch::Executor.current.wait_all
    error = assert_raises(GraphQL::Batch::DependencyCycleError) { promise_a.sync }

    assert_equal [[loader_b, :b], [loader_a, :a], [loader_b, :b]], error.chain
    assert_equal true, promise_a.rejected?
    assert_equal true, promise_b.rejected?
  end

  def test_single_key_dependency_cycle_through_promise_group
    loader = SelfGroupCycleLoader.for
    promise = loader.load(:self)

    GraphQL::Batch::Executor.current.wait_all
    error = assert_raises(GraphQL::Batch::DependencyCycleError) { promise.sync }

    assert_equal [[loader, :self], [loader, :self]], error.chain
    assert_equal true, promise.rejected?
    assert_equal error, promise.reason
  end

  def test_single_key_dependency_cycle
    loader = SelfCycleLoader.for
    promise = loader.load(:self)

    error = assert_raises(GraphQL::Batch::DependencyCycleError) { promise.sync }

    assert_equal [[loader, :self], [loader, :self]], error.chain
    assert_equal true, promise.rejected?
    assert_equal error, promise.reason
  end

  def test_diamond_shared_dependency
    assert_equal [[:left, :shared], [:right, :shared]], DiamondRootLoader.load(:root).sync
  end

  def test_completed_key_does_not_create_cycle
    assert_equal :ready, EchoLoader.load(:ready).sync
    assert_equal :ready, EchoLoader.load(:ready).sync
  end

  def test_key_fulfilled_with_another_loader_promise_can_be_waited
    assert_equal :ready, FulfilledByIndependentLoaderLoader.load(1).sync
  end

  def test_unrelated_request_remains_executable_after_cycle
    loader = MutualWaitLoader.for
    loader.load(0)
    loader.load(1)
    independent_promise = loader.load(2)
    independent_loader = EchoLoader.for
    separate_promise = independent_loader.load(:independent)

    assert_raises(GraphQL::Batch::DependencyCycleError) { loader.load(0).sync }

    assert_equal :independent, independent_promise.sync
    assert_equal :independent, separate_promise.sync
    assert_equal :later, independent_loader.load(:later).sync
    assert_equal false, GraphQL::Batch::Executor.current.loading
  end

  def test_cycle_message_does_not_inspect_key
    loader = UnsafeMutualWaitLoader.for
    loader.load(0)
    loader.load(UnsafeKey.new)

    error = assert_raises(GraphQL::Batch::DependencyCycleError) { loader.load(0).sync }

    assert_includes error.message, "GraphQL::Batch::DependencyCycleTest::UnsafeMutualWaitLoader"
    assert_includes error.message, "<GraphQL::Batch::DependencyCycleTest::UnsafeKey:0x"
  end
end
