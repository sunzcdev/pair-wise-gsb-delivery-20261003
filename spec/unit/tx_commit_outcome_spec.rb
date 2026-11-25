# -*- coding: utf-8 -*-
# frozen_string_literal: true
require "spec_helper"
require_relative "support/protocol_stand_in"

describe Bunny::Channel, "#tx_commit" do
  # These examples use an in-process protocol stand-in so that the failure modes
  # around tx.commit-ok can be reproduced deterministically, without a broker.

  let(:logger) { Logger.new(IO::NULL) }

  def new_connection(server, opts = {})
    Bunny.new({
      host: "127.0.0.1",
      port: server.port,
      username: "guest",
      password: "guest",
      heartbeat: 0,
      automatically_recover: false,
      network_recovery_interval: 0.05,
      continuation_timeout: 1_000,
      logger: logger
    }.merge(opts))
  end

  let(:server) { BunnyProtocolStandIn.new.start }
  let(:connection) { new_connection(server) }

  after :each do
    # a connection whose peer went away without a close handshake cannot be closed
    # gracefully, this is not what these examples are about
    Bunny::Timeout.timeout(2) { connection.close } rescue nil
    server.stop
  end

  it "commits normally when commit-ok does arrive" do
    connection.start
    ch = connection.create_channel
    ch.tx_select
    ch.default_exchange.publish("batch", routing_key: "bunny.tx.known")

    expect(ch.tx_commit).to be_a(AMQ::Protocol::Tx::CommitOk)
    expect(server.received_method_count(90, 20)).to eq 1
  end

  it "reports an unknown outcome rather than success when commit-ok never arrives" do
    connection.start
    ch = connection.create_channel
    ch.tx_select
    ch.default_exchange.publish("first batch", routing_key: "bunny.tx.unknown")

    server.withhold_next_commit_ok!

    expect { ch.tx_commit }.to raise_error(Bunny::TransactionOutcomeUnknown) do |e|
      expect(e.operation).to eq :commit
    end
  end

  it "reports an unknown outcome when the connection goes down before commit-ok arrives" do
    connection.start
    ch = connection.create_channel
    ch.tx_select
    ch.default_exchange.publish("first batch", routing_key: "bunny.tx.unknown")

    server.drop_next_commit_ok!

    expect { ch.tx_commit }.to raise_error(Bunny::TransactionOutcomeUnknown)
  end

  it "does not carry a previous successful commit over to one with an unknown outcome" do
    connection.start
    ch = connection.create_channel
    ch.tx_select

    expect(ch.tx_commit).to be_a(AMQ::Protocol::Tx::CommitOk)

    server.withhold_next_commit_ok!

    expect { ch.tx_commit }.to raise_error(Bunny::TransactionOutcomeUnknown)
  end

  it "does not treat a late commit-ok as the response to a later commit" do
    connection.start
    ch = connection.create_channel
    ch.tx_select

    # the first commit-ok arrives only after its caller has given up on it
    server.delay_next_commit_ok!(1.5)
    expect { ch.tx_commit }.to raise_error(Bunny::TransactionOutcomeUnknown)

    server.withhold_next_commit_ok!

    # the late commit-ok for the first commit arrives while this one is waiting;
    # it must not be taken for the response to this commit
    expect { ch.tx_commit }.to raise_error(Bunny::TransactionOutcomeUnknown)
  end

  context "when the server rejects the commit" do
    it "raises the channel-level error instead of an unknown outcome" do
      connection.start
      ch = connection.create_channel

      server.reject_next_commit!(reply_code: 406, reply_text: "PRECONDITION_FAILED - channel is not transactional")

      expect { ch.tx_commit }.to raise_error(Bunny::PreconditionFailed)
    end
  end

  context "after the connection has been recovered" do
    let(:connection) { new_connection(server, automatically_recover: true) }

    it "opens a new transaction and commits a new batch without replaying the previous one" do
      connection.start
      ch = connection.create_channel
      ch.tx_select
      ch.default_exchange.publish("first batch", routing_key: "bunny.tx.first")

      server.drop_next_commit_ok!

      expect { ch.tx_commit }.to raise_error(Bunny::TransactionOutcomeUnknown)

      Bunny::TestKit.poll_until(10) { connection.open? && ch.open? }
      expect(ch).to be_using_tx

      ch.default_exchange.publish("second batch", routing_key: "bunny.tx.second")
      expect { ch.tx_commit }.not_to raise_error

      expect(server.published_messages.map(&:first)).to eq %w[bunny.tx.first bunny.tx.second]
      # one commit for the unknown batch and one for the new one: the first is never resent
      expect(server.received_method_count(90, 20)).to eq 2
    end
  end
end
