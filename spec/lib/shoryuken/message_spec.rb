# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Shoryuken::Message do
  let(:client) { instance_double('Aws::SQS::Client') }
  let(:queue) { instance_double('Shoryuken::Queue', name: 'test-queue', url: 'https://sqs.us-east-1.amazonaws.com/123456789/test-queue') }
  let(:data) do
    instance_double(
      'Aws::SQS::Types::Message',
      message_id: 'msg-123',
      receipt_handle: 'handle-456',
      md5_of_body: 'abcd1234',
      body: '{"test": "data"}',
      attributes: { 'ApproximateReceiveCount' => '1' },
      md5_of_message_attributes: 'efgh5678',
      message_attributes: { 'type' => 'test' }
    )
  end

  subject { described_class.new(client, queue, data) }

  describe '#initialize' do
    it 'sets client, queue_url, queue_name, and data' do
      expect(subject.client).to eq(client)
      expect(subject.queue_url).to eq('https://sqs.us-east-1.amazonaws.com/123456789/test-queue')
      expect(subject.queue_name).to eq('test-queue')
      expect(subject.data).to eq(data)
    end
  end

  describe 'delegated methods' do
    it 'delegates message_id to data' do
      expect(subject.message_id).to eq('msg-123')
    end

    it 'delegates receipt_handle to data' do
      expect(subject.receipt_handle).to eq('handle-456')
    end

    it 'delegates md5_of_body to data' do
      expect(subject.md5_of_body).to eq('abcd1234')
    end

    it 'delegates body to data' do
      expect(subject.body).to eq('{"test": "data"}')
    end

    it 'delegates attributes to data' do
      expect(subject.attributes).to eq({ 'ApproximateReceiveCount' => '1' })
    end

    it 'delegates md5_of_message_attributes to data' do
      expect(subject.md5_of_message_attributes).to eq('efgh5678')
    end

    it 'delegates message_attributes to data' do
      expect(subject.message_attributes).to eq({ 'type' => 'test' })
    end
  end

  describe '#delete' do
    it 'calls delete_message on the client with correct parameters' do
      expect(client).to receive(:delete_message).with(
        queue_url: 'https://sqs.us-east-1.amazonaws.com/123456789/test-queue',
        receipt_handle: 'handle-456'
      )

      subject.delete
    end
  end

  describe '#change_visibility' do
    it 'calls change_message_visibility on the client with merged parameters' do
      options = { visibility_timeout: 300 }

      expect(client).to receive(:change_message_visibility).with(
        hash_including(
          visibility_timeout: 300,
          queue_url: 'https://sqs.us-east-1.amazonaws.com/123456789/test-queue',
          receipt_handle: 'handle-456'
        )
      )

      subject.change_visibility(options)
    end

    it 'merges queue_url and receipt_handle into provided options' do
      options = { visibility_timeout: 120, custom_param: 'value' }

      expect(client).to receive(:change_message_visibility).with(
        hash_including(
          visibility_timeout: 120,
          custom_param: 'value',
          queue_url: 'https://sqs.us-east-1.amazonaws.com/123456789/test-queue',
          receipt_handle: 'handle-456'
        )
      )

      subject.change_visibility(options)
    end
  end

  describe '#visibility_timeout=' do
    it 'calls change_message_visibility on the client with the timeout' do
      expect(client).to receive(:change_message_visibility).with(
        queue_url: 'https://sqs.us-east-1.amazonaws.com/123456789/test-queue',
        receipt_handle: 'handle-456',
        visibility_timeout: 600
      )

      subject.visibility_timeout = 600
    end
  end

  # The visibility of a message is either renewed while its job runs or settled for good when the job's terminal
  # handling deletes or releases it. These pin the ordering that keeps a stalled renewal from undoing that terminal
  # change: a renewal in flight finishes first, and no renewal is sent after the message is settled.
  describe 'visibility renewal and terminal changes' do
    describe '#renew_visibility' do
      it 'changes the visibility timeout of the message' do
        expect(client).to receive(:change_message_visibility).with(
          hash_including(
            queue_url: 'https://sqs.us-east-1.amazonaws.com/123456789/test-queue',
            receipt_handle: 'handle-456',
            visibility_timeout: 30
          )
        )

        subject.renew_visibility(30)
      end

      it 'is skipped once the message is settled' do
        subject.settle_visibility

        expect(client).not_to receive(:change_message_visibility)

        expect(subject.renew_visibility(30)).to be_nil
      end

      it 'is skipped once the message is released' do
        allow(client).to receive(:change_message_visibility)

        subject.visibility_timeout = 0

        expect(client).to have_received(:change_message_visibility).once
        expect(subject.renew_visibility(30)).to be_nil
        expect(client).to have_received(:change_message_visibility).once
      end

      it 'is still sent after a positive visibility change' do
        allow(client).to receive(:change_message_visibility)

        subject.change_visibility(visibility_timeout: 120)
        subject.renew_visibility(30)

        expect(client).to have_received(:change_message_visibility).twice
      end

      it 'is skipped once the message is deleted' do
        allow(client).to receive(:change_message_visibility)
        allow(client).to receive(:delete_message)

        subject.delete

        expect(subject.renew_visibility(30)).to be_nil
        expect(client).not_to have_received(:change_message_visibility)
      end
    end

    describe '#settle_visibility' do
      it 'waits for a renewal already in flight, so the terminal change follows it' do
        renewal_started = Queue.new
        finish_renewal = Queue.new
        order = []

        allow(client).to receive(:change_message_visibility) do
          renewal_started << true
          finish_renewal.pop
          order << :renewal
        end

        renewal = Thread.new { subject.renew_visibility(30) }
        renewal_started.pop

        settle = Thread.new do
          subject.settle_visibility
          order << :settled
        end

        # The settle must block until the renewal in flight returns
        sleep 0.05
        expect(order).to be_empty

        finish_renewal << true
        renewal.join
        settle.join

        expect(order).to eq(%i[renewal settled])
      end

      it 'lets other messages keep renewing' do
        other = described_class.new(client, queue, data)

        allow(client).to receive(:change_message_visibility)

        subject.settle_visibility
        other.renew_visibility(30)
        other.renew_visibility(30)

        expect(client).to have_received(:change_message_visibility).twice
      end
    end

    describe '#delete' do
      it 'is skipped for renewal once it ran' do
        allow(client).to receive(:change_message_visibility)
        allow(client).to receive(:delete_message)

        subject.delete

        expect(subject.renew_visibility(30)).to be_nil
        expect(client).not_to have_received(:change_message_visibility)
      end

      it 'waits for a renewal in flight before deleting' do
        renewal_started = Queue.new
        finish_renewal = Queue.new
        order = []

        allow(client).to receive(:change_message_visibility) do
          renewal_started << true
          finish_renewal.pop
          order << :renewal
        end
        allow(client).to receive(:delete_message) { order << :delete }

        renewal = Thread.new { subject.renew_visibility(30) }
        renewal_started.pop

        delete = Thread.new { subject.delete }

        sleep 0.05
        expect(order).to be_empty

        finish_renewal << true
        renewal.join
        delete.join

        expect(order).to eq(%i[renewal delete])
      end
    end
  end

  describe '#last_receive?' do
    let(:attributes) { { 'ApproximateReceiveCount' => receive_count } }
    let(:data) { instance_double('Aws::SQS::Types::Message', attributes: attributes) }
    let(:receive_count) { '3' }

    before do
      allow(Shoryuken::Client).to receive(:queues).with(queue.url).and_return(queue)
      allow(queue).to receive(:max_receive_count).and_return(max_receive_count)
    end

    context 'when the receive count is below maxReceiveCount' do
      let(:max_receive_count) { 4 }

      it { expect(subject.last_receive?).to be(false) }
    end

    context 'when the receive count equals maxReceiveCount' do
      let(:max_receive_count) { 3 }

      it { expect(subject.last_receive?).to be(true) }
    end

    context 'when the receive count is above maxReceiveCount' do
      let(:max_receive_count) { 2 }

      it { expect(subject.last_receive?).to be(true) }
    end

    context 'when the queue has no redrive policy' do
      let(:max_receive_count) { nil }

      it { expect(subject.last_receive?).to be(false) }
    end

    context 'when the message has no ApproximateReceiveCount' do
      let(:attributes) { {} }
      let(:max_receive_count) { 1 }

      it { expect(subject.last_receive?).to be(false) }
    end

    context 'when the message has no attributes' do
      let(:attributes) { nil }
      let(:max_receive_count) { 1 }

      it { expect(subject.last_receive?).to be(false) }
    end
  end
end
