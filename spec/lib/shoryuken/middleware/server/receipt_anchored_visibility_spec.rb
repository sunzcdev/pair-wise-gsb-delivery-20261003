# frozen_string_literal: true

require 'spec_helper'

# Timing contract for one receipt of a message: SQS delivers it (the visibility
# clock starts), it waits in the local dispatch queue, then runs under the
# auto-extend and exponential-backoff middlewares. Both middlewares reckon from
# the receipt time - the only clock SQS agrees on - so every visibility_timeout
# sent to the SQS stub and the final remaining wait reconcile against the same t0.
RSpec.describe 'receipt-anchored visibility timing' do
  max_visibility_timeout = Shoryuken::Middleware::Server::AutoExtendVisibility::MAX_VISIBILITY_TIMEOUT
  extend_upfront = Shoryuken::Middleware::Server::AutoExtendVisibility::EXTEND_UPFRONT_SECONDS

  let(:queue_name) { 'default' }
  let(:visibility_timeout) { 30 }
  let(:t0) { Time.at(1_700_000_000) }
  let(:queue_url) { "https://sqs.us-east-1.amazonaws.com/123456789/#{queue_name}" }
  let(:credentials) { Aws::Credentials.new('access_key_id', 'secret_access_key') }
  let(:sqs_client) { Aws::SQS::Client.new(stub_responses: true, credentials: credentials) }
  let(:sqs_queue) { Shoryuken::Queue.new(sqs_client, queue_name) }
  let(:visibility_calls) { [] }
  let(:timers) { [] }

  let(:chain) do
    Shoryuken::Middleware::Chain.new do |c|
      c.add Shoryuken::Middleware::Server::AutoExtendVisibility
      c.add Shoryuken::Middleware::Server::ExponentialBackoffRetry
    end
  end

  # t0: SQS delivers the message; the receipt's visibility starts counting here
  let(:sqs_msg) { sqs_queue.receive_messages(attribute_names: %w[All]).first }

  before do
    @now = t0
    allow(Time).to receive(:now) { @now }

    allow(sqs_client).to receive(:get_queue_url).and_return(double(queue_url: queue_url))
    sqs_client.stub_responses(:get_queue_attributes, attributes: { 'VisibilityTimeout' => visibility_timeout.to_s })
    sqs_client.stub_responses(
      :receive_message,
      messages: [{
        message_id: 'msg-1',
        receipt_handle: 'rh-1',
        body: 'test',
        attributes: { 'ApproximateReceiveCount' => '1' }
      }]
    )
    allow(sqs_client).to receive(:change_message_visibility) { |params| visibility_calls << params }

    allow(Shoryuken::Client).to receive(:queues).with(queue_name).and_return(sqs_queue)

    # Capture the extension timer instead of spawning a real thread: the fake
    # clock only moves when the spec advances it, so a real sleep would never fire
    allow(Shoryuken::Helpers::TimerTask).to receive(:new) do |execution_interval:, initial_interval: nil, &task|
      fake_timer = instance_double(Shoryuken::Helpers::TimerTask, execute: nil, kill: true)
      timers << {
        execution_interval: execution_interval,
        initial_interval: initial_interval,
        task: task,
        timer: fake_timer
      }
      fake_timer
    end

    TestWorker.get_shoryuken_options['auto_visibility_timeout'] = true

    # Receive the message at t0, before any test moves the clock
    sqs_msg
  end

  def advance_clock_by(seconds)
    @now += seconds
  end

  def advance_clock_to(time)
    @now = time
  end

  def fire_extension
    timers.first[:task].call
  end

  it 'accounts every visibility request and the final remaining wait from the same receipt time' do
    TestWorker.get_shoryuken_options['retry_intervals'] = [86_400]

    expect(sqs_msg.received_at).to eq(t0)

    # Most of the receipt's 30s visibility is spent waiting in the local dispatch
    # queue before a worker thread picks the message up.
    advance_clock_by(20)

    chain.invoke(TestWorker.new, queue_name, sqs_msg, sqs_msg.body) do
      # The first extension is anchored to what is left of the receipt (10s), not
      # to the full timeout: it fires extend_upfront before the receipt expires.
      # The steady-state interval keeps the full-timeout cadence for later fires.
      expect(timers.size).to eq(1)
      expect(timers.first[:initial_interval]).to eq(visibility_timeout - 20 - extend_upfront)
      expect(timers.first[:execution_interval]).to eq(visibility_timeout - extend_upfront)

      # t0+25: first extension fires just before the receipt would expire
      advance_clock_by(timers.first[:initial_interval])
      fire_extension
      expect(visibility_calls.last[:visibility_timeout]).to eq(30)

      # t0+50: steady-state extension, still far from the 12h ceiling
      advance_clock_by(timers.first[:execution_interval])
      fire_extension
      expect(visibility_calls.last[:visibility_timeout]).to eq(30)

      # t0+43180: an extension fires 20s before the 12h in-flight ceiling. The
      # request is capped to what SQS can still accept: 43200 - 43180 - 1 = 19.
      advance_clock_to(t0 + 43_180)
      fire_extension
      expect(visibility_calls.last[:visibility_timeout]).to eq(19)

      # t0+43190: the job fails right at the allowed boundary. Backoff asks for
      # 86400s, but only 9s remain on this receipt: 43200 - 43190 - 1.
      advance_clock_by(10)
      raise 'boom'
    end

    # Every request the SQS stub saw, in order, derived from the same t0
    expect(visibility_calls.map { |call| call[:visibility_timeout] }).to eq([30, 30, 19, 9])
    expect(visibility_calls.map { |call| call[:receipt_handle] }.uniq).to eq(%w[rh-1])
    expect(visibility_calls.map { |call| call[:queue_url] }.uniq).to eq([queue_url])

    # The final remaining wait lands the message one second before the ceiling
    failed_at = t0 + 43_190
    final_wait = visibility_calls.last[:visibility_timeout]
    expect(failed_at + final_wait).to eq(t0 + max_visibility_timeout - 1)
  end

  it 'stops the extension timer instead of computing a delay SQS cannot accept' do
    advance_clock_by(20)

    chain.invoke(TestWorker.new, queue_name, sqs_msg, sqs_msg.body) do
      # The receipt reaches the 12h in-flight ceiling while the job still runs
      advance_clock_to(t0 + max_visibility_timeout)
      fire_extension
    end

    expect(visibility_calls).to be_empty
    expect(timers.first[:timer]).to have_received(:kill).at_least(:once)
  end

  it 'does not schedule an extension when the receipt expired while queued locally' do
    warnings = []
    allow(Shoryuken.logger).to receive(:warn) { |&block| warnings << block.call }

    # The whole 30s visibility is spent in the local dispatch queue
    advance_clock_by(35)

    ran = false
    chain.invoke(TestWorker.new, queue_name, sqs_msg, sqs_msg.body) { ran = true }

    expect(ran).to be true
    expect(timers).to be_empty
    expect(visibility_calls).to be_empty
    expect(warnings.grep(/already expired/)).not_to be_empty
  end
end
