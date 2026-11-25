# frozen_string_literal: true

# Local doubles for the acknowledgement-safe failure handoff scenario.
module HandoffSpecDoubles
  # Accepts exactly once per stable identity, but the first accept response is
  # "lost" (the call raises after storing). The result query immediately
  # following that lost accept is unavailable once, then recovers.
  class LostReplyDestination
    attr_reader :records, :accept_calls, :query_calls

    def initialize
      @records = {}
      @accept_calls = 0
      @query_calls = 0
      @reconcile_outage = false
    end

    def accept_failure(identity, record)
      @accept_calls += 1
      @records[identity.idempotency_key] ||= record
      @reconcile_outage = true
      raise Errno::ECONNRESET, 'destination accepted but the reply was lost'
    end

    def failure_result(identity)
      @query_calls += 1

      if @reconcile_outage
        @reconcile_outage = false
        raise RuntimeError, 'result query temporarily unavailable'
      end

      @records.key?(identity.idempotency_key) ? :accepted : nil
    end
  end

  class FirstQueryOutageDestination
    attr_reader :records, :accept_calls, :query_calls

    def initialize
      @records = {}
      @accept_calls = 0
      @query_calls = 0
    end

    def accept_failure(identity, record)
      @accept_calls += 1
      @records[identity.idempotency_key] ||= record
      raise Errno::ECONNRESET, 'destination accepted but the reply was lost'
    end

    def failure_result(identity)
      @query_calls += 1
      raise RuntimeError, 'first result query unavailable' if @query_calls == 1

      @records.key?(identity.idempotency_key) ? :accepted : nil
    end
  end

  class RejectingDestination
    attr_reader :accept_calls

    def initialize
      @accept_calls = 0
    end

    def failure_result(_identity)
      nil
    end

    def accept_failure(_identity, _record)
      @accept_calls += 1
      :rejected
    end
  end

  class UnknownDestination
    def failure_result(_identity)
      raise 'query permanently unavailable'
    end

    def accept_failure(_identity, _record)
      raise 'accept response lost'
    end
  end

  class SelectiveDestination
    def initialize
      @records = {}
    end

    def failure_result(identity)
      @records[identity.idempotency_key]
    end

    def accept_failure(identity, _record)
      status = identity.source_message_id.start_with?('ok') ? :accepted : :rejected
      @records[identity.idempotency_key] = status
      status
    end
  end
end

