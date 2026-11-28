# frozen_string_literal: true

# This spec pins the difference between a selection that was never polled (no local capacity) and an SQS answer that
# really was an empty list, plus the age of the reports the dispatch loop feeds back to the strategy.
#
# WeightedRoundRobin drives its rotation from those reports, so a report about a state the queue has already left must
# not change the state: otherwise a late empty answer pauses a queue that has already recovered.
RSpec.describe Shoryuken::Polling::WeightedRoundRobin, 'capacity and stale reports' do
  let(:queue_a) { 'queue-a' }
  let(:queue_b) { 'queue-b' }
  # A has two weights, B one
  let(:queues) { [queue_a, queue_a, queue_b] }
  let(:delay) { 10 }

  subject(:strategy) { described_class.new(queues, delay) }

  # Runs one dispatch pass the way Manager does: select a queue, then report what actually happened to that selection
  def select
    strategy.next_queue
  end

  def polled(selection, count)
    strategy.messages_found(selection.name, count, selection.poll_id)
  end

  def polled_empty(selection)
    polled(selection, 0)
  end

  def skipped(selection)
    strategy.poll_skipped(selection.name, selection.poll_id)
  end

  it 'keeps A at its recovered weight and holds B to its backoff when A was skipped and B answered empty' do
    # A really gets messages and recovers its weight of 2
    a = select
    expect(a.name).to eq(queue_a)
    polled(a, 2)
    expect(strategy.active_queues.to_h).to eq(queue_a => 2, queue_b => 1)

    # B is selected next, and its request is still on its way
    b = select
    expect(b.name).to eq(queue_b)

    # A comes around again, but the group has no free processor: no request, so no empty answer
    a_without_capacity = select
    expect(a_without_capacity.name).to eq(queue_a)
    skipped(a_without_capacity)

    # The skipped turn gives its place back, but A keeps its weight of 2
    expect(strategy.active_queues.to_h).to eq(queue_a => 2, queue_b => 1)

    # B's request really returned an empty list: B goes into its backoff
    polled_empty(b)

    expect(strategy.active_queues.to_h).to eq(queue_a => 2)

    # The next round turns on A's recovered weight of 2 while B waits out its backoff
    names = Array.new(4) { select.name }
    expect(names).to eq([queue_a, queue_a, queue_a, queue_a])

    # No answer can belong to the skipped selection: SQS was never asked for it
    polled_empty(a_without_capacity)
    expect(strategy.active_queues.to_h).to eq(queue_a => 2)
  end

  it 'does not let a late empty answer for an earlier round pause the queue that recovered meanwhile' do
    # An empty answer is requested for A, but the answer is delayed
    delayed = select
    expect(delayed.name).to eq(queue_a)

    # A is selected again and really gets messages before the delayed answer arrives
    a = select
    expect(a.name).to eq(queue_b)
    polled_empty(a) # B really is empty, so it backs off

    a = select
    expect(a.name).to eq(queue_a)
    polled(a, 2)

    # The delayed empty answer for the first selection arrives now: A has moved on, so it is ignored
    polled_empty(delayed)

    # A keeps the weight its recovering poll gave it, and B stays in its backoff
    expect(strategy.active_queues.to_h).to eq(queue_a => 2)
  end

  it 'does not let an old empty answer restart a backoff that already passed' do
    now = Time.now
    allow(Time).to receive(:now) { now }

    first = select
    expect(first.name).to eq(queue_a)
    polled_empty(first)
    expect(strategy.active_queues.to_h).to eq(queue_b => 1)

    now += delay + 1

    # The backoff passed, so A is unpaused and selected again
    expect(select.name).to eq(queue_b)
    a = select
    expect(a.name).to eq(queue_a)
    polled(a, 2)

    # Replaying the old empty answer must not pause A for another delay
    polled_empty(first)

    expect(strategy.active_queues.to_h).to eq(queue_a => 2, queue_b => 1)
  end

  it 'ignores an empty answer for a selection made before a processed message resumed the queue' do
    first = select
    expect(first.name).to eq(queue_a)
    polled_empty(first)
    expect(strategy.active_queues.to_h).to eq(queue_b => 1)

    strategy.message_processed(queue_a) # a message already in flight completed
    expect(select).not_to be_nil # implicitly unpauses A
    expect(strategy.active_queues.to_h).to eq(queue_a => 1, queue_b => 1)

    # The first round's empty answer arrives after A was resumed: it must not pause A again
    polled_empty(first)

    expect(strategy.active_queues.to_h).to eq(queue_a => 1, queue_b => 1)
  end

  it 'does not let a skipped selection change the weight or pause the queue' do
    selection = select
    weight = strategy.active_queues.to_h[selection.name]

    skipped(selection)

    expect(strategy.active_queues.to_h[selection.name]).to eq(weight)
    expect(strategy.active_queues.to_h.keys).to include(selection.name)
  end
end
