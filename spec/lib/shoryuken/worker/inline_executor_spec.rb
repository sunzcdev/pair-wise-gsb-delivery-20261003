# frozen_string_literal: true

RSpec.describe Shoryuken::Worker::InlineExecutor do
  before do
    @previous_executor = Shoryuken.worker_executor
    Shoryuken.worker_executor = described_class
  end

  after do
    described_class.disable_fifo_deduplication!
    Shoryuken.worker_executor = @previous_executor
  end

  describe '.perform_async' do
    specify do
      expect_any_instance_of(TestWorker).to receive(:perform).with(anything, 'test')

      TestWorker.perform_async('test')
    end

    it 'properly sets message_attributes' do
      custom_attributes = {
        'custom_key' => { string_value: 'custom_value', data_type: 'String' }
      }

      expect_any_instance_of(TestWorker).to receive(:perform) do |_, sqs_msg, _|
        expect(sqs_msg.message_attributes).to include('shoryuken_class')
        expect(sqs_msg.message_attributes).to include('custom_key')
        expect(sqs_msg.message_attributes['custom_key'][:string_value]).to eq('custom_value')
      end

      TestWorker.perform_async('test', message_attributes: custom_attributes)
    end

    it 'does not mutate the caller-supplied options hash' do
      allow_any_instance_of(TestWorker).to receive(:perform)

      options = { queue: 'a_queue', message_attributes: { 'custom' => { string_value: 'v', data_type: 'String' } } }

      TestWorker.perform_async('test', options)

      # :queue and :message_attributes must survive intact so a reused options
      # hash routes later jobs correctly instead of falling back to the default.
      expect(options).to eq(queue: 'a_queue', message_attributes: { 'custom' => { string_value: 'v', data_type: 'String' } })
    end

    context 'when the worker has no shoryuken_options' do
      let(:worker_class) do
        stub_const(
          'NoOptionsWorker',
          Class.new do
            include Shoryuken::Worker

            def perform(sqs_msg, body); end
          end
        )
      end

      it 'uses the default worker options' do
        expect_any_instance_of(worker_class).to receive(:perform).with(kind_of(Shoryuken::InlineMessage), 'test')

        worker_class.perform_async('test')
      end

      it 'passes a batch when the default worker options enable batch' do
        allow(Shoryuken).to receive(:default_worker_options).and_return('queue' => 'default', 'batch' => true)

        expect_any_instance_of(worker_class).to receive(:perform).with([kind_of(Shoryuken::InlineMessage)], ['test'])

        worker_class.perform_async('test')
      end
    end
  end

  describe '.perform_in' do
    specify do
      expect_any_instance_of(TestWorker).to receive(:perform).with(anything, 'test')

      TestWorker.perform_in(60, 'test')
    end

    it 'properly passes message_attributes to perform_async' do
      custom_attributes = {
        'custom_key' => { string_value: 'custom_value', data_type: 'String' }
      }

      expect_any_instance_of(TestWorker).to receive(:perform) do |_, sqs_msg, _|
        expect(sqs_msg.message_attributes).to include('shoryuken_class')
        expect(sqs_msg.message_attributes).to include('custom_key')
        expect(sqs_msg.message_attributes['custom_key'][:string_value]).to eq('custom_value')
      end

      TestWorker.perform_in(60, 'test', message_attributes: custom_attributes)
    end
  end

  context 'batch' do
    before do
      TestWorker.get_shoryuken_options['batch'] = true
    end

    after do
      TestWorker.get_shoryuken_options['batch'] = false
    end

    describe '.perform_async' do
      specify do
        expect_any_instance_of(TestWorker).to receive(:perform).with(anything, ['test'])

        TestWorker.perform_async('test')
      end

      it 'properly passes message_attributes with batch' do
        custom_attributes = {
          'custom_key' => { string_value: 'custom_value', data_type: 'String' }
        }

        expect_any_instance_of(TestWorker).to receive(:perform) do |_, sqs_msgs, _|
          expect(sqs_msgs.first.message_attributes).to include('shoryuken_class')
          expect(sqs_msgs.first.message_attributes).to include('custom_key')
          expect(sqs_msgs.first.message_attributes['custom_key'][:string_value]).to eq('custom_value')
        end

        TestWorker.perform_async('test', message_attributes: custom_attributes)
      end
    end

    describe '.perform_in' do
      specify do
        expect_any_instance_of(TestWorker).to receive(:perform).with(anything, ['test'])

        TestWorker.perform_in(60, 'test')
      end

      it 'properly passes message_attributes with batch' do
        custom_attributes = {
          'custom_key' => { string_value: 'custom_value', data_type: 'String' }
        }

        expect_any_instance_of(TestWorker).to receive(:perform) do |_, sqs_msgs, _|
          expect(sqs_msgs.first.message_attributes).to include('shoryuken_class')
          expect(sqs_msgs.first.message_attributes).to include('custom_key')
          expect(sqs_msgs.first.message_attributes['custom_key'][:string_value]).to eq('custom_value')
        end

        TestWorker.perform_in(60, 'test', message_attributes: custom_attributes)
      end
    end
  end

  describe 'FIFO deduplication' do
    let(:fifo_worker) do
      stub_const(
        'InlineFifoWorker',
        Class.new do
          include Shoryuken::Worker

          shoryuken_options queue: 'orders.fifo'

          def perform(sqs_msg, body)
            $inline_fifo_executions << [sqs_msg.queue_name, body]
            "done-#{body}"
          end
        end
      )
    end

    let(:time) { [1_000.0] }
    let(:clock) { -> { time.first } }

    before do
      $inline_fifo_executions = []
      described_class.enable_fifo_deduplication!(window: 300, clock: clock)
    end

    def advance(seconds)
      time[0] += seconds
    end

    describe '.enable_fifo_deduplication!' do
      it 'is disabled by default' do
        described_class.disable_fifo_deduplication!

        expect(described_class.fifo_deduplication?).to be(false)
        expect(described_class.fifo_deduplicator).to be_nil
        expect(described_class.observed_deduplication_count).to eq(0)
      end

      it 'installs a deduplicator with the SQS five-minute window by default' do
        described_class.enable_fifo_deduplication!

        expect(described_class.fifo_deduplication?).to be(true)
        expect(described_class.fifo_deduplicator.window).to eq(
          Shoryuken::Worker::InlineExecutor::FifoDeduplicator::DEFAULT_WINDOW
        )
      end
    end

    it 'executes the first submission of a dedup scope and suppresses the repeat' do
      first = fifo_worker.perform_async('order-1', message_group_id: 'g1', message_deduplication_id: 'd1')
      second = fifo_worker.perform_async('order-1', message_group_id: 'g1', message_deduplication_id: 'd1')

      expect($inline_fifo_executions).to eq([['orders.fifo', 'order-1']])
      expect(first).to eq('done-order-1')
      expect(second).to be_a(Shoryuken::Worker::InlineExecutor::SuppressedMessage)
      expect(second.deduplicated?).to be(true)
      expect(second.queue_name).to eq('orders.fifo')
      expect(second.message_group_id).to eq('g1')
      expect(second.message_deduplication_id).to eq('d1')
      expect(second.observed_at).to be_a(Time)
      expect(described_class.observed_deduplication_count).to eq(1)
    end

    it 'counts the executed inputs through the observed scopes' do
      fifo_worker.perform_async('a', message_group_id: 'g1', message_deduplication_id: 'd1')
      fifo_worker.perform_async('a', message_group_id: 'g1', message_deduplication_id: 'd1')
      fifo_worker.perform_async('b', message_group_id: 'g1', message_deduplication_id: 'd2')

      expect($inline_fifo_executions.map(&:last)).to eq(%w[a b])
      expect(described_class.observed_deduplication_count).to eq(2)
      expect(described_class.observed_deduplication_keys).to contain_exactly(
        ['orders.fifo', 'g1', 'd1'],
        ['orders.fifo', 'g1', 'd2']
      )
    end

    it 'uses the queue actually submitted to, so an isolated queue executes again' do
      options = { message_group_id: 'g1', message_deduplication_id: 'd1' }

      fifo_worker.perform_async('order-1', options)
      same_queue = fifo_worker.perform_async('order-1', options)
      other_queue = fifo_worker.perform_async('order-1', options.merge(queue: 'other-orders.fifo'))

      expect(same_queue).to be_a(Shoryuken::Worker::InlineExecutor::SuppressedMessage)
      expect(other_queue).to eq('done-order-1')
      expect($inline_fifo_executions).to eq(
        [
          ['orders.fifo', 'order-1'],
          ['other-orders.fifo', 'order-1']
        ]
      )
      expect(described_class.observed_deduplication_keys.map(&:first)).to contain_exactly(
        'orders.fifo', 'other-orders.fifo'
      )
    end

    it 'isolates message groups, so the same deduplication id in another group executes' do
      fifo_worker.perform_async('order-1', message_group_id: 'g1', message_deduplication_id: 'd1')
      other_group = fifo_worker.perform_async(
        'order-1', message_group_id: 'g2', message_deduplication_id: 'd1'
      )

      expect(other_group).to eq('done-order-1')
      expect($inline_fifo_executions.size).to eq(2)
    end

    it 'defaults the group id to the Shoryuken FIFO default when none is given' do
      default_group = Shoryuken::Queue::MESSAGE_GROUP_ID
      fifo_worker.perform_async('order-1', message_deduplication_id: 'd1')
      repeat = fifo_worker.perform_async('order-1', message_deduplication_id: 'd1')

      expect(repeat).to be_a(Shoryuken::Worker::InlineExecutor::SuppressedMessage)
      expect(repeat.message_group_id).to eq(default_group)
      expect($inline_fifo_executions.size).to eq(1)
    end

    it 'executes again once the bounded window has elapsed' do
      fifo_worker.perform_async('order-1', message_group_id: 'g1', message_deduplication_id: 'd1')
      advance(299)
      still_suppressed = fifo_worker.perform_async(
        'order-1', message_group_id: 'g1', message_deduplication_id: 'd1'
      )
      advance(1) # reaches the 300s window
      re_executed = fifo_worker.perform_async(
        'order-1', message_group_id: 'g1', message_deduplication_id: 'd1'
      )

      expect(still_suppressed).to be_a(Shoryuken::Worker::InlineExecutor::SuppressedMessage)
      expect(re_executed).to eq('done-order-1')
      expect($inline_fifo_executions.map(&:last)).to eq(%w[order-1 order-1])
      expect(described_class.observed_deduplication_count).to eq(1)
    end

    it 'never observes submissions without a message_deduplication_id' do
      first = fifo_worker.perform_async('order-1', message_group_id: 'g1')
      second = fifo_worker.perform_async('order-1', message_group_id: 'g1')

      expect(first).to eq('done-order-1')
      expect(second).to eq('done-order-1')
      expect($inline_fifo_executions.size).to eq(2)
      expect(described_class.observed_deduplication_count).to eq(0)
    end

    it 'does not mutate the caller-supplied options hash' do
      options = {
        queue: 'orders.fifo',
        message_group_id: 'g1',
        message_deduplication_id: 'd1',
        message_attributes: { 'custom' => { string_value: 'v', data_type: 'String' } }
      }

      fifo_worker.perform_async('order-1', options)
      fifo_worker.perform_async('order-1', options)

      expect(options).to eq(
        queue: 'orders.fifo',
        message_group_id: 'g1',
        message_deduplication_id: 'd1',
        message_attributes: { 'custom' => { string_value: 'v', data_type: 'String' } }
      )
    end

    it 'suppresses repeated perform_in submissions through the same observation' do
      first = fifo_worker.perform_in(60, 'order-1', message_group_id: 'g1', message_deduplication_id: 'd1')
      second = fifo_worker.perform_in(60, 'order-1', message_group_id: 'g1', message_deduplication_id: 'd1')

      expect(first).to eq('done-order-1')
      expect(second).to be_a(Shoryuken::Worker::InlineExecutor::SuppressedMessage)
      expect($inline_fifo_executions.size).to eq(1)
    end

    describe '.reset_fifo_deduplication!' do
      it 'forgets observed scopes while keeping observation enabled' do
        fifo_worker.perform_async('order-1', message_group_id: 'g1', message_deduplication_id: 'd1')

        described_class.reset_fifo_deduplication!

        expect(described_class.fifo_deduplication?).to be(true)
        expect(described_class.observed_deduplication_count).to eq(0)
        expect(fifo_worker.perform_async(
          'order-1', message_group_id: 'g1', message_deduplication_id: 'd1'
        )).to eq('done-order-1')
      end
    end

    describe '.disable_fifo_deduplication!' do
      it 'runs every submission again, including repeats' do
        fifo_worker.perform_async('order-1', message_group_id: 'g1', message_deduplication_id: 'd1')

        described_class.disable_fifo_deduplication!
        result = fifo_worker.perform_async('order-1', message_group_id: 'g1', message_deduplication_id: 'd1')

        expect(result).to eq('done-order-1')
        expect($inline_fifo_executions.size).to eq(2)
      end
    end
  end
end
