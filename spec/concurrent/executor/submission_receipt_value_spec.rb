require 'concurrent/executor/submission_receipt'

module Concurrent
  RSpec.describe SubmissionReceipt do

    subject { described_class.new }

    it 'starts without an admission and in the :pending state' do
      expect(subject.admission).to be_nil
      expect(subject.status).to eq :pending
      expect(subject).to be_pending
      expect(subject).not_to be_terminal
      expect(subject.value).to be_nil
      expect(subject.error).to be_nil
    end

    it 'is not terminal before the admission is recorded' do
      subject.complete!(:done)
      expect(subject.status).to eq :completed
      expect(subject).not_to be_terminal
      expect(subject.wait(0)).to eq false

      subject.mark_queued!
      expect(subject).to be_terminal
      expect(subject.wait(0)).to eq true
    end

    it 'records a queued successful execution' do
      subject.mark_queued!
      subject.complete!(42)

      expect(subject.admission).to eq :queued
      expect(subject).to be_queued
      expect(subject).not_to be_caller_runs
      expect(subject).not_to be_rejected

      expect(subject.status).to eq :completed
      expect(subject).to be_completed
      expect(subject).not_to be_failed
      expect(subject.value).to eq 42
      expect(subject.error).to be_nil
      expect(subject).to be_terminal
    end

    it 'records a caller_runs failure' do
      error = StandardError.new('boom')
      subject.mark_caller_runs!
      subject.fail!(error)

      expect(subject).to be_caller_runs
      expect(subject.status).to eq :failed
      expect(subject).to be_failed
      expect(subject.error).to eq error
      expect(subject.value).to be_nil
      expect(subject).to be_terminal
    end

    it 'records a rejection with its reason' do
      error = RejectedExecutionError.new
      subject.mark_rejected!(error)

      expect(subject).to be_rejected
      expect(subject.status).to eq :rejected
      expect(subject.error).to eq error
      expect(subject).to be_terminal
    end

    it 'records a rejection without a reason for the :discard policy' do
      subject.mark_rejected!

      expect(subject).to be_rejected
      expect(subject.status).to eq :rejected
      expect(subject.error).to be_nil
    end

    it 'ignores attempts to overwrite a terminal state' do
      subject.mark_queued!
      subject.complete!(:first)
      subject.fail!(StandardError.new('later'))
      subject.complete!(:second)

      expect(subject.status).to eq :completed
      expect(subject.value).to eq :first
      expect(subject.error).to be_nil
    end

    it 'keeps the first admission' do
      subject.mark_queued!
      subject.mark_caller_runs!
      subject.mark_rejected!

      expect(subject.admission).to eq :queued
    end

    it 'wakes up a waiting thread when the execution finishes' do
      waiting = Thread.new { subject.wait(5) }
      sleep 0.05

      subject.mark_queued!
      subject.complete!(:done)

      expect(waiting.join(2)).not_to be_nil
      expect(waiting.value).to eq true
    end

    it 'returns false when the timeout expires while still pending' do
      expect(subject.wait(0.01)).to eq false
    end

    it 'lists the understood admissions and statuses' do
      expect(described_class::ADMISSIONS).to eq [:queued, :caller_runs, :rejected]
      expect(described_class::STATUSES).to eq [:pending, :completed, :failed, :rejected]
    end

    it 'renders both parts in its string form' do
      subject.mark_caller_runs!
      subject.complete!(:done)
      expect(subject.to_s).to include('caller_runs')
      expect(subject.to_s).to include('completed')
    end
  end
end
