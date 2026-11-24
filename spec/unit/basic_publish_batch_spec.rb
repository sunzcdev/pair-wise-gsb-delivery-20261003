# -*- coding: utf-8 -*-
# frozen_string_literal: true
require "spec_helper"

describe Bunny::Channel do
  # These examples exercise basic_publish_batch slot accounting and the
  # real write phase without a running RabbitMQ node: confirm.select and
  # frame writes are stubbed on an unconnected session. End-to-end batch
  # behavior is covered by the integration specs.

  let(:logger) { Logger.new(IO::NULL) }

  def new_session
    Bunny.new(host: "127.0.0.1", port: 65_534, logger: logger, continuation_timeout: 5000)
  end

  def new_channel
    session = new_session
    ch = Bunny::Channel.new(session, 1)
    ch.instance_variable_set(:@status, :open)
    ch.instance_variable_set(:@frame_max, 131_072)
    [ch, session]
  end

  # confirm_select waits on a continuation that the reader loop would push;
  # emulate that response asynchronously.
  def enable_confirms(ch, session, limit, confirm_timeout: 5000)
    allow(session).to receive(:send_frame) do
      Thread.new { ch.handle_method(AMQ::Protocol::Confirm::SelectOk.new) }
    end
    ch.confirm_select(tracking: true, outstanding_limit: limit, confirm_timeout: confirm_timeout)
  end

  # Records every frameset write (batch and single-message paths) and routes
  # batch writes through +handlers+, indexed by batch write number.
  def stub_writes(session, ch, handlers)
    writes = []
    batch_writes = 0
    allow(session).to receive(:send_frameset_without_timeout) do |frames, channel|
      expect(channel).to equal(ch)
      writes << frames
      handlers[batch_writes]&.call
      batch_writes += 1
    end
    allow(session).to receive(:send_frameset) do |frames, channel|
      expect(channel).to equal(ch)
      writes << frames
    end
    writes
  end

  describe "#basic_publish_batch" do
    it "assigns sequential publisher confirm numbers to every entry" do
      ch, session = new_channel
      enable_confirms(ch, session, 100)
      writes = stub_writes(session, ch, [])

      result = ch.basic_publish_batch(%w[a b c], "", "rk")

      expect(result.success?).to be true
      expect(result.size).to eq 3
      expect(result.seq_nos).to eq [1, 2, 3]
      expect(result.map(&:status)).to eq [:written, :written, :written]
      expect(ch.next_publish_seq_no).to eq 4
      expect(writes.size).to eq 3
    end

    it "writes each message in its own frameset during the real write phase" do
      ch, session = new_channel
      enable_confirms(ch, session, 100)
      writes = stub_writes(session, ch, [])

      ch.basic_publish_batch(%w[a b], "", "rk")

      expect(writes.size).to eq 2
      writes.each { |frames| expect(frames.map(&:class)).to include(AMQ::Protocol::MethodFrame) }
    end

    it "returns an empty result for an empty batch" do
      ch, _session = new_channel
      result = ch.basic_publish_batch([], "", "rk")

      expect(result).to be_a(Bunny::BatchPublishResult)
      expect(result.size).to eq 0
      expect(result.written_count).to eq 0
    end

    context "when a write raises" do
      it "keeps the entries written before the failure and marks the rest not started" do
        ch, session = new_channel
        enable_confirms(ch, session, 100)
        boom = IOError.new("simulated write failure")
        writes = stub_writes(session, ch, [nil, -> { raise boom }])

        result = ch.basic_publish_batch(%w[a b c], "", "rk")

        expect(result.success?).to be false
        expect(result.written_count).to eq 1
        expect(result[0]).to be_written
        expect(result[0].seq_no).to eq 1

        expect(result[1]).to be_failed
        expect(result[1].seq_no).to eq 2
        expect(result[1].error).to equal(boom)
        expect(result.failure).to equal(result[1])

        expect(result[2]).to be_not_started
        expect(result[2].seq_no).to be_nil
        expect(result.not_started_indexes).to eq [2]
        expect(result.not_started_payloads).to eq ["c"]

        # only the first message reached the transport
        expect(writes.size).to eq 2
        # the failed slot was rolled back, only the written seq remains unconfirmed
        expect(ch.unconfirmed_set.to_a).to eq [1]
        expect(ch.next_publish_seq_no).to eq 2
      end

      it "does not raise so the caller can inspect every entry" do
        ch, session = new_channel
        enable_confirms(ch, session, 100)
        stub_writes(session, ch, [-> { raise IOError.new("boom") }])

        result = nil
        expect {
          result = ch.basic_publish_batch(%w[a b], "", "rk")
        }.not_to raise_error
        expect(result[0]).to be_failed
        expect(result[1]).to be_not_started
      end

      it "releases the failed slot so its sequence number is reused" do
        ch, session = new_channel
        enable_confirms(ch, session, 100)
        stub_writes(session, ch, [-> { raise IOError.new("boom") }])

        result = ch.basic_publish_batch(["a"], "", "rk")
        expect(result[0]).to be_failed
        expect(ch.unconfirmed_set).to be_empty
        expect(ch.next_publish_seq_no).to eq 1

        writes2 = stub_writes(session, ch, [])
        ch.basic_publish_batch(["b"], "", "rk")
        expect(writes2.size).to eq 1
        expect(ch.next_publish_seq_no).to eq 2
      end
    end

    context "when the batch is larger than the outstanding limit" do
      it "does not block forever and continues once a confirm actually frees a slot" do
        ch, session = new_channel
        # limit of 2, batch of 3: the third entry waits for a free slot
        enable_confirms(ch, session, 2)
        writes = stub_writes(session, ch, [])

        result = nil
        thread = Thread.new { result = ch.basic_publish_batch(%w[a b c], "", "rk") }

        Bunny::TestKit.poll_until(2) { writes.size == 2 }
        expect(writes.size).to eq 2
        expect(ch.unconfirmed_set.to_a).to contain_exactly(1, 2)
        expect(ch.next_publish_seq_no).to eq 3

        # nothing frees a slot on its own, so the third entry must still be waiting
        sleep 0.1
        expect(writes.size).to eq 2

        # the broker confirms the first message, one slot becomes available
        ch.handle_ack_or_nack(1, false, false)
        Bunny::TestKit.poll_until(2) { writes.size == 3 }
        thread.join(2)

        expect(result.success?).to be true
        expect(result.written_count).to eq 3
        expect(result.seq_nos).to eq [1, 2, 3]
        expect(writes.size).to eq 3
      end

      it "lets an independent publish continue when a slot is actually free" do
        ch, session = new_channel
        # limit of 2, batch of 3 parks waiting for a slot for its third entry
        enable_confirms(ch, session, 2)
        writes = stub_writes(session, ch, [])

        batch_thread = Thread.new { ch.basic_publish_batch(%w[a b c], "", "rk") }
        Bunny::TestKit.poll_until(2) { writes.size == 2 }

        independent_thread = Thread.new { ch.basic_publish("independent", "", "rk") }
        sleep 0.1
        # both slots occupied: neither the batch's third entry nor the
        # independent publish has written
        expect(writes.size).to eq 2

        # one ack frees exactly one slot; one more write can proceed
        ch.handle_ack_or_nack(1, false, false)
        Bunny::TestKit.poll_until(2) { writes.size == 3 }
        sleep 0.1
        expect(writes.size).to eq 3

        # the second ack frees the last slot, the remaining thread proceeds
        ch.handle_ack_or_nack(2, false, false)
        Bunny::TestKit.poll_until(2) { writes.size == 4 }
        batch_thread.join(2)
        independent_thread.join(2)

        expect(writes.size).to eq 4
        expect(ch.next_publish_seq_no).to eq 5
      end

      it "times out instead of waiting forever when no confirm frees a slot" do
        ch, session = new_channel
        enable_confirms(ch, session, 2, confirm_timeout: 300)
        stub_writes(session, ch, [])

        outcome = nil
        thread = Thread.new do
          begin
            ch.basic_publish_batch(%w[a b c], "", "rk")
          rescue Timeout::Error => e
            outcome = e
          end
        end

        Bunny::TestKit.poll_until(2) { ch.unconfirmed_set.size == 2 }
        thread.join(5)

        expect(outcome).to be_a(Timeout::Error)
      end

      it "hands back the entries that never started after a failure past the limit" do
        ch, session = new_channel
        # limit of 2, batch of 3, the second write fails
        enable_confirms(ch, session, 2)
        boom = IOError.new("boom on second write")
        writes = stub_writes(session, ch, [nil, -> { raise boom }])

        result = ch.basic_publish_batch(%w[a b c], "", "rk")

        # first two entries got slots; the third never started because entry 2 failed
        expect(writes.size).to eq 2
        expect(result.written_count).to eq 1
        expect(result[1]).to be_failed
        expect(result[1].error).to equal(boom)
        expect(result[2]).to be_not_started
        expect(result.not_started_payloads).to eq ["c"]
        expect(ch.unconfirmed_set.to_a).to eq [1]
      end
    end

    context "without confirms enabled" do
      it "writes every entry without allocating sequence numbers" do
        ch, session = new_channel
        writes = stub_writes(session, ch, [])

        result = ch.basic_publish_batch(%w[a b], "", "rk")

        expect(result.success?).to be true
        expect(result.seq_nos).to eq []
        expect(result.map(&:seq_no)).to eq [nil, nil]
        expect(writes.size).to eq 2
        expect(ch.using_publisher_confirmations?).to be false
      end

      it "still reports failures and not-started entries" do
        ch, session = new_channel
        writes = stub_writes(session, ch, [-> { raise IOError.new("boom") }])

        result = ch.basic_publish_batch(%w[a b], "", "rk")

        expect(writes.size).to eq 1
        expect(result[0]).to be_failed
        expect(result[0].seq_no).to be_nil
        expect(result[1]).to be_not_started
        expect(result.not_started_payloads).to eq ["b"]
        expect(ch.using_publisher_confirmations?).to be false
      end
    end
  end

  describe Bunny::BatchPublishResult do
    let(:error) { IOError.new("boom") }

    let(:result) do
      entries = [
        Bunny::BatchPublishResult::Entry.new(0, "a", :written, seq_no: 1),
        Bunny::BatchPublishResult::Entry.new(1, "b", :failed, seq_no: 2, error: error),
        Bunny::BatchPublishResult::Entry.new(2, "c", :not_started)
      ]
      described_class.new(entries)
    end

    it "is indexable and enumerable over entries" do
      expect(result[0].payload).to eq "a"
      expect(result.map(&:status)).to eq [:written, :failed, :not_started]
      expect(result.size).to eq 3
    end

    it "reports written, failed and not-started entries" do
      expect(result.written_count).to eq 1
      expect(result.failed_count).to eq 1
      expect(result.not_started_indexes).to eq [2]
      expect(result.not_started_payloads).to eq ["c"]
      expect(result.seq_nos).to eq [1]
      expect(result.failure.error).to equal(error)
      expect(result.success?).to be false
    end

    it "rejects unknown entry statuses" do
      expect {
        Bunny::BatchPublishResult::Entry.new(0, "a", :bogus)
      }.to raise_error(ArgumentError, /unknown status/)
    end
  end
end
