require_relative 'test_helper'

class GraphQL::Batch::MaxBatchSizeTest < Minitest::Test
  # Records every array of keys handed to perform and fulfills them with the
  # keys doubled, so perform calls can be inspected after resolution.
  class RecordingLoader < GraphQL::Batch::Loader
    max_batch_size 2

    def perform(keys)
      performed_keys << keys.dup
      keys.each { |key| fulfill(key, key * 2) }
    end

    def performed_keys
      @performed_keys ||= []
    end
  end

  class UnlimitedRecordingLoader < RecordingLoader
    def max_batch_size
      nil
    end
  end

  class CallbackAppendingLoader < RecordingLoader
    # Fulfills every key, and appends a new key (only once) from within a
    # promise callback. The newly queued key must land in a later wave instead
    # of mutating the slices of the current wave.
    def perform(keys)
      performed_keys << keys.dup
      keys.each { |key| fulfill(key, key) }

      unless fulfilled?(99)
        keys.first.then { |_| self.class.for.load(99) }
      end
    end
  end

  class SecondShardFailingLoader < GraphQL::Batch::Loader
    max_batch_size 2

    def perform(keys)
      raise 'second shard failed' if keys.first == 3

      keys.each { |key| fulfill(key, key) }
    end
  end

  class InstanceSizeLoader < RecordingLoader
    def max_batch_size
      3
    end
  end

  class InstanceBadSizeLoader < RecordingLoader
    def max_batch_size
      -1
    end
  end

  # Keys 3 and 4 are only queued from a callback fired while the first slice
  # (1,2) performs. All slices of the wave are decided up front, so the callback
  # cannot enlarge the second slice of the current wave.
  class LateAppendingLoader < RecordingLoader
    def perform(keys)
      performed_keys << keys.dup
      keys.each { |key| fulfill(key, key * 2) }

      if keys == [1, 2]
        keys.first.then do |_|
          self.class.for.load(3)
          self.class.for.load(4)
        end
      end
    end
  end

  def setup
    GraphQL::Batch::Executor.current = GraphQL::Batch::Executor.new
  end

  def teardown
    GraphQL::Batch::Executor.current = nil
  end

  def test_default_keeps_single_perform_call
    loader = UnlimitedRecordingLoader.for

    assert_equal [2, 4, 6, 8, 10], loader.load_many([1, 2, 3, 4, 5]).sync
    assert_equal [[1, 2, 3, 4, 5]], loader.performed_keys
  end

  def test_slices_in_enqueue_order_at_boundary_sizes
    loader = RecordingLoader.for

    assert_equal [2, 4, 6, 8, 10], loader.load_many([1, 2, 3, 4, 5]).sync
    assert_equal [[1, 2], [3, 4], [5]], loader.performed_keys
  end

  def test_exact_boundary
    loader = RecordingLoader.for

    assert_equal [2, 4, 6, 8], loader.load_many([1, 2, 3, 4]).sync
    assert_equal [[1, 2], [3, 4]], loader.performed_keys
  end

  def test_size_one_slices_every_key
    loader_class = Class.new(GraphQL::Batch::Loader) do
      max_batch_size 1

      def perform(keys)
        performed_keys << keys.dup
        keys.each { |key| fulfill(key, key * 3) }
      end

      def performed_keys
        @performed_keys ||= []
      end
    end

    loader = loader_class.for

    assert_equal [3, 6, 9], loader.load_many([1, 2, 3]).sync
    assert_equal [[1], [2], [3]], loader.performed_keys
  end

  def test_instance_max_batch_size_override
    loader = InstanceSizeLoader.for

    assert_equal [2, 4, 6, 8], loader.load_many([1, 2, 3, 4]).sync
    assert_equal [[1, 2, 3], [4]], loader.performed_keys
  end

  def test_duplicate_keys_are_coalesced_and_promise_identity_preserved
    loader = RecordingLoader.for

    first_promise = loader.load(1)
    second_promise = loader.load(1)

    assert_same first_promise, second_promise

    assert_equal [2, 2, 4, 6], loader.load_many([1, 1, 2, 3]).sync
    assert_equal [[1, 2], [3]], loader.performed_keys
    assert_same first_promise, loader.load(1)
  end

  def test_load_many_results_stay_aligned_with_input_keys
    loader = RecordingLoader.for
    keys = (1..5).to_a + [1, 3]

    assert_equal keys.map { |key| key * 2 }, loader.load_many(keys).sync
  end

  def test_callback_appended_key_joins_next_wave_without_broken_promise
    loader = CallbackAppendingLoader.for
    promise_a = loader.load(1)
    promise_b = loader.load(2)

    executor.resolve(loader)

    assert_equal 1, promise_a.sync
    assert_equal 2, promise_b.sync
    assert_equal [[1, 2]], loader.performed_keys

    extra_promise = loader.load(99)
    executor.resolve(loader)

    assert_equal 99, extra_promise.sync
    assert_equal [[1, 2], [99]], loader.performed_keys
  end

  def test_callback_appended_key_is_not_mistaken_for_broken_in_current_wave
    loader = CallbackAppendingLoader.for
    promise_a = loader.load(1)
    promise_b = loader.load(2)

    promise_a.sync

    assert_equal 1, promise_a.value
    assert_equal 2, promise_b.value
    assert_equal 99, loader.load(99).sync
  end

  def test_callback_cannot_grow_a_later_slice_of_the_same_wave
    loader = LateAppendingLoader.for
    promises = [loader.load(1), loader.load(2)]

    executor.resolve(loader)

    assert_equal [2, 4], promises.map(&:sync)
    assert_equal [[1, 2]], loader.performed_keys

    later_promises = [loader.load(3), loader.load(4)]

    executor.resolve(loader)

    assert_equal [6, 8], later_promises.map(&:sync)
    assert_equal [[1, 2], [3, 4]], loader.performed_keys
  end

  def test_failing_shard_only_rejects_its_unfinished_keys
    loader = SecondShardFailingLoader.for
    promises = (1..4).map { |key| loader.load(key) }

    GraphQL::Batch::Executor.current.wait_all

    assert_equal 1, promises[0].value
    assert_equal 2, promises[1].value

    assert promises[2].rejected?
    assert promises[3].rejected?
    assert_equal 'second shard failed', promises[2].reason.message
    assert_equal 'second shard failed', promises[3].reason.message
  end

  def test_remaining_shards_still_run_after_a_failing_shard
    loader = SecondShardFailingLoader.for
    loader.define_singleton_method(:perform) do |keys|
      if keys.first == 3
        fulfill(keys.first, keys.first)
        raise 'second shard failed'
      end

      keys.each { |key| fulfill(key, key) }
    end

    promises = (1..6).map { |key| loader.load(key) }

    GraphQL::Batch::Executor.current.wait_all

    assert_equal [1, 2], promises.values_at(0, 1).map(&:value)

    # The first key of the failing shard was fulfilled before the raise.
    assert_equal 3, promises[2].value
    assert promises[3].rejected?
    assert_equal 'second shard failed', promises[3].reason.message

    assert_equal [5, 6], promises.values_at(4, 5).map(&:value)
  end

  def test_zero_max_batch_size_rejected_at_declaration
    error = assert_raises(ArgumentError) do
      Class.new(GraphQL::Batch::Loader) do
        max_batch_size 0
      end
    end

    assert_match(/positive integer/, error.message)
  end

  def test_negative_max_batch_size_rejected_at_declaration
    error = assert_raises(ArgumentError) do
      Class.new(GraphQL::Batch::Loader) do
        max_batch_size(-2)
      end
    end

    assert_match(/positive integer/, error.message)
  end

  def test_non_integer_max_batch_size_rejected_at_declaration
    error = assert_raises(ArgumentError) do
      Class.new(GraphQL::Batch::Loader) do
        max_batch_size 2.5
      end
    end

    assert_match(/positive integer/, error.message)
  end

  def test_bad_instance_max_batch_size_rejects_pending_promises
    loader = InstanceBadSizeLoader.for
    promise = loader.load(1)

    promise.wait

    assert promise.rejected?
    assert_kind_of ArgumentError, promise.reason
  end

  def test_max_batch_size_is_inherited_per_subclass
    assert_equal 2, RecordingLoader.max_batch_size
    assert_equal 2, CallbackAppendingLoader.max_batch_size
    assert_nil UnlimitedRecordingLoader.for.max_batch_size
  end

  private

  def executor
    GraphQL::Batch::Executor.current
  end
end
