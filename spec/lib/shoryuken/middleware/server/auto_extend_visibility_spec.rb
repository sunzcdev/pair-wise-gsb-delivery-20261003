# frozen_string_literal: true

require 'timeout'

# We need to run our worker inside actor context.
class Runner
  def run_and_sleep(worker, queue, sqs_msg, interval)
    Shoryuken::Middleware::Server::AutoExtendVisibility.new.call(worker, queue, sqs_msg, sqs_msg.body) do
      sleep interval
    end
  end

  def run_and_raise(worker, queue, sqs_msg)
    Shoryuken::Middleware::Server::AutoExtendVisibility.new.call(worker, queue, sqs_msg, sqs_msg.body) do
      raise
    end
  end
end

RSpec.describe Shoryuken::Middleware::Server::AutoExtendVisibility do
  let(:queue) { 'default' }
  let(:visibility_timeout) { 3 }
  let(:extend_upfront) { 1 }
  let(:sqs_queue) { instance_double Shoryuken::Queue, visibility_timeout: visibility_timeout }

  def build_message
    double Shoryuken::Message, queue_url: queue, body: 'test', receipt_handle: SecureRandom.uuid,
                               settle_visibility: nil, renew_visibility: nil
  end

  let(:sqs_msg) { build_message }

  before do
    allow(Shoryuken::Client).to receive(:queues).with(queue).and_return(sqs_queue)
    stub_const('Shoryuken::Middleware::Server::AutoExtendVisibility::EXTEND_UPFRONT_SECONDS', extend_upfront)
  end

  context 'when batch worker' do
    it 'yields' do
      expect { |b| subject.call(TestWorker.new, nil, [], nil, &b) }.to yield_control
    end
  end

  it 'extends message visibility if jobs takes a long time' do
    TestWorker.get_shoryuken_options['auto_visibility_timeout'] = true

    allow(sqs_msg).to receive(:queue) { sqs_queue }
    expect(sqs_msg).to receive(:renew_visibility).with(visibility_timeout)

    Runner.new.run_and_sleep(TestWorker.new, queue, sqs_msg, visibility_timeout)
  end

  it 'settles the finished message so its terminal change is the last visibility change' do
    TestWorker.get_shoryuken_options['auto_visibility_timeout'] = true

    allow(sqs_msg).to receive(:queue) { sqs_queue }
    allow(sqs_msg).to receive(:renew_visibility)

    expect(sqs_msg).to receive(:settle_visibility).once

    Runner.new.run_and_sleep(TestWorker.new, queue, sqs_msg, visibility_timeout)
  end

  it 'does not extend message visibility if worker raises' do
    TestWorker.get_shoryuken_options['auto_visibility_timeout'] = true

    allow(sqs_msg).to receive(:queue) { sqs_queue }
    expect(sqs_msg).to_not receive(:renew_visibility)

    expect { Runner.new.run_and_raise(TestWorker.new, queue, sqs_msg) }.to raise_error(RuntimeError)
  end

  it 'does not extend message visibility if auto_visibility_timeout is not true' do
    TestWorker.get_shoryuken_options['auto_visibility_timeout'] = false

    allow(sqs_msg).to receive(:queue) { sqs_queue }
    expect(sqs_msg).to_not receive(:renew_visibility)
    expect(sqs_msg).to_not receive(:settle_visibility)

    Runner.new.run_and_sleep(TestWorker.new, queue, sqs_msg, visibility_timeout)
  end

  # The scenario the coordination exists for: a renewal request is stalled inside the SDK (its change_message_visibility
  # does not return), the job then decides to put the message back at once (visibility 0), and only then is the renewal
  # released. The put-back must be the last visibility the SQS stub records for that receipt handle.
  context 'when a renewal is stalled and the job puts the message back' do
    let(:recorded) { [] }
    let(:renewal_entered) { Queue.new }
    let(:release_renewal) { Queue.new }

    # Stands in for the SQS stub: records every visibility change per receipt handle. A renewal stalls until the spec
    # releases it, so a put-back sent while it is stalled would be recorded before it.
    let(:stub_client) do
      recorded_visibilities = recorded
      entered = renewal_entered
      release = release_renewal

      client = Object.new
      client.define_singleton_method(:change_message_visibility) do |params|
        if params[:visibility_timeout].to_i.positive?
          entered << true
          release.pop
        end
        recorded_visibilities << [params[:receipt_handle], params[:visibility_timeout]]
        nil
      end
      client
    end

    let(:queue_url) { 'https://sqs.us-east-1.amazonaws.com/123456789/coord' }
    let(:data) do
      Aws::SQS::Types::Message.new(
        message_id: 'msg-coord', receipt_handle: 'receipt-coord', body: 'job body', attributes: {}
      )
    end

    before do
      allow(Shoryuken::Client).to receive(:queues).with(queue_url).and_return(sqs_queue)
      allow(sqs_queue).to receive(:url).and_return(queue_url)
      allow(sqs_queue).to receive(:name).and_return(queue)
      allow(sqs_queue).to receive(:visibility_timeout).and_return(visibility_timeout)
      stub_const('Shoryuken::Middleware::Server::AutoExtendVisibility::EXTEND_UPFRONT_SECONDS', extend_upfront)
    end

    it 'records the put-back after the stalled renewal' do
      TestWorker.get_shoryuken_options['auto_visibility_timeout'] = true

      message = Shoryuken::Message.new(stub_client, sqs_queue, data)
      job_done = Queue.new

      result = Thread.new do
        Shoryuken::Middleware::Server::AutoExtendVisibility.new.call(
          TestWorker.new, queue, message, message.body
        ) { job_done.pop }
      end

      # Let the timer fire and stall inside change_message_visibility
      Timeout.timeout(10) { renewal_entered.pop }

      # The job finishes and immediately puts the message back. This must block behind the stalled renewal.
      job_done << true
      put_back = Thread.new { message.visibility_timeout = 0 }
      sleep 0.1
      expect(recorded).to be_empty

      release_renewal << true
      Timeout.timeout(10) { result.join }
      Timeout.timeout(10) { put_back.join }

      receipt_visibilities = recorded.select { |receipt, _| receipt == 'receipt-coord' }.map(&:last)
      expect(receipt_visibilities.last).to eq(0),
                                           'the put-back must be the last visibility recorded, got ' \
                                           "#{receipt_visibilities.inspect}"
      expect(receipt_visibilities).to include(visibility_timeout)
    end
  end

  context 'when batch worker with auto_visibility_timeout' do
    let(:batch_worker_class) do
      Class.new do
        include Shoryuken::Worker

        shoryuken_options queue: 'batch', batch: true, auto_visibility_timeout: true

        def perform(sqs_msgs, bodies); end
      end
    end

    it 'warns once per worker class and does not extend visibility for the batches' do
      warnings = []
      allow(Shoryuken.logger).to receive(:warn) { |&block| warnings << block.call }
      expect(Shoryuken::Helpers::TimerTask).not_to receive(:new)

      2.times do
        expect { |b| subject.call(batch_worker_class.new, queue, [sqs_msg], nil, &b) }.to yield_control
      end

      expect(warnings).to eq(
        ["Auto extend visibility isn't supported for batch workers, skipping it for #{batch_worker_class}"]
      )
    end

    it 'warns again for another worker class' do
      other_worker_class = Class.new(batch_worker_class)
      warnings = []
      allow(Shoryuken.logger).to receive(:warn) { |&block| warnings << block.call }

      subject.call(batch_worker_class.new, queue, [sqs_msg], nil) {}
      subject.call(other_worker_class.new, queue, [sqs_msg], nil) {}

      expect(warnings.size).to eq(2)
    end

    it 'warns once for a worker class defined again under the same name' do
      warnings = []
      allow(Shoryuken.logger).to receive(:warn) { |&block| warnings << block.call }

      %w[batch-1 batch-2].each do |batch_queue|
        stub_const(
          'BatchAutoExtendReloadedWorker',
          Class.new do
            include Shoryuken::Worker

            shoryuken_options queue: batch_queue, batch: true, auto_visibility_timeout: true

            def perform(sqs_msgs, bodies); end
          end
        )

        subject.call(BatchAutoExtendReloadedWorker.new, queue, [sqs_msg], nil) {}
      end

      expect(warnings.size).to eq(1)
    end
  end

  context 'with a receive-time visibility_timeout' do
    let(:receive_visibility_timeout) { 2 }

    before do
      TestWorker.get_shoryuken_options['auto_visibility_timeout'] = true
      allow(sqs_msg).to receive(:message_id).and_return('test-message-id')
    end

    it 'extends by the visibility_timeout set for the queue, before it expires' do
      Shoryuken.sqs_client_receive_message_opts[queue] = { visibility_timeout: receive_visibility_timeout }

      expect(sqs_msg).to receive(:renew_visibility).with(receive_visibility_timeout)

      Runner.new.run_and_sleep(TestWorker.new, queue, sqs_msg, 1.5)
    end

    it 'extends by the visibility_timeout set for the group of the queue' do
      Shoryuken.add_group('custom', 1)
      Shoryuken.add_queue(queue, 1, 'custom')
      Shoryuken.sqs_client_receive_message_opts['custom'] = { visibility_timeout: receive_visibility_timeout }

      expect(sqs_msg).to receive(:renew_visibility).with(receive_visibility_timeout)

      Runner.new.run_and_sleep(TestWorker.new, queue, sqs_msg, 1.5)
    end

    it 'uses the queue visibility timeout when the receive options do not set one' do
      Shoryuken.sqs_client_receive_message_opts[queue] = { wait_time_seconds: 1 }

      expect(sqs_msg).to receive(:renew_visibility).with(visibility_timeout)

      Runner.new.run_and_sleep(TestWorker.new, queue, sqs_msg, visibility_timeout)
    end
  end

  context 'when visibility extension fails' do
    it 'logs error when change_visibility raises an exception' do
      TestWorker.get_shoryuken_options['auto_visibility_timeout'] = true

      allow(sqs_msg).to receive(:queue) { sqs_queue }
      allow(sqs_msg).to receive(:message_id).and_return('test-message-id')
      allow(sqs_msg).to receive(:renew_visibility).and_raise(StandardError, 'AWS error')

      expect(Shoryuken.logger).to receive(:error) do |&block|
        msg = block.call
        expect(msg).to include('Could not auto extend the message')
        expect(msg).to include('test-message-id')
        expect(msg).to include('AWS error')
      end

      Runner.new.run_and_sleep(TestWorker.new, queue, sqs_msg, visibility_timeout)
    end
  end

  context 'debug logging' do
    it 'logs debug message when extending visibility' do
      TestWorker.get_shoryuken_options['auto_visibility_timeout'] = true

      allow(sqs_msg).to receive(:queue) { sqs_queue }
      allow(sqs_msg).to receive(:message_id).and_return('test-message-id')
      allow(sqs_msg).to receive(:renew_visibility)

      expect(Shoryuken.logger).to receive(:debug) do |&block|
        msg = block.call
        expect(msg).to include('Extending message')
        expect(msg).to include('test-message-id')
        expect(msg).to include("by #{visibility_timeout}s")
      end

      Runner.new.run_and_sleep(TestWorker.new, queue, sqs_msg, visibility_timeout)
    end
  end

  context 'when the visibility timeout is too short for the upfront margin' do
    # interval would be visibility_timeout - extend_upfront == 0, which used to
    # make TimerTask raise before the worker ran.
    let(:visibility_timeout) { extend_upfront }

    it 'runs the worker instead of raising' do
      TestWorker.get_shoryuken_options['auto_visibility_timeout'] = true
      allow(sqs_msg).to receive(:queue) { sqs_queue }
      allow(sqs_msg).to receive(:message_id).and_return('test-message-id')
      allow(sqs_msg).to receive(:renew_visibility)

      ran = false
      subject.call(TestWorker.new, queue, sqs_msg, sqs_msg.body) { ran = true }

      expect(ran).to be true
    end
  end

  context 'when the visibility timeout is zero' do
    let(:visibility_timeout) { 0 }

    it 'warns and runs the worker without scheduling an extension' do
      TestWorker.get_shoryuken_options['auto_visibility_timeout'] = true
      allow(sqs_msg).to receive(:queue) { sqs_queue }
      allow(sqs_msg).to receive(:message_id).and_return('test-message-id')
      expect(sqs_msg).not_to receive(:renew_visibility)
      expect(Shoryuken.logger).to receive(:warn)

      ran = false
      subject.call(TestWorker.new, queue, sqs_msg, sqs_msg.body) { ran = true }

      expect(ran).to be true
    end
  end
end
