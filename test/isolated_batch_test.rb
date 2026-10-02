require_relative 'test_helper'

class GraphQL::Batch::IsolatedBatchTest < Minitest::Test
  def setup
    @queries = []
    QueryNotifier.subscriber = ->(query) { @queries << query }
  end

  def teardown
    QueryNotifier.subscriber = nil
  end

  def test_isolated_batch_uses_fresh_executor
    outer_executor = nil
    isolated_executor = nil

    GraphQL::Batch.batch do
      outer_executor = GraphQL::Batch::Executor.current
      GraphQL::Batch.batch(isolated: true) do
        isolated_executor = GraphQL::Batch::Executor.current
      end
      assert_equal outer_executor, GraphQL::Batch::Executor.current
    end

    refute_nil outer_executor
    refute_nil isolated_executor
    refute_equal outer_executor, isolated_executor
    assert_nil GraphQL::Batch::Executor.current
  end

  def test_nested_batch_inside_isolated_batch_shares_isolated_executor
    promise1 = nil
    promise2 = nil

    GraphQL::Batch.batch(isolated: true) do
      promise1 = RecordLoader.for(Product).load(1)
      GraphQL::Batch.batch do
        promise2 = RecordLoader.for(Product).load(1)
      end
      promise1
    end

    assert_equal promise1, promise2
    assert_equal ['Product/1'], @queries
  end

  def test_three_level_mixed_nesting
    outer_promise = nil
    isolated_promise = nil
    innermost_promise = nil

    GraphQL::Batch.batch do
      outer_promise = RecordLoader.for(Product).load(1)
      GraphQL::Batch.batch(isolated: true) do
        isolated_promise = RecordLoader.for(Product).load(1)
        GraphQL::Batch.batch do
          innermost_promise = RecordLoader.for(Product).load(1)
        end
        isolated_promise
      end
      outer_promise
    end

    assert_equal 'Shirt', outer_promise.sync.title
    assert_equal 'Shirt', isolated_promise.sync.title
    assert_equal isolated_promise, innermost_promise
    refute_equal outer_promise, isolated_promise
    # The isolated scope re-loads the same key with its own cache
    assert_equal ['Product/1', 'Product/1'], @queries
    assert_nil GraphQL::Batch::Executor.current
  end

  def test_isolated_batch_does_not_reuse_outer_loader_cache
    outer_promise = nil
    isolated_promise = nil

    GraphQL::Batch.batch do
      outer_promise = RecordLoader.for(Product).load(1)
      GraphQL::Batch.batch(isolated: true) do
        isolated_promise = RecordLoader.for(Product).load(1)
      end
      outer_promise
    end

    refute_equal outer_promise, isolated_promise
    assert_equal 'Shirt', outer_promise.sync.title
    assert_equal 'Shirt', isolated_promise.sync.title
    assert_equal ['Product/1', 'Product/1'], @queries
  end

  def test_parent_cache_survives_isolated_batch_and_keeps_loading
    GraphQL::Batch.batch do
      shirt = RecordLoader.for(Product).load(1)
      pants = RecordLoader.for(Product).load(2)
      assert_equal 'Shirt', shirt.sync.title
      assert_equal 'Pants', pants.sync.title
      assert_equal ['Product/1,2'], @queries

      GraphQL::Batch.batch(isolated: true) do
        # Same loader arguments and key: misses the isolated cache
        RecordLoader.for(Product).load(1)
      end
      assert_equal ['Product/1,2', 'Product/1'], @queries

      # Parent cache was preserved while the isolated batch ran
      assert_equal 'Shirt', RecordLoader.for(Product).load(1).sync.title
      assert_equal ['Product/1,2', 'Product/1'], @queries

      # Parent executor still accepts and batches new keys after restore
      assert_equal 'Sweater', RecordLoader.for(Product).load(3).sync.title
      assert_equal ['Product/1,2', 'Product/1', 'Product/3'], @queries
    end
  end

  def test_isolated_batch_exception_restores_parent_executor
    parent_executor = nil
    pending_promise = nil

    assert_raises(RuntimeError) do
      GraphQL::Batch.batch do
        parent_executor = GraphQL::Batch::Executor.current
        pending_promise = RecordLoader.for(Product).load(1)
        begin
          GraphQL::Batch.batch(isolated: true) do
            RecordLoader.for(Product).load(2)
            raise 'boom'
          end
        rescue RuntimeError
          # Parent executor is restored even though the isolated batch blew up
          assert_equal parent_executor, GraphQL::Batch::Executor.current
          # Parent query was not executed early by the isolated batch
          assert_equal [], @queries
          raise
        end
      end
    end

    # Parent queue was not cleared: the pending load still resolves
    assert_equal 'Shirt', pending_promise.sync.title
    assert_equal ['Product/1'], @queries
    assert_nil GraphQL::Batch::Executor.current
  end

  def test_isolated_batch_inside_promise_callback
    isolated_executor = nil
    parent_executor = nil
    result = nil

    GraphQL::Batch.batch do
      parent_executor = GraphQL::Batch::Executor.current
      RecordLoader.for(Product).load(1).then do |product|
        GraphQL::Batch.batch(isolated: true) do
          isolated_executor = GraphQL::Batch::Executor.current
          RecordLoader.for(Product).load(product.id)
        end
      end.then do |isolated_product|
        assert_equal parent_executor, GraphQL::Batch::Executor.current
        result = isolated_product
      end
    end

    assert_equal 'Shirt', result.title
    refute_equal parent_executor, isolated_executor
    assert_equal ['Product/1', 'Product/1'], @queries
  end

  def test_isolated_batch_returns_promise_settled_in_inner_scope
    product = GraphQL::Batch.batch do
      GraphQL::Batch.batch(isolated: true) do
        RecordLoader.for(Product).load(1).then do |product|
          # Chained loads must settle while the isolated executor is active
          RecordLoader.for(ProductVariant).load(product.id)
        end
      end
    end

    assert_equal 'Red', product.title
    assert_equal ['Product/1', 'ProductVariant/1'], @queries
    assert_nil GraphQL::Batch::Executor.current
  end

  def test_isolated_batch_in_other_thread_is_independent
    main_executor = nil
    thread_executor = :unset
    thread_executor_after = :unset

    GraphQL::Batch.batch do
      main_executor = GraphQL::Batch::Executor.current
      thread = Thread.new do
        GraphQL::Batch.batch(isolated: true) do
          thread_executor = GraphQL::Batch::Executor.current
          RecordLoader.for(Product).load(1)
        end
        thread_executor_after = GraphQL::Batch::Executor.current
      end
      thread.join
      assert_equal main_executor, GraphQL::Batch::Executor.current
    end

    refute_equal main_executor, thread_executor
    assert_nil thread_executor_after
    assert_equal ['Product/1'], @queries
  end
end
