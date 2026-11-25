require "flipper/adapters/memory"
require "flipper/adapters/operation_logger"
require "flipper/adapters/sync/feature_synchronizer"

RSpec.describe Flipper::Adapters::Sync::FeatureSynchronizer, "increments" do
  let(:adapter) do
    Flipper::Adapters::OperationLogger.new Flipper::Adapters::Memory.new
  end
  let(:flipper) { Flipper.new(adapter) }
  let(:feature) { flipper[:search] }

  before do
    feature.enable_group(:staff)
    feature.enable_percentage_of_time(25)
    feature.enable_actor(Flipper::Actor.new("1"))
    adapter.reset
  end

  it "only settles the gates the increment mentions" do
    described_class.new(feature, feature.gate_values, {actors: Set["1", "2"]}).call

    values = feature.gate_values
    expect(values.actors).to eq(Set["1", "2"])
    expect(values.groups).to eq(Set["staff"])
    expect(values.percentage_of_time).to be(25)
  end

  it "keeps the gates a boolean increment does not mention" do
    described_class.new(feature, feature.gate_values, {boolean: true}).call

    values = feature.gate_values
    expect(values.boolean).to be(true)
    expect(values.actors).to eq(Set["1"])
    expect(values.groups).to eq(Set["staff"])
    expect(values.percentage_of_time).to be(25)
  end

  it "keeps the gates a boolean increment does not mention when turning it off" do
    # An adapter can hold the boolean gate next to other gates, which is why an
    # increment that only mentions the boolean gate must not drop them.
    feature.enable
    feature.enable_group(:staff)
    feature.enable_percentage_of_time(25)
    adapter.reset

    described_class.new(feature, feature.gate_values, {boolean: false}).call

    values = feature.gate_values
    expect(values.boolean).to be(false)
    expect(values.groups).to eq(Set["staff"])
    expect(values.percentage_of_time).to be(25)
  end

  it "clears a gate it mentions with an empty value" do
    described_class.new(feature, feature.gate_values, {groups: Set.new}).call

    expect(feature.groups_value).to eq(Set.new)
    expect(feature.percentage_of_time_value).to be(25)
  end

  it "clears a gate it names under removed_gates" do
    described_class.new(
      feature,
      feature.gate_values,
      {groups: Set["staff", "other"], removed_gates: [:percentage_of_time]},
    ).call

    expect(feature.groups_value).to eq(Set["staff", "other"])
    expect(feature.percentage_of_time_value).to be(0)
  end

  it "treats a hash naming every gate as a complete mirror" do
    complete = {
      boolean: nil,
      actors: Set.new,
      groups: Set.new,
      expression: nil,
      percentage_of_actors: nil,
      percentage_of_time: nil,
    }

    described_class.new(feature, feature.gate_values, complete).call

    expect(feature.groups_value).to eq(Set.new)
    expect(feature.percentage_of_time_value).to be(0)
    expect(feature.actors_value).to eq(Set.new)
  end

  it "removes the feature when deleted" do
    described_class.new(feature, feature.gate_values, {deleted: true}).call

    expect(feature.exist?).to be(false)
  end
end