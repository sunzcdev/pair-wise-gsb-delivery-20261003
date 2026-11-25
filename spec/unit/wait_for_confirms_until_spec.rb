# -*- coding: utf-8 -*-
# frozen_string_literal: true
require "spec_helper"

# These examples drive Bunny::Channel#wait_for_confirms_until in isolation,
# without a running RabbitMQ node: confirmations are fed to the channel through
# the same private methods the reader loop uses.

describe "Bunny::Channel#wait_for_confirms_until" do
  let(:logger) { Logger.new(IO::NULL) }

  let(:connection) do
    conn = Bunny.new(host: "127.0.0.1", port: 65_534, logger: logger)
    allow(conn).to receive(:release_channel_id)
    conn
  end

  # A channel in the state confirm_select would have left it in, with the
  # given messages published and, by default, all of them unconfirmed.
  def channel_with(published:, unconfirmed: (1..published).to_a)
    ch = Bunny::Channel.new(connection, 1)
    ch.instance_variable_set(:@status, :open)
    ch.instance_variable_set(:@next_publish_seq_no, published + 1)
    ch.instance_variable_set(:@unconfirmed_set, Set.new(unconfirmed))
    ch.instance_variable_set(:@nacked_set, Set.new)
    ch.instance_variable_set(:@only_acks_received, true)
    ch.instance_variable_set(:@confirms_continuations, Bunny::Concurrent::ContinuationQueue.new)
    ch
  end

  # Runs the block in another thread and returns whatever it raised
  # (or :no_error).
  def outcome_of
    Thread.new do
      begin
        yield
        :no_error
      rescue Exception => e
        e
      end
    end.value
  end

  describe "return value" do
    it "returns true right away when there is nothing to wait for" do
      ch = channel_with(published: 0)

      expect(ch.wait_for_confirms_until).to eq true
    end

    it "returns true once every requested message is acked" do
      ch = channel_with(published: 3)

      waiter = Thread.new { ch.wait_for_confirms_until(2, timeout: 5_000) }
      sleep 0.05
      ch.handle_ack_or_nack(2, true, false)

      expect(waiter.value).to eq true
    end

    it "returns true once every message is acked individually" do
      ch = channel_with(published: 3)

      waiter = Thread.new { ch.wait_for_confirms_until(nil, timeout: 5_000) }
      sleep 0.05
      ch.handle_ack_or_nack(1, false, false)
      ch.handle_ack_or_nack(2, false, false)
      ch.handle_ack_or_nack(3, false, false)

      expect(waiter.value).to eq true
    end
  end

  describe "messages published after the wait started" do
    it "does not extend the wait" do
      ch = channel_with(published: 2)

      waiter = Thread.new { ch.wait_for_confirms_until(nil, timeout: 5_000) }
      sleep 0.05

      # simulate a message published afterwards, which stays unconfirmed
      ch.instance_variable_get(:@unconfirmed_set_mutex).synchronize do
        ch.instance_variable_get(:@unconfirmed_set).add(3)
        ch.instance_variable_set(:@next_publish_seq_no, 4)
      end
      ch.handle_ack_or_nack(2, true, false)

      expect(waiter.value).to eq true
    end
  end

  describe "nacks" do
    it "raises MessageNacked naming the nacked sequence number" do
      ch = channel_with(published: 3)

      waiter = Thread.new do
        begin
          ch.wait_for_confirms_until(3, timeout: 5_000)
        rescue Bunny::MessageNacked => e
          e
        end
      end
      sleep 0.05
      ch.handle_ack_or_nack(1, false, true)

      expect(waiter.value).to be_a(Bunny::MessageNacked)
      expect(waiter.value.delivery_tag).to eq 1
    end

    it "raises MessageNacked without waiting when the nack already happened" do
      ch = channel_with(published: 3)
      ch.handle_ack_or_nack(2, false, true)

      expect { ch.wait_for_confirms_until(3, timeout: 1_000) }
        .to raise_error(Bunny::MessageNacked) { |e| expect(e.delivery_tag).to eq 2 }
    end

    it "does not fail a wait whose sequence number is below the nacked one" do
      ch = channel_with(published: 3, unconfirmed: [2, 3])
      ch.handle_ack_or_nack(2, false, true)

      expect(ch.wait_for_confirms_until(1, timeout: 5_000)).to eq true
    end
  end

  describe "timeouts" do
    it "raises Timeout::Error when confirmations do not arrive in time" do
      ch = channel_with(published: 2)

      expect { ch.wait_for_confirms_until(nil, timeout: 100) }
        .to raise_error(Timeout::Error)
    end

    it "does not raise when the wait ends before the timeout" do
      ch = channel_with(published: 1)

      waiter = Thread.new { ch.wait_for_confirms_until(nil, timeout: 5_000) }
      sleep 0.05
      ch.handle_ack_or_nack(1, false, false)

      expect(waiter.value).to eq true
    end
  end

  describe "the channel or its connection going away" do
    it "raises NetworkFailure when the connection is lost while waiting" do
      ch = channel_with(published: 2)

      waiter = Thread.new do
        begin
          ch.wait_for_confirms_until(nil, timeout: 5_000)
        rescue Bunny::NetworkFailure => e
          e
        end
      end
      sleep 0.05
      ch.connection_closed!

      expect(waiter.value).to be_a(Bunny::NetworkFailure)
    end

    it "raises ChannelAlreadyClosed when the channel is closed while waiting" do
      ch = channel_with(published: 2)

      waiter = Thread.new do
        begin
          ch.wait_for_confirms_until(nil, timeout: 5_000)
        rescue Bunny::ChannelAlreadyClosed => e
          e
        end
      end
      sleep 0.05
      ch.send(:closed!)

      expect(waiter.value).to be_a(Bunny::ChannelAlreadyClosed)
    end

    it "does not lose the confirmation that arrived just before the loss" do
      ch = channel_with(published: 2)

      waiter = Thread.new { ch.wait_for_confirms_until(nil, timeout: 5_000) }
      sleep 0.05
      # both are acked before the channel is closed
      ch.handle_ack_or_nack(2, true, false)
      ch.connection_closed!

      expect(waiter.value).to eq true
    end

    it "leaves messages published after a loss waitable" do
      ch = channel_with(published: 2)
      ch.connection_closed!

      # recovery reopens the channel, clears the unconfirmed set, and tags keep increasing
      ch.instance_variable_set(:@status, :open)
      ch.instance_variable_get(:@unconfirmed_set_mutex).synchronize do
        ch.instance_variable_get(:@unconfirmed_set).clear
        ch.instance_variable_get(:@unconfirmed_set).add(3)
        ch.instance_variable_get(:@unconfirmed_set).add(4)
        ch.instance_variable_set(:@next_publish_seq_no, 5)
      end

      waiter = Thread.new { ch.wait_for_confirms_until(4, timeout: 5_000) }
      sleep 0.05
      ch.handle_ack_or_nack(4, true, false)

      expect(waiter.value).to eq true
    end

    it "still reports a loss for the messages that were pending at the time" do
      ch = channel_with(published: 2)
      ch.connection_closed!
      ch.instance_variable_set(:@status, :open)

      expect { ch.wait_for_confirms_until(2, timeout: 1_000) }
        .to raise_error(Bunny::NetworkFailure)
    end
  end

  describe "sequential rounds while publishing continues" do
    # The scenario from the feature request: wait for the first two messages,
    # publish a third, have the broker ack the first two with the multiple flag
    # and then nack the third.
    it "lets the first round finish while the third message is still pending" do
      ch = channel_with(published: 2)

      first_round = Thread.new { ch.wait_for_confirms_until(2, timeout: 5_000) }
      sleep 0.05

      # a third message is published while the first round is in progress
      ch.instance_variable_get(:@unconfirmed_set_mutex).synchronize do
        ch.instance_variable_get(:@unconfirmed_set).add(3)
        ch.instance_variable_set(:@next_publish_seq_no, 4)
      end

      # a single ack covering the first two messages
      ch.handle_ack_or_nack(2, true, false)

      expect(first_round.value).to eq true

      # the third message is then nacked, which a later round observes
      ch.handle_ack_or_nack(3, false, true)

      expect { ch.wait_for_confirms_until(3, timeout: 1_000) }
        .to raise_error(Bunny::MessageNacked) { |e| expect(e.delivery_tag).to eq 3 }
    end
  end

  describe "argument validation" do
    it "rejects a sequence number that was never published" do
      ch = channel_with(published: 2)

      expect { ch.wait_for_confirms_until(5) }
        .to raise_error(ArgumentError, /only 2 message/)
    end

    it "rejects a negative sequence number" do
      ch = channel_with(published: 2)

      expect { ch.wait_for_confirms_until(-1) }.to raise_error(ArgumentError)
    end

    it "rejects a non-positive timeout" do
      ch = channel_with(published: 1)

      expect { ch.wait_for_confirms_until(nil, timeout: 0) }.to raise_error(ArgumentError)
    end

    it "rejects use on a channel without publisher confirms enabled" do
      ch = Bunny::Channel.new(connection, 1)
      ch.instance_variable_set(:@status, :open)

      expect { ch.wait_for_confirms_until(1) }
        .to raise_error(ArgumentError, /publisher confirms are not enabled/)
    end
  end
end
