# frozen_string_literal: true

RSpec.describe Shoryuken::DeletionResult do
  let(:entries) do
    [
      { id: '0', receipt_handle: 'first' },
      { id: '1', receipt_handle: 'next' },
      { id: '2', receipt_handle: 'other' }
    ]
  end

  def failure(id, code:, sender_fault: true)
    double(id: id, code: code, message: code.downcase, sender_fault: sender_fault)
  end

  def response(successful: [], failed: [])
    double(successful: successful.map { |id| double(id: id) }, failed: failed)
  end

  describe '#record / grouping' do
    subject(:result) { described_class.new(entries).record(batch_response) }

    let(:batch_response) do
      response(
        successful: ['0'],
        failed: [
          failure('1', code: 'InternalError', sender_fault: false),
          failure('2', code: 'ReceiptHandleIsInvalid', sender_fault: true)
        ]
      )
    end

    it 'groups successful receipts' do
      expect(result.successful.map(&:receipt_handle)).to eq(['first'])
    end

    it 'classifies clearly transient failures as retryable' do
      expect(result.retryable_failures.map(&:id)).to eq(['1'])
      expect(result.retryable?).to be true
    end

    it 'classifies invalid/expired receipts as terminal' do
      expect(result.terminal_failures.map(&:id)).to eq(['2'])
    end

    it 'builds retry entries preserving the original id and receipt handle' do
      expect(result.retry_entries).to eq([{ id: '1', receipt_handle: 'next' }])
    end

    it 'exposes every receipt with its final status' do
      expect(result.receipts.map { |r| [r.receipt_handle, r.status] }).to eq(
        [
          ['first', :deleted],
          ['next', :retryable_failed],
          ['other', :terminal_failed]
        ]
      )
    end

    it 'reports the historical transient code on the receipt' do
      receipt = result.receipts.find { |r| r.id == '1' }
      expect(receipt.code).to eq('InternalError')
      expect(receipt).to be_retryable
    end

    it 'is failed but not fully successful' do
      expect(result.failed?).to be true
      expect(result.success?).to be false
    end

    it 'groups by status' do
      expect(result.group_by_status.keys).to contain_exactly(:deleted, :retryable_failed, :terminal_failed)
    end
  end

  describe 'recording a successful retry' do
    subject(:result) { described_class.new(entries) }

    before do
      result.record(
        response(
          failed: [
            failure('1', code: 'ServiceUnavailable', sender_fault: false),
            failure('2', code: 'ReceiptHandleIsInvalid')
          ]
        )
      )
      result.record(response(successful: ['1']))
    end

    it 'moves the retried receipt to deleted while keeping the invalid one terminal' do
      by_handle = result.receipts.to_h { |receipt| [receipt.receipt_handle, receipt] }

      expect(by_handle['first'].status).to eq(:pending)
      expect(by_handle['next']).to be_deleted
      expect(by_handle['next']).to be_retried
      expect(by_handle['next'].attempts).to eq(2)
      expect(by_handle['other']).to be_terminal
    end

    it 'no longer lists the resolved transient failure as retryable' do
      expect(result.retry_entries).to be_empty
      expect(result.retryable_failures).to be_empty
    end

    it 'records the retried entries and API call count' do
      expect(result.retried_entries).to eq([{ id: '1', receipt_handle: 'next' }])
      expect(result.api_calls).to eq(2)
    end

    it 'still reports the terminal receipt as failed' do
      expect(result.failed.map(&:id)).to eq(['2'])
      expect(result.failed?).to be true
    end
  end

  describe '#retryable_code?' do
    it 'accepts codes that clearly name a temporary condition' do
      %w[InternalError InternalFailure ServiceUnavailable RequestThrottled ThrottlingException
         RequestTimeout TemporaryFailure TransientError try_again_later].each do |code|
        expect(described_class.new.retryable_code?(code)).to be(true), "expected #{code} retryable"
      end
    end

    it 'rejects invalid/expired and unknown sender codes' do
      %w[ReceiptHandleIsInvalid InvalidIdFormat BatchEntryIdsNotDistinct AccessDenied code Unknown].each do |code|
        expect(described_class.new.retryable_code?(code)).to be(false), "expected #{code} terminal"
      end
    end

    it 'never retries codes that explicitly say not to retry' do
      %w[NonRetryableFailure DoNotRetry ThrottledPermanent].each do |code|
        expect(described_class.new.retryable_code?(code)).to be(false)
      end
    end
  end

  describe '#retryable_failure?' do
    subject(:result) { described_class.new }

    it 'retries an unknown code when AWS flags it as a non-sender fault' do
      expect(result.retryable_failure?('SomeBrokerBlip', false)).to be true
    end

    it 'does not retry an unknown sender fault or a missing flag' do
      expect(result.retryable_failure?('WeirdRequest', true)).to be false
      expect(result.retryable_failure?('WeirdRequest', nil)).to be false
    end

    it 'never retries an invalid receipt even if the sender_fault flag is mislabeled' do
      expect(result.retryable_failure?('ReceiptHandleIsInvalid', false)).to be false
    end

    it 'retries a clearly temporary code regardless of the flag' do
      expect(result.retryable_failure?('ServiceUnavailable', true)).to be true
    end
  end

  describe 'Hash responses' do
    it 'records symbol/string keyed hashes' do
      result = described_class.new(entries).record(
        successful: [{ id: '0' }],
        failed: [{ id: '2', code: 'ReceiptHandleIsInvalid', message: 'x', sender_fault: true }]
      )

      expect(result.successful.map(&:id)).to eq(['0'])
      expect(result.terminal_failures.map(&:id)).to eq(['2'])
    end
  end

  describe '#to_h / JSON' do
    it 'produces a JSON-safe grouped snapshot' do
      result = described_class.from_response(
        entries,
        response(
          successful: ['0'],
          failed: [failure('1', code: 'InternalError', sender_fault: false)]
        )
      )

      json = JSON.parse(JSON.generate(middleware_result: result))
      snapshot = json['middleware_result']

      expect(snapshot['receipts'].size).to eq(3)
      expect(snapshot['retry_entries']).to eq([{ 'id' => '1', 'receipt_handle' => 'next' }])
      expect(snapshot['successful'].size).to eq(1)
    end
  end

  describe '.recordable?' do
    it 'recognizes response-like objects and hashes' do
      expect(described_class.recordable?(double(failed: []))).to be true
      expect(described_class.recordable?(failed: [])).to be true
      expect(described_class.recordable?(true)).to be false
      expect(described_class.recordable?(nil)).to be false
    end
  end
end
