require_relative 'test_helper'

class GraphQL::Batch::LoadManySettledTest < Minitest::Test
  class EchoLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each { |key| fulfill(key, key) }
    end
  end

  # Fulfills keys present in VALUES (nil included), rejects keys present
  # in REJECTIONS, and leaves any other key unfulfilled.
  class MixedLoader < GraphQL::Batch::Loader
    VALUES = { a: 1, b: nil, c: 3 }
    REJECTIONS = { rejected: 'nope' }

    attr_reader :performed_keys

    def initialize
      @performed_keys = []
    end

    def perform(keys)
      @performed_keys.concat(keys)
      keys.each do |key|
        if VALUES.key?(key)
          fulfill(key, VALUES[key])
        elsif REJECTIONS.key?(key)
          reject(key, RuntimeError.new(REJECTIONS[key]))
        end
      end
    end
  end

  class ExplodingLoader < GraphQL::Batch::Loader
    def perform(_keys)
      raise 'perform failed'
    end
  end

  class PartialLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each { |key| fulfill(key, key) if key.is_a?(Integer) }
    end
  end

  class NestedLoader < GraphQL::Batch::Loader
    def perform(keys)
      keys.each { |key| fulfill(key, EchoLoader.load(key)) }
    end
  end

  def setup
    GraphQL::Batch::Executor.current = GraphQL::Batch::Executor.new
  end

  def teardown
    GraphQL::Batch::Executor.current = nil
  end

  def test_mixed_success_missing_and_rejection
    results = MixedLoader.load_many_settled([:a, :missing, :b, :rejected]).sync

    assert_equal 4, results.size

    assert results[0].fulfilled?
    assert_equal 1, results[0].value

    assert results[1].rejected?
    assert_equal GraphQL::Batch::BrokenPromiseError, results[1].reason.class

    assert results[2].fulfilled?
    assert_nil results[2].value

    assert results[3].rejected?
    assert_equal 'nope', results[3].reason.message
  end

  def test_nil_value_is_a_success
    result = MixedLoader.load_many_settled([:b]).sync.first

    assert result.fulfilled?
    refute result.rejected?
    assert_nil result.value
    assert_nil result.reason
  end

  def test_rejected_result_exposes_reason_without_guessing
    result = MixedLoader.load_many_settled([:rejected]).sync.first

    assert result.rejected?
    refute result.fulfilled?
    assert_kind_of RuntimeError, result.reason
    assert_nil result.value
  end

  def test_duplicate_keys_share_promise_but_keep_positions
    loader = MixedLoader.for
    results = loader.load_many_settled([:a, :c, :a]).sync

    assert_equal 3, results.size
    assert_equal [1, 3, 1], results.map(&:value)
    assert_equal [:a, :c], loader.performed_keys
  end

  def test_empty_keys
    assert_equal [], EchoLoader.load_many_settled([]).sync
  end

  def test_perform_raising_settles_every_key
    results = ExplodingLoader.load_many_settled([:a, :b, :c]).sync

    assert_equal 3, results.size
    results.each do |result|
      assert result.rejected?
      assert_equal 'perform failed', result.reason.message
    end
  end

  def test_partial_fulfillment_settles_remaining_keys
    results = PartialLoader.load_many_settled([1, :missing, 2]).sync

    assert_equal 3, results.size
    assert results[0].fulfilled?
    assert_equal 1, results[0].value
    assert results[1].rejected?
    assert_equal GraphQL::Batch::BrokenPromiseError, results[1].reason.class
    assert results[2].fulfilled?
    assert_equal 2, results[2].value
  end

  def test_nested_loads_from_callbacks_settle
    results = NestedLoader.load_many_settled([:a, :b]).sync

    assert_equal 2, results.size
    assert_equal [:a, :b], results.map(&:value)
    assert results.all?(&:fulfilled?)
  end

  def test_normal_load_from_same_executor_after_settled_failures
    settled = ExplodingLoader.load_many_settled([:a]).sync
    assert settled.first.rejected?

    assert_equal :ok, EchoLoader.load(:ok).sync
    assert_equal [:x, :y], EchoLoader.load_many([:x, :y]).sync
  end
end
