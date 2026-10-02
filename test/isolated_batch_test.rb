require_relative 'test_helper'

class GraphQL::Batch::IsolatedBatchTest < Minitest::Test
  class ExplodingLoader < GraphQL::Batch::Loader
    def perform(_keys)
      raise 'perform failed'
    end
  end

  def setup
    @queries = []
    QueryNotifier.subscriber = ->(query) { @queries << query }
  end

  def teardown
    QueryNotifier.subscriber = nil
  end

  def test_isolated_batch_builds_a_new_executor_loader_registry_and_queue
    parent_executor = nil
    parent_loader = nil
    inner_executor = nil
    inner_loader = nil

    product = GraphQL::Batch.batch do
      parent_executor = GraphQL::Batch::Executor.current
      parent_loader = RecordLoader.for(Product)
      parent_promise = parent_loader.load(1)

      result = GraphQL::Batch.batch(isolated: true) do
        inner_executor = GraphQL::Batch::Executor.current
        inner_loader = RecordLoader.for(Product)
        refute_same parent_executor, inner_executor
        refute_same parent_loader, inner_loader
        inner_loader.load(1)
      end

      assert_equal 'Shirt', result.title
      # The parent executor is restored with its queued, still cached request.
      assert_same parent_executor, GraphQL::Batch::Executor.current
      assert_same parent_promise, parent_loader.load(1)
      parent_promise
    end

    assert_equal 'Shirt', product.title
    # The same loader argument and key were loaded independently, so the
    # parent's queued request still performs when the parent batch resumes.
    assert_equal ['Product/1', 'Product/1'], @queries
    assert_nil GraphQL::Batch::Executor.current
  end

  def test_three_level_mixed_nesting
    parent_executor = parent_loader = nil
    middle_executor = middle_normal_loader = middle_isolated_loader = nil
    inner_executor = nil

    parent_promise = nil
    inner_promise = nil
    middle_late_promise = nil

    results = GraphQL::Batch.batch do
      parent_executor = GraphQL::Batch::Executor.current
      parent_loader = RecordLoader.for(Product)
      parent_promise = parent_loader.load(1)

      middle_results = GraphQL::Batch.batch(isolated: true) do
        middle_executor = GraphQL::Batch::Executor.current

        # A plain nested batch keeps sharing the middle executor/registry.
        GraphQL::Batch.batch do
          middle_normal_loader = RecordLoader.for(Product)
          middle_normal_loader.load(3)
          assert_same middle_executor, GraphQL::Batch::Executor.current
          middle_executor.wait_all
        end

        # The parent batch is still paused after the plain-nested flush.
        assert_equal ['Product/3'], @queries

        inner_promise = GraphQL::Batch.batch(isolated: true) do
          inner_executor = GraphQL::Batch::Executor.current
          RecordLoader.for(Product).load(2)
        end

        middle_isolated_loader = RecordLoader.for(Product)
        middle_late_promise = middle_isolated_loader.load(3)

        # Both the plain-nested and late loads use the middle executor, so they
        # are isolated from both the parent and the innermost registries.
        assert_same middle_executor, middle_isolated_loader.executor
        Promise.all([inner_promise, middle_late_promise])
      end

      # The parent executor is restored while the outer block is still active.
      assert_same parent_executor, GraphQL::Batch::Executor.current

      Promise.all([parent_promise, middle_results]).then do |parent, middles|
        [parent.title, middles.map(&:title)]
      end
    end

    assert_equal ['Shirt', ['Pants', 'Sweater']], results

    refute_same parent_executor, middle_executor
    refute_same middle_executor, inner_executor

    # Same loader arguments/keys produce distinct registries per executor.
    refute_same parent_loader, middle_normal_loader

    # Plain nesting reused the middle executor even around an isolated child.
    assert_same middle_normal_loader.executor, middle_isolated_loader.executor
    # The isolated child queries run first; the paused parent batch flushes last.
    assert_equal ['Product/3', 'Product/2', 'Product/3', 'Product/1'], @queries
  end

  def test_inner_exception_restores_parent_executor_and_keeps_parent_queue
    parent_executor = nil
    parent_loader = nil
    parent_promise = nil

    product = GraphQL::Batch.batch do
      parent_executor = GraphQL::Batch::Executor.current
      parent_loader = RecordLoader.for(Product)
      parent_promise = parent_loader.load(2)

      error = assert_raises(RuntimeError) do
        GraphQL::Batch.batch(isolated: true) do
          raise 'inner boom'
        end
      end
      assert_equal 'inner boom', error.message

      # The parent executor is restored, its cache and queue are untouched, and
      # the inner failure never flushed the parent's request.
      assert_same parent_executor, GraphQL::Batch::Executor.current
      assert_same parent_promise, parent_loader.load(2)
      assert_equal [], @queries

      # A new isolated batch works normally after the failed one.
      pants = GraphQL::Batch.batch(isolated: true) do
        RecordLoader.for(Product).load(2)
      end
      assert_equal 'Pants', pants.title

      parent_promise
    end

    assert_equal 'Pants', product.title
    assert_equal ['Product/2', 'Product/2'], @queries
    assert_nil GraphQL::Batch::Executor.current
  end

  def test_inner_rejected_promise_restores_parent_executor
    parent_executor = nil
    parent_loader = nil
    parent_promise = nil

    GraphQL::Batch.batch do
      parent_executor = GraphQL::Batch::Executor.current
      parent_loader = RecordLoader.for(Product)
      parent_promise = parent_loader.load(1)

      assert_raises(RuntimeError) do
        GraphQL::Batch.batch(isolated: true) do
          ExplodingLoader.load(1)
        end
      end

      assert_same parent_executor, GraphQL::Batch::Executor.current
      assert_same parent_promise, parent_loader.load(1)
      assert_equal [], @queries
      parent_promise
    end

    assert_equal ['Product/1'], @queries
    assert_nil GraphQL::Batch::Executor.current
  end

  def test_isolated_batch_inside_promise_callback
    parent_executor = inner_executor = nil
    seen_in_remaining_callback = nil

    value = GraphQL::Batch.batch do
      parent_executor = GraphQL::Batch::Executor.current
      RecordLoader.for(Product).load(1).then do |product|
        title = GraphQL::Batch.batch(isolated: true) do
          inner_executor = GraphQL::Batch::Executor.current
          refute_same parent_executor, inner_executor
          RecordLoader.for(ProductVariant).load(2).then(&:title)
        end
        "#{product.title}:#{title}"
      end.then do |combined|
        seen_in_remaining_callback = GraphQL::Batch::Executor.current
        combined
      end
    end

    assert_equal 'Shirt:Blue', value
    assert_same parent_executor, seen_in_remaining_callback
    assert_nil GraphQL::Batch::Executor.current
    assert_equal ['Product/1', 'ProductVariant/2'], @queries
  end

  def test_returned_promise_settles_inside_the_inner_scope
    loaded_in_inner_scope = nil
    parent_executor = nil

    result = GraphQL::Batch.batch do
      parent_executor = GraphQL::Batch::Executor.current
      # Queue a parent load, which stays paused while the isolated block runs.
      parent_promise = RecordLoader.for(Product).load(1)

      value = GraphQL::Batch.batch(isolated: true) do
        variant_promise = RecordLoader.for(ProductVariant).load(1)
        variant_promise.then do |variant|
          loaded_in_inner_scope = GraphQL::Batch::Executor.current
          refute_same parent_executor, loaded_in_inner_scope
          image_id = Image.fixtures.values.find do |image|
            image.owner_type == 'ProductVariant' && image.owner_id == variant.id
          end.id
          RecordLoader.for(Image).load(image_id)
        end.then(&:filename)
      end

      # The inner chain is fully resolved to a plain value before returning.
      assert_equal 'red-shirt.jpg', value

      parent_promise.then { |product| [product.title, value] }
    end

    assert_equal ['Shirt', 'red-shirt.jpg'], result
    refute_nil loaded_in_inner_scope
    # The inner chain fully settles before the parent's paused request resumes.
    assert_equal ['ProductVariant/1', 'Image/4', 'Product/1'], @queries
  end

  def test_other_pending_inner_loads_still_resolve_inside_scope
    deferred_promise = nil

    GraphQL::Batch.batch(isolated: true) do
      deferred_promise = RecordLoader.for(Product).load(3)
      # Do not return the promise: it must still resolve before the scope exits,
      # so it can never wait on a loader from a torn-down scope.
      'ignored'
    end

    assert_equal 'Sweater', deferred_promise.sync.title
    assert_equal ['Product/3'], @queries
  end

  def test_threads_remain_independent
    main_executor = nil
    main_inner_executor = nil
    thread_result = nil
    thread_queries = nil

    thread = Thread.new do
      local = []
      QueryNotifier.subscriber = ->(query) { local << query }
      thread_result = GraphQL::Batch.batch do
        GraphQL::Batch.batch(isolated: true) do
          RecordLoader.for(Product).load(3)
        end
      end
      thread_queries = local
      QueryNotifier.subscriber = nil
    end

    main_result = GraphQL::Batch.batch do
      main_executor = GraphQL::Batch::Executor.current
      GraphQL::Batch.batch(isolated: true) do
        main_inner_executor = GraphQL::Batch::Executor.current
        RecordLoader.for(Product).load(1)
      end
    end

    thread.join

    assert_equal 'Shirt', main_result.title
    assert_equal 'Sweater', thread_result.title
    refute_same main_executor, main_inner_executor
    assert_equal ['Product/1'], @queries
    assert_equal ['Product/3'], thread_queries
  end

  def test_default_nested_batch_still_shares_the_executor_and_cache
    outer_executor = nil
    inner_executor = nil
    outer_promise = nil
    inner_promise = nil

    GraphQL::Batch.batch do
      outer_executor = GraphQL::Batch::Executor.current
      outer_promise = RecordLoader.for(Product).load(1)
      GraphQL::Batch.batch do
        inner_executor = GraphQL::Batch::Executor.current
        inner_promise = RecordLoader.for(Product).load(1)
      end
    end

    assert_same outer_executor, inner_executor
    assert_same outer_promise, inner_promise
    assert_equal ['Product/1'], @queries
  end
end
