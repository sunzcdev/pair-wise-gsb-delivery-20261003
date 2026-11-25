# frozen_string_literal: true

RSpec.describe Shoryuken::Manager, 'fetch slot reservations' do
  let(:queues) { %w[queue-a queue-b] }
  let(:polling_strategy) { Shoryuken::Polling::WeightedRoundRobin.new(queues) }
  let(:fetcher) { double(Shoryuken::Fetcher) }
  let(:executor) { Concurrent::ImmediateExecutor.new }
  let(:concurrency) { 3 }

  subject(:manager) { described_class.new('default', fetcher, polling_strategy, concurrency, executor) }

  before do
    @posted_workers = []
    allow(executor).to receive(:post) { |&block| @posted_workers << block }
    allow(Shoryuken::Client).to receive(:queues).and_return(double(Shoryuken::Queue, fifo?: false))
    allow(Shoryuken::Processor).to receive(:process)
  end

  def queue_config(name)
    Shoryuken::Polling::QueueConfiguration.new(name, {})
  end

  it 'gives slots unused by a partial response back before the next queue fetches' do
    first = double(Shoryuken::Message, message_id: 'a1', 'visibility_timeout=': nil)
    second = double(Shoryuken::Message, message_id: 'b1', 'visibility_timeout=': nil)
    limits = []

    allow(fetcher).to receive(:fetch) do |queue, limit|
      limits << [queue.name, limit]
      queue.name == 'queue-a' ? [first] : [second]
    end

    manager.send(:dispatch_single_messages, queue_config('queue-a'))

    expect(manager.send(:busy)).to eq(1)
    expect(manager.send(:ready)).to eq(2)

    manager.send(:dispatch_single_messages, queue_config('queue-b'))

    expect(limits).to eq([['queue-a', 3], ['queue-b', 2]])
    expect(manager.send(:busy)).to eq(2)

    @posted_workers.each(&:call)

    expect(Shoryuken::Processor).to have_received(:process).with('queue-a', first)
    expect(Shoryuken::Processor).to have_received(:process).with('queue-b', second)
    expect(manager.send(:busy)).to eq(0)
  end

  it 'does not call SQS when every slot is leased' do
    3.times { manager.send(:slots).occupy }

    expect(fetcher).not_to receive(:fetch)

    manager.send(:dispatch_single_messages, queue_config('queue-a'))
  end

  it 'discards a late cancelled response and returns every reservation' do
    late_message = double(Shoryuken::Message, message_id: 'late', 'visibility_timeout=': nil)

    allow(fetcher).to receive(:fetch) do |_queue, limit|
      expect(limit).to eq(3)
      manager.stop_new_dispatching
      [late_message]
    end

    manager.send(:dispatch_single_messages, queue_config('queue-a'))

    expect(manager.send(:busy)).to eq(0)
    expect(manager.send(:ready)).to eq(3)
    expect(late_message).to have_received(:visibility_timeout=).with(0)
    expect(Shoryuken::Processor).not_to have_received(:process)
  end

  it 'shares the three slots when two queues start fetching simultaneously' do
    entered = Queue.new
    release = Queue.new
    limits = []
    limit_mutex = Mutex.new

    allow(fetcher).to receive(:fetch) do |queue, limit|
      limit_mutex.synchronize { limits << [queue.name, limit] }
      entered << queue.name
      release.pop
      []
    end

    threads = queues.map do |name|
      Thread.new { manager.send(:dispatch_single_messages, queue_config(name)) }
    end

    Timeout.timeout(3) { 2.times { entered.pop } }
    2.times { release << true }
    threads.each(&:value)

    expect(limits.map(&:last).sum).to eq(3)
    expect(limits.map(&:last)).to all(be_positive)
    expect(manager.send(:ready)).to eq(3)
  end
end
