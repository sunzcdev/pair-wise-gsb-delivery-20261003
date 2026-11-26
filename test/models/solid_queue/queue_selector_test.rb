require "test_helper"

class SolidQueue::QueueSelectorTest < ActiveSupport::TestCase
  setup do
    @relation = SolidQueue::ReadyExecution
  end

  test "defaults to the strict strategy" do
    selector = SolidQueue::QueueSelector.new(%w[ high low ], @relation)

    assert_instance_of SolidQueue::QueueSelector::StrictStrategy, selector.strategy
  end

  test "strict strategy keeps the configured queue order on every round" do
    selector = SolidQueue::QueueSelector.new(%w[ high low ], @relation, strategy: :strict)

    3.times do
      assert_equal %w[ high low ], selector.scoped_relations.map { |relation| relation.where_values_hash.fetch("queue_name") }
    end
  end

  test "fair strategy rotates the queue order each round within the window" do
    selector = SolidQueue::QueueSelector.new(%w[ high low ], @relation, strategy: :fair, window: 2)

    assert_equal %w[ high low ], queue_names(selector)
    assert_equal %w[ low high ], queue_names(selector)
    assert_equal %w[ high low ], queue_names(selector)
    assert_equal %w[ low high ], queue_names(selector)
  end

  test "fair strategy with a window larger than the number of queues visits every queue" do
    selector = SolidQueue::QueueSelector.new(%w[ one two three ], @relation, strategy: :fair, window: 5)

    orders = 3.times.map { queue_names(selector) }
    assert_equal [%w[ one two three ], %w[ two three one ], %w[ three one two ]], orders
  end

  test "fair strategy does not rotate a single queue" do
    selector = SolidQueue::QueueSelector.new(%w[ only ], @relation, strategy: :fair)

    3.times { assert_equal %w[ only ], queue_names(selector) }
  end

  test "fair strategy guarantees the last queue leads within window rounds" do
    selector = SolidQueue::QueueSelector.new(%w[ first second third ], @relation, strategy: :fair, window: 3)

    leading_queues = 3.times.map { queue_names(selector).first }
    assert_equal %w[ first second third ], leading_queues
  end

  test "duplicate wildcard declarations matching the same queue are not counted twice" do
    AddToBufferJob.set(queue: "queue_a1").perform_later(1)
    AddToBufferJob.set(queue: "queue_b1").perform_later(1)

    selector = SolidQueue::QueueSelector.new(%w[ queue_* queue_a* queue_a1 ], @relation, strategy: :fair)

    # `queue_a1` is matched by all three entries but must appear exactly once,
    # so the rotation only ever sees two distinct queues.
    4.times do
      names = queue_names(selector)
      assert_equal names.uniq, names
      assert_equal %w[ queue_a1 queue_b1 ].sort, names.sort
    end
  end

  test "paused queues stay excluded under the fair strategy" do
    AddToBufferJob.set(queue: "high").perform_later(1)
    AddToBufferJob.set(queue: "paused").perform_later(1)
    SolidQueue::Queue.find_by_name("paused").pause

    selector = SolidQueue::QueueSelector.new(%w[ high paused ], @relation, strategy: :fair)

    assert_equal %w[ high ], queue_names(selector)
  end

  test "fair strategy ignores rotation for the all-queues wildcard, which stays a single relation" do
    selector = SolidQueue::QueueSelector.new("*", @relation, strategy: :fair)

    3.times { assert_equal 1, selector.scoped_relations.size }
  end

  test "build_strategy accepts a strategy instance and passes window options through" do
    strategy = SolidQueue::QueueSelector::FairStrategy.new(window: 4)
    selector = SolidQueue::QueueSelector.new(%w[ high low ], @relation, strategy: strategy)

    assert_same strategy, selector.strategy
    assert_equal 4, selector.strategy.window
  end

  test "build_strategy rejects unknown strategy names" do
    error = assert_raises(ArgumentError) do
      SolidQueue::QueueSelector.new(%w[ high low ], @relation, strategy: :round_robin)
    end

    assert_match(/Unknown queue selection strategy/, error.message)
  end

  test "fair strategy rejects non-positive windows" do
    assert_raises(ArgumentError) do
      SolidQueue::QueueSelector::FairStrategy.new(window: 0)
    end
  end

  private
    def queue_names(selector)
      selector.scoped_relations.map { |relation| relation.where_values_hash.fetch("queue_name") }
    end
end
