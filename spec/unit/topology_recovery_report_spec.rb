# -*- coding: utf-8 -*-
# frozen_string_literal: true
require "spec_helper"

describe Bunny::TopologyRecoveryReport do
  let(:entity) { Object.new }

  it "separates recovered entries from skipped ones" do
    report = described_class.new
    report.record_recovered(described_class::QUEUE, entity)
    report.record_skipped(described_class::EXCHANGE, entity, described_class::FILTERED_OUT)

    expect(report.recovered.size).to eq 1
    expect(report.skipped.size).to eq 1
    expect(report.recovered.first).to be_recovered
    expect(report.skipped.first).to be_skipped
  end

  it "filters entries by kind and reason" do
    report = described_class.new
    report.record_skipped(described_class::QUEUE_BINDING, entity,
                          described_class::MISSING_SOURCE_EXCHANGE)
    report.record_skipped(described_class::EXCHANGE_BINDING, entity,
                          described_class::MISSING_SOURCE_EXCHANGE)

    expect(report.skipped_of_kind(described_class::QUEUE_BINDING).size).to eq 1
    expect(report.skipped_with(described_class::QUEUE_BINDING,
                               described_class::MISSING_SOURCE_EXCHANGE).size).to eq 1
    expect(report.skipped_with(described_class::QUEUE_BINDING,
                               described_class::FILTERED_OUT)).to be_empty
  end

  it "re-classifies an existing skip entry instead of duplicating it" do
    report = described_class.new
    report.record_skipped(described_class::EXCHANGE, entity, described_class::FILTERED_OUT)
    report.replace_skip(described_class::EXCHANGE, entity,
                        described_class::EXTERNALLY_PREDECLARED)

    expect(report.entries.size).to eq 1
    expect(report.skipped.first.reason).to eq described_class::EXTERNALLY_PREDECLARED
  end

  it "adds a new entry with replace_skip when none exists" do
    report = described_class.new
    report.replace_skip(described_class::QUEUE, entity,
                        described_class::EXTERNALLY_PREDECLARED)

    expect(report.entries.size).to eq 1
    expect(report.skipped.first.reason).to eq described_class::EXTERNALLY_PREDECLARED
  end
end

describe Bunny::TopologyRegistry, "externally predeclared entities" do
  let(:ch) { nil }
  subject { described_class.new }

  it "tracks externally predeclared exchanges" do
    name = "bunny.external.exchange"
    subject.record_exchange_with(ch, name, :fanout, true, false, {})

    expect(subject.externally_predeclared_exchange?(name)).to be false
    subject.mark_exchange_as_externally_predeclared(name)
    expect(subject.externally_predeclared_exchange?(name)).to be true
  end

  it "tracks externally predeclared queues" do
    name = "bunny.external.queue"
    subject.record_queue_with(ch, name, false, true, false, false, {})

    expect(subject.externally_predeclared_queue?(name)).to be false
    subject.mark_queue_as_externally_predeclared(name)
    expect(subject.externally_predeclared_queue?(name)).to be true
  end

  it "clears externally predeclared state on reset!" do
    subject.mark_exchange_as_externally_predeclared("bunny.external.x")
    subject.mark_queue_as_externally_predeclared("bunny.external.q")

    subject.reset!

    expect(subject.externally_predeclared_exchange?("bunny.external.x")).to be false
    expect(subject.externally_predeclared_queue?("bunny.external.q")).to be false
  end
end
