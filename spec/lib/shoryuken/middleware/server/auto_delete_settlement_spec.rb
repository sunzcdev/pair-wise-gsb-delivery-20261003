# frozen_string_literal: true

# Acceptance for partial SQS batch deletes:
# A delete_message_batch call can succeed at the HTTP level while SQS lists some receipts in
# +failed+. The framework must settle each receipt separately and redo only the clearly
# retryable delete - never re-run the already-finished worker handler. The AWS stub answers
# the three receipts with success, a transient failure and an invalid/expired receipt, then
# lets the targeted retry pass.

RSpec.describe 'AutoDelete partial batch settlement' do
  let(:credentials) { Aws::Credentials.new('access_key_id', 'secret_access_key') }
  let(:sqs) { Aws::SQS::Client.new(stub_responses: true, credentials: credentials) }
  let(:queue_name) { 'settlement-queue' }
  let(:queue_url) { "https://sqs.eu-west-1.amazonaws.com:6059/0123456789/#{queue_name}" }
  let(:queue) { Shoryuken::Queue.new(sqs, queue_name) }

  let(:message_struct) { Struct.new(:message_id, :receipt_handle) }
  let(:batch) do
    [
      message_struct.new('A1', 'first'),
      message_struct.new('A2', 'next'),
      message_struct.new('B1', 'other')
    ]
  end

  let(:first_response) do
    {
      successful: [{ id: '0' }],
      failed: [
        { id: '1', code: 'InternalError', message: 'temporarily unavailable', sender_fault: false },
        { id: '2', code: 'ReceiptHandleIsInvalid', message: 'receipt expired', sender_fault: true }
      ]
    }
  end

  before do
    allow(queue).to receive(:url).and_return(queue_url)
    allow(Shoryuken::Client).to receive(:queues).with(queue_name).and_return(queue)

    # First batch: one success, one transient failure, one dead receipt. Then the retry passes.
    sqs.stub_responses(:delete_message_batch, [first_response, { successful: [{ id: '1' }], failed: [] }])
  end

  def delete_calls
    sqs.api_requests
       .select { |request| request[:operation_name] == :delete_message_batch }
       .map { |request| request[:params][:entries] }
  end

  describe Shoryuken::Queue do
    it 'retries only the clearly transient receipt and settles every receipt' do
      result = queue.delete_messages_settlement(entries: [
                                                 { id: '0', receipt_handle: 'first' },
                                                 { id: '1', receipt_handle: 'next' },
                                                 { id: '2', receipt_handle: 'other' }
                                               ])

      expect(delete_calls.size).to eq(2)
      expect(delete_calls.first.map { |e| e[:id] }).to eq(%w[0 1 2])
      expect(delete_calls.last).to eq([{ id: '1', receipt_handle: 'next' }])

      by_handle = result.receipts.to_h { |receipt| [receipt.receipt_handle, receipt] }
      expect(by_handle['first']).to be_deleted
      expect(by_handle['next']).to be_deleted
      expect(by_handle['next']).to be_retried
      expect(by_handle['other']).to be_terminal
    end

    it 'keeps the historical boolean delete_messages contract' do
      expect(queue.delete_messages(entries: [{ id: '9', receipt_handle: 'x' }])).to be true
    end
  end

  describe Shoryuken::Middleware::Server::AutoDelete do
    let(:worker_class) do
      Class.new do
        def self.auto_delete?
          true
        end
      end
    end
    let(:worker) { worker_class.new }

    it 'runs the handler once and retries only the transient delete' do
      handler_calls = 0
      result = subject.call(worker, queue_name, batch, nil) { handler_calls += 1 }

      # The finished handler is never executed again for the delete retry.
      expect(handler_calls).to eq(1)

      expect(delete_calls.size).to eq(2)
      expect(delete_calls.last).to eq([{ id: '1', receipt_handle: 'next' }])

      # The middleware returns the grouped, inspectable final settlement.
      expect(result).to be_a(Shoryuken::DeletionResult)
      expect(result.receipts.size).to eq(3)
      by_handle = result.receipts.to_h { |receipt| [receipt.receipt_handle, receipt] }
      expect(by_handle['other']).to be_terminal
      expect(JSON.generate(middleware_result: result)).to include('terminal_failures')
    end
  end

  context 'with a queue adapter that only implements #delete_messages' do
    let(:worker) { Class.new { def self.auto_delete? = true }.new }
    let(:middleware) { Shoryuken::Middleware::Server::AutoDelete.new }
    let(:queue_api) do
      entry_struct = Struct.new(:id)
      response_struct = Struct.new(:successful, :failed)
      failure_struct = Struct.new(:id, :code, :message, :sender_fault)
      calls = []
      api = Object.new

      api.define_singleton_method(:delete_messages) do |entries:|
        calls << entries
        if calls.size == 1
          response_struct.new(
            [entry_struct.new('0')],
            [
              failure_struct.new('1', 'ServiceUnavailable', 'busy', false),
              failure_struct.new('2', 'ReceiptHandleIsInvalid', 'bad handle', true)
            ]
          )
        else
          response_struct.new(entries.map { |entry| entry_struct.new(entry[:id]) }, [])
        end
      end

      api.define_singleton_method(:calls) { calls }
      api
    end

    before do
      allow(Shoryuken::Client).to receive(:queues).with(queue_name).and_return(queue_api)
    end

    it 'drives the targeted retry through the historical seam without re-running the handler' do
      handler_calls = 0
      result = middleware.call(worker, queue_name, batch, nil) { handler_calls += 1 }

      expect(handler_calls).to eq(1)
      expect(queue_api.calls.size).to eq(2)
      expect(queue_api.calls.last).to eq([{ id: '1', receipt_handle: 'next' }])
      expect(result).to be_a(Shoryuken::DeletionResult)
      expect(result.retried_entries).to eq([{ id: '1', receipt_handle: 'next' }])
    end

    it 'returns the raw boolean result when the adapter gives no per-entry outcome' do
      plain_api = Object.new
      plain_api.define_singleton_method(:delete_messages) { |entries:| true }
      allow(Shoryuken::Client).to receive(:queues).with(queue_name).and_return(plain_api)

      expect(middleware.call(worker, queue_name, [batch.first], nil) {}).to be true
    end
  end
end
