# -*- coding: utf-8 -*-
# frozen_string_literal: true
require "spec_helper"

describe Bunny::Session, "dependency-aware topology recovery" do
  # These examples exercise the recovery orchestration in isolation, without
  # a running RabbitMQ node: the protocol-level operations are stubbed out and
  # two independent channels stand in for two independent topology branches.
  let(:logger) { Logger.new(IO::NULL) }

  # A filter that removes a single exchange source. Bindings, queues and
  # consumers are left untouched so that dependency resolution decides their
  # fate: a binding follows its source exchange.
  class BranchAExchangeFilter < Bunny::TopologyRecoveryFilter
    FILTERED_EXCHANGE = "bunny.recovery.branch-a.exchange".freeze

    def filter_exchanges(xs)
      xs.reject { |rx| rx.name == FILTERED_EXCHANGE }
    end

    def filter_queues(qs)
      qs
    end

    def filter_queue_bindings(bs)
      bs
    end

    def filter_exchange_bindings(bs)
      bs
    end

    def filter_consumers(cs)
      cs
    end
  end

  def new_session(opts = {})
    Bunny.new({
      host: "127.0.0.1",
      port: 65_534,
      network_recovery_interval: 0,
      logger: logger,
      topology_recovery_filter: BranchAExchangeFilter.new
    }.merge(opts))
  end

  # Records two fully independent topology branches, each on its own channel.
  def record_two_branches(session)
    branch_a = double("branch-a channel")
    branch_b = double("branch-b channel")

    # Branch A: exchange a -> queue a, with a consumer on queue a.
    session.record_exchange_with(branch_a, "bunny.recovery.branch-a.exchange",
                                 :fanout, true, false, {})
    session.record_queue_with(branch_a, "bunny.recovery.branch-a.queue",
                              false, true, false, false, {})
    session.record_queue_binding_with(branch_a,
                                      "bunny.recovery.branch-a.exchange",
                                      "bunny.recovery.branch-a.queue",
                                      "rk.branch-a", {})
    session.record_consumer_with(branch_a, "consumer.branch-a",
                                 "bunny.recovery.branch-a.queue", proc {},
                                 true, false, {})

    # Branch B: exchange b -> queue b, with a consumer on queue b.
    session.record_exchange_with(branch_b, "bunny.recovery.branch-b.exchange",
                                 :fanout, true, false, {})
    session.record_queue_with(branch_b, "bunny.recovery.branch-b.queue",
                              false, true, false, false, {})
    session.record_queue_binding_with(branch_b,
                                      "bunny.recovery.branch-b.exchange",
                                      "bunny.recovery.branch-b.queue",
                                      "rk.branch-b", {})
    session.record_consumer_with(branch_b, "consumer.branch-b",
                                 "bunny.recovery.branch-b.queue", proc {},
                                 true, false, {})

    [branch_a, branch_b]
  end

  # Captures the protocol-level recovery commands that were actually issued.
  def stub_recovery_commands(session)
    commands = Hash.new { |h, k| h[k] = [] }
    allow(session).to receive(:recover_exchange) do |x|
      commands[:exchange] << x.name
    end
    allow(session).to receive(:recover_queue) do |q|
      commands[:queue] << q.name
    end
    allow(session).to receive(:recover_queue_binding) do |b|
      commands[:queue_binding] << [b.source, b.destination]
    end
    allow(session).to receive(:recover_exchange_binding) do |b|
      commands[:exchange_binding] << [b.source, b.destination]
    end
    allow(session).to receive(:recover_consumer) do |c|
      commands[:consumer] << c.consumer_tag
    end
    commands
  end

  describe "#recover_topology" do
    it "recovers the healthy branch and reports explainable skips for the filtered branch" do
      session = new_session
      record_two_branches(session)
      commands = stub_recovery_commands(session)

      report = session.send(:recover_topology)

      # Branch B is fully recovered: a damaged/filtered branch must not deny
      # the complete branch its recovery. The branch A queue is independent of
      # the filtered exchange, so it is still declared; only the exchange and
      # its binding are affected.
      expect(commands[:exchange]).to eq ["bunny.recovery.branch-b.exchange"]
      expect(commands[:queue]).to match_array [
        "bunny.recovery.branch-a.queue",
        "bunny.recovery.branch-b.queue"
      ]
      expect(commands[:queue_binding]).to eq [
        ["bunny.recovery.branch-b.exchange", "bunny.recovery.branch-b.queue"]
      ]
      expect(commands[:consumer]).to match_array %w[consumer.branch-a consumer.branch-b]

      # The filtered exchange is reported as filtered out, not silently lost.
      filtered_x = report.skipped_with(
        Bunny::TopologyRecoveryReport::EXCHANGE,
        Bunny::TopologyRecoveryReport::FILTERED_OUT
      )
      expect(filtered_x.map { |e| e.entity.name }).to eq ["bunny.recovery.branch-a.exchange"]

      # Its dependent binding is skipped with an explainable reason: the
      # source exchange is neither recovered nor known to exist externally.
      skipped_binding = report.skipped_with(
        Bunny::TopologyRecoveryReport::QUEUE_BINDING,
        Bunny::TopologyRecoveryReport::MISSING_SOURCE_EXCHANGE
      )
      expect(skipped_binding.map { |e| e.entity.source }).to eq ["bunny.recovery.branch-a.exchange"]

      # Branch B's entities are all reported as actually recovered.
      expect(report.recovered_of_kind(Bunny::TopologyRecoveryReport::EXCHANGE)
        .map { |e| e.entity.name }).to eq ["bunny.recovery.branch-b.exchange"]
      expect(report.recovered_of_kind(Bunny::TopologyRecoveryReport::QUEUE_BINDING)
        .map { |e| e.entity.destination }).to eq ["bunny.recovery.branch-b.queue"]
      expect(report.recovered_of_kind(Bunny::TopologyRecoveryReport::CONSUMER).size).to eq 2
    end

    it "recovers the dependent binding once the filtered source is marked as externally predeclared" do
      session = new_session
      record_two_branches(session)

      # Round 1: the branch A exchange source is filtered out, so its binding
      # cannot be recovered against a missing source.
      first_commands = stub_recovery_commands(session)
      first_report = session.send(:recover_topology)

      expect(first_commands[:queue_binding].map(&:first)).not_to include "bunny.recovery.branch-a.exchange"
      expect(first_report.recovered_of_kind(Bunny::TopologyRecoveryReport::QUEUE_BINDING).size).to eq 1

      # The exchange is now declared to still exist on the broker, owned by
      # another component.
      session.mark_exchange_as_externally_predeclared("bunny.recovery.branch-a.exchange")

      # Round 2: run the recovery again against the same recorded topology.
      second_commands = stub_recovery_commands(session)
      second_report = session.send(:recover_topology)

      # The external exchange is not declared by this connection ...
      expect(second_commands[:exchange]).to eq ["bunny.recovery.branch-b.exchange"]
      external = second_report.skipped_with(
        Bunny::TopologyRecoveryReport::EXCHANGE,
        Bunny::TopologyRecoveryReport::EXTERNALLY_PREDECLARED
      )
      expect(external.map { |e| e.entity.name }).to eq ["bunny.recovery.branch-a.exchange"]

      # ... but the binding that depends on it is now recovered, because the
      # broker-side dependency is known to be satisfied.
      expect(second_commands[:queue_binding]).to match_array [
        ["bunny.recovery.branch-b.exchange", "bunny.recovery.branch-b.queue"],
        ["bunny.recovery.branch-a.exchange", "bunny.recovery.branch-a.queue"]
      ]
      expect(second_report.recovered_of_kind(Bunny::TopologyRecoveryReport::QUEUE_BINDING).size).to eq 2
    end

    it "skips a binding with a missing-source reason when only the exchange is filtered" do
      # A filter that removes the branch A exchange source but leaves its
      # binding in place, exercising the dependency-resolution reason directly.
      filter = Class.new(Bunny::TopologyRecoveryFilter) do
        def filter_exchanges(xs)
          xs.reject { |rx| rx.name == "bunny.recovery.branch-a.exchange" }
        end
        def filter_queues(qs); qs; end
        def filter_queue_bindings(bs); bs; end
        def filter_exchange_bindings(bs); bs; end
        def filter_consumers(cs); cs; end
      end.new

      session = new_session(topology_recovery_filter: filter)
      record_two_branches(session)
      stub_recovery_commands(session)

      report = session.send(:recover_topology)

      missing_source = report.skipped_with(
        Bunny::TopologyRecoveryReport::QUEUE_BINDING,
        Bunny::TopologyRecoveryReport::MISSING_SOURCE_EXCHANGE
      )
      expect(missing_source.map { |e| e.entity.source }).to eq ["bunny.recovery.branch-a.exchange"]

      # Marking the source as externally predeclared turns the skip into a
      # successful recovery on the next pass.
      session.mark_exchange_as_externally_predeclared("bunny.recovery.branch-a.exchange")
      stub_recovery_commands(session)
      second_report = session.send(:recover_topology)

      expect(
        second_report.skipped_with(
          Bunny::TopologyRecoveryReport::QUEUE_BINDING,
          Bunny::TopologyRecoveryReport::MISSING_SOURCE_EXCHANGE
        )
      ).to be_empty
      expect(second_report.recovered_of_kind(Bunny::TopologyRecoveryReport::QUEUE_BINDING).size).to eq 2
    end

    it "isolates a failure in one branch so the other branch still recovers" do
      session = new_session
      record_two_branches(session)
      commands = stub_recovery_commands(session)

      # Branch A's exchange declaration blows up, as if that branch were
      # corrupted on the broker side.
      allow(session).to receive(:recover_exchange) do |x|
        raise "branch A exchange is broken" if x.name == "bunny.recovery.branch-a.exchange"
        commands[:exchange] << x.name
      end

      expect { session.send(:recover_topology) }.not_to raise_error

      # Branch B still recovered completely; its independent queue and
      # consumer are unaffected by the broken branch A exchange.
      expect(commands[:exchange]).to eq ["bunny.recovery.branch-b.exchange"]
      expect(commands[:queue]).to match_array [
        "bunny.recovery.branch-a.queue",
        "bunny.recovery.branch-b.queue"
      ]
      expect(commands[:consumer]).to match_array %w[consumer.branch-a consumer.branch-b]
    end

    it "scopes a single-channel recovery to that channel's branch" do
      session = new_session
      _branch_a, branch_b = record_two_branches(session)
      commands = stub_recovery_commands(session)

      # Branch B's exchange is not filtered, so its whole branch is recovered.
      report = session.recover_channel_topology(branch_b)

      expect(commands[:exchange]).to eq ["bunny.recovery.branch-b.exchange"]
      expect(commands[:queue]).to eq ["bunny.recovery.branch-b.queue"]
      expect(commands[:queue_binding]).to eq [
        ["bunny.recovery.branch-b.exchange", "bunny.recovery.branch-b.queue"]
      ]
      expect(commands[:consumer]).to eq ["consumer.branch-b"]

      # The other branch is simply not part of this recovery pass.
      recovered_names = report.recovered_of_kind(Bunny::TopologyRecoveryReport::EXCHANGE)
        .map { |e| e.entity.name }
      expect(recovered_names).to eq ["bunny.recovery.branch-b.exchange"]
    end

    it "resolves an exchange-to-exchange binding against an externally predeclared source" do
      session = new_session
      ch = double("channel")
      session.record_exchange_with(ch, "bunny.recovery.branch-a.exchange", :fanout, true, false, {})
      session.record_exchange_with(ch, "bunny.recovery.x2", :topic, true, false, {})
      session.record_exchange_binding_with(ch,
                                           "bunny.recovery.branch-a.exchange",
                                           "bunny.recovery.x2", "#", {})
      commands = stub_recovery_commands(session)

      first_report = session.send(:recover_topology)
      expect(
        first_report.skipped_with(
          Bunny::TopologyRecoveryReport::EXCHANGE_BINDING,
          Bunny::TopologyRecoveryReport::MISSING_SOURCE_EXCHANGE
        ).map { |e| e.entity.destination }
      ).to eq ["bunny.recovery.x2"]
      expect(commands[:exchange_binding]).to be_empty

      session.mark_exchange_as_externally_predeclared("bunny.recovery.branch-a.exchange")
      commands = stub_recovery_commands(session)
      second_report = session.send(:recover_topology)

      expect(commands[:exchange_binding]).to eq [
        ["bunny.recovery.branch-a.exchange", "bunny.recovery.x2"]
      ]
      expect(
        second_report.recovered_of_kind(Bunny::TopologyRecoveryReport::EXCHANGE_BINDING).size
      ).to eq 1
    end

    it "does not declare an externally predeclared queue but still binds to it" do
      session = new_session
      ch = double("channel")
      session.record_exchange_with(ch, "bunny.recovery.branch-b.exchange", :fanout, true, false, {})
      session.record_queue_with(ch, "bunny.recovery.external.queue",
                                false, true, false, false, {})
      session.record_queue_binding_with(ch,
                                        "bunny.recovery.branch-b.exchange",
                                        "bunny.recovery.external.queue", "rk", {})
      commands = stub_recovery_commands(session)

      session.mark_queue_as_externally_predeclared("bunny.recovery.external.queue")
      report = session.send(:recover_topology)

      # The external queue is not declared by this connection ...
      expect(commands[:queue]).to be_empty
      expect(
        report.skipped_with(
          Bunny::TopologyRecoveryReport::QUEUE,
          Bunny::TopologyRecoveryReport::EXTERNALLY_PREDECLARED
        ).map { |e| e.entity.name }
      ).to eq ["bunny.recovery.external.queue"]

      # ... but the binding that targets it is still recovered.
      expect(commands[:queue_binding]).to eq [
        ["bunny.recovery.branch-b.exchange", "bunny.recovery.external.queue"]
      ]
    end
  end
end