RSpec.describe Shoryuken::Middleware::Server::NonRetryableException, 'failure handoff' do
  let(:queue) { 'handoff-queue' }
  let(:sqs_queue) { double(Shoryuken::Queue) }
  let(:delete_entries) { [] }
  let(:middleware) { described_class.new }

  let(:message_struct) { Struct.new(:message_id, :receipt_handle) }
  let(:first_message) { message_struct.new('A1', 'first-receipt') }
  let(:redelivered_message) { message_struct.new('A1', 'second-receipt') }

  def worker_with(options)
    Class.new do
      define_singleton_method(:get_shoryuken_options) { options }
    end.new
  end

  def call_permanent_failure(worker, message)
    middleware.call(worker, queue, message, nil) { raise ArgumentError, 'permanent local fixture' }
  end

  before do
    Shoryuken::FailureHandoff.reset_recovery_marks!
    allow(Shoryuken::Client).to receive(:queues).with(queue).and_return(sqs_queue)
    allow(sqs_queue).to receive(:delete_messages) do |kwargs|
      delete_entries.concat(kwargs[:entries])
      false
    end
  end

  after do
    Shoryuken.failure_handoff = nil
    Shoryuken::FailureHandoff.reset_recovery_marks!
  end

  describe 'lost acceptance reply followed by a new receipt redelivery' do
    let(:destination) { HandoffSpecDoubles::LostReplyDestination.new }
    let(:options) do
      {
        'non_retryable_exceptions' => [ArgumentError],
        'failure_handoff' => destination
      }
    end
    let(:worker) { worker_with(options) }

    it 'keeps the first receipt when acceptance cannot be confirmed' do
      call_permanent_failure(worker, first_message)

      expect(delete_entries).to be_empty
    end

    it 'reuses the stable identity, finds the original acceptance and deletes only the current receipt' do
      call_permanent_failure(worker, first_message)
      call_permanent_failure(worker, redelivered_message)

      # The destination holds exactly one record and was asked to accept once.
      expect(destination.records.size).to eq(1)
      expect(destination.accept_calls).to eq(1)

      # Only the currently valid (new) receipt is acknowledged, never the first one.
      expect(delete_entries.map { |entry| entry[:receipt_handle] }).to eq(['second-receipt'])
      expect(delete_entries.none? { |entry| entry[:receipt_handle] == 'first-receipt' }).to be(true)
    end

    it 'stores a traceable failure record with the original source' do
      call_permanent_failure(worker, first_message)

      record = destination.records.fetch('handoff-queue/A1')
      expect(record.source_queue).to eq('handoff-queue')
      expect(record.source_message_id).to eq('A1')
      expect(record.receipt_handle).to eq('first-receipt')
      expect(record.exception_class).to eq('ArgumentError')
      expect(record.exception_message).to match(/permanent local fixture/)
      expect(record.identity.idempotency_key).to eq('handoff-queue/A1')
    end
  end

  describe 'when the first reconciliation query itself is unavailable' do
    let(:destination) { HandoffSpecDoubles::FirstQueryOutageDestination.new }
    let(:worker) do
      worker_with(
        'non_retryable_exceptions' => [ArgumentError],
        'failure_handoff' => destination
      )
    end

    it 'does not spend that outage on a pre-admission lookup and confirms only the new receipt' do
      call_permanent_failure(worker, first_message)

      expect(delete_entries).to be_empty
      expect(destination.accept_calls).to eq(1)
      expect(destination.query_calls).to eq(1)

      call_permanent_failure(worker, redelivered_message)

      expect(destination.records.size).to eq(1)
      expect(destination.accept_calls).to eq(1)
      expect(destination.query_calls).to eq(2)
      expect(delete_entries.map { |entry| entry[:receipt_handle] }).to eq(['second-receipt'])
    end
  end

  describe 'when SQS receive count already marks a redelivery in a fresh process' do
    let(:destination) do
      Class.new do
        attr_reader :accept_calls

        def initialize
          @accept_calls = 0
        end

        def failure_result(_identity)
          :accepted
        end

        def accept_failure(_identity, _record)
          @accept_calls += 1
          raise 'the existing result should be queried before admitting again'
        end
      end.new
    end
    let(:worker) { worker_with('non_retryable_exceptions' => [ArgumentError], 'failure_handoff' => destination) }
    let(:redelivered_message) do
      Struct.new(:message_id, :receipt_handle, :attributes).new(
        'A1',
        'second-receipt',
        { 'ApproximateReceiveCount' => '2' }
      )
    end

    it 'queries the original admission without relying on process-local marks' do
      Shoryuken::FailureHandoff.reset_recovery_marks!

      call_permanent_failure(worker, redelivered_message)

      expect(destination.accept_calls).to eq(0)
      expect(delete_entries.map { |entry| entry[:receipt_handle] }).to eq(['second-receipt'])
    end
  end

  describe 'explicit rejection' do
    let(:destination) { HandoffSpecDoubles::RejectingDestination.new }
    let(:worker) do
      worker_with(
        'non_retryable_exceptions' => [ArgumentError],
        'failure_handoff' => destination
      )
    end

    it 'does not delete the source message' do
      expect { call_permanent_failure(worker, first_message) }.not_to raise_error

      expect(destination.accept_calls).to eq(1)
      expect(delete_entries).to be_empty
    end
  end

  describe 'indeterminate handoff' do
    let(:destination) { HandoffSpecDoubles::UnknownDestination.new }
    let(:worker) do
      worker_with(
        'non_retryable_exceptions' => [ArgumentError],
        'failure_handoff' => destination
      )
    end

    it 'never deletes across repeated deliveries while the result stays unknowable' do
      call_permanent_failure(worker, first_message)
      call_permanent_failure(worker, redelivered_message)

      expect(delete_entries).to be_empty
    end
  end

  describe 'synchronous confirmed acceptance' do
    let(:destination) { Shoryuken::FailureHandoff::MemoryDestination.new }
    let(:worker) do
      worker_with(
        'non_retryable_exceptions' => [ArgumentError],
        'failure_handoff' => destination
      )
    end

    it 'deletes the current receipt immediately' do
      call_permanent_failure(worker, first_message)

      expect(destination.size).to eq(1)
      expect(delete_entries.map { |entry| entry[:receipt_handle] }).to eq(['first-receipt'])
    end
  end

  describe 'configuration entry points' do
    it 'uses the global Shoryuken.failure_handoff when the worker has no option' do
      destination = Shoryuken::FailureHandoff::MemoryDestination.new
      Shoryuken.failure_handoff = destination
      worker = worker_with('non_retryable_exceptions' => [ArgumentError])

      call_permanent_failure(worker, first_message)

      expect(destination.size).to eq(1)
      expect(delete_entries.map { |entry| entry[:receipt_handle] }).to eq(['first-receipt'])
    end

    it 'accepts a constructor-injected adapter' do
      destination = Shoryuken::FailureHandoff::MemoryDestination.new
      mw = described_class.new(destination)
      worker = worker_with('non_retryable_exceptions' => [ArgumentError])

      mw.call(worker, queue, first_message, nil) { raise ArgumentError, 'x' }

      expect(destination.size).to eq(1)
      expect(delete_entries.map { |entry| entry[:receipt_handle] }).to eq(['first-receipt'])
    end

    it 'recognizes alternative option spellings such as dead_letter_destination' do
      destination = Shoryuken::FailureHandoff::MemoryDestination.new
      worker = worker_with(
        'non_retryable_exceptions' => [ArgumentError],
        'dead_letter_destination' => destination
      )

      call_permanent_failure(worker, first_message)

      expect(destination.size).to eq(1)
    end

    it 'falls back to the legacy immediate delete when handoff is not enabled' do
      worker = worker_with('non_retryable_exceptions' => [ArgumentError])

      call_permanent_failure(worker, first_message)

      expect(delete_entries.map { |entry| entry[:receipt_handle] }).to eq(['first-receipt'])
    end

    it 'lets an explicit failure_handoff: false override a global adapter' do
      Shoryuken.failure_handoff = Shoryuken::FailureHandoff::MemoryDestination.new
      worker = worker_with(
        'non_retryable_exceptions' => [ArgumentError],
        'failure_handoff' => false
      )

      call_permanent_failure(worker, first_message)

      expect(delete_entries.map { |entry| entry[:receipt_handle] }).to eq(['first-receipt'])
      expect(Shoryuken.failure_handoff.size).to eq(0)
    end

    it 'accepts keyword constructor injection' do
      destination = Shoryuken::FailureHandoff::MemoryDestination.new
      mw = described_class.new(failure_handoff: destination)
      worker = worker_with('non_retryable_exceptions' => [ArgumentError])

      mw.call(worker, queue, first_message, nil) { raise ArgumentError, 'x' }

      expect(destination.size).to eq(1)
    end

    it 'reads a worker class-level failure_handoff reader' do
      destination = Shoryuken::FailureHandoff::MemoryDestination.new
      klass = Class.new do
        define_singleton_method(:failure_handoff) { @destination }
        define_singleton_method(:get_shoryuken_options) do
          { 'non_retryable_exceptions' => [ArgumentError] }
        end
      end
      klass.instance_variable_set(:@destination, destination)

      middleware.call(klass.new, queue, first_message, nil) { raise ArgumentError, 'x' }

      expect(destination.size).to eq(1)
    end
  end

  describe 'batch messages' do
    let(:destination) { HandoffSpecDoubles::SelectiveDestination.new }
    let(:worker) do
      worker_with(
        'non_retryable_exceptions' => [ArgumentError],
        'failure_handoff' => destination
      )
    end

    it 'deletes only the confirmed members of the batch' do
      accepted = message_struct.new('ok-1', 'receipt-ok')
      rejected = message_struct.new('bad-1', 'receipt-bad')

      middleware.call(worker, queue, [accepted, rejected], nil) { raise ArgumentError, 'batch permanent' }

      expect(delete_entries.map { |entry| entry[:receipt_handle] }).to eq(['receipt-ok'])
    end
  end
end
