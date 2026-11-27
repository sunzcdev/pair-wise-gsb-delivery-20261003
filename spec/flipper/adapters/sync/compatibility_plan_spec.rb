require "flipper/adapters/memory"
require "flipper/adapters/limited"
require "flipper/adapters/sync/compatibility_plan"

RSpec.describe Flipper::Adapters::Sync::CompatibilityPlan do
  let(:source_adapter) { Flipper::Adapters::Memory.new }
  let(:source) { Flipper.new(source_adapter, memoize: false) }
  let(:target_adapter) do
    Flipper::Adapters::Limited.new(supported_gates: %i[boolean actors groups percentage_of_actors])
  end
  let(:target) { Flipper.new(target_adapter, memoize: false) }

  subject { described_class.new(target_adapter, source_adapter) }

  describe "#compatible_features" do
    it "lists features whose enabled gates can all be represented by the target" do
      source.enable(:plain_on)
      source.enable_actor(:actors_only, Flipper::Actor.new("1"))
      source.enable_group(:groups_only, :staff)
      source.enable_percentage_of_actors(:rollout, 25)

      expect(subject.compatible_features.map(&:to_s)).to match_array(
        %w[plain_on actors_only groups_only rollout]
      )
    end

    it "treats a feature with no enabled gates as fully expressible" do
      source.add(:empty_feature)

      expect(subject.compatible_features.map(&:to_s)).to eq(["empty_feature"])
      expect(subject.incompatible_features).to be_empty
    end

    it "does not list features that need a gate the target cannot store" do
      source.enable_expression(:fancy, Flipper.property(:plan).eq("basic"))
      source.enable_percentage_of_time(:half_time, 50)

      expect(subject.compatible_features).to be_empty
    end
  end

  describe "#incompatible_features" do
    it "lists features with the unsupported gates they require" do
      source.enable_expression(:fancy, Flipper.property(:plan).eq("basic"))
      source.enable_percentage_of_time(:half_time, 50)
      source.enable_actor(:mixed, Flipper::Actor.new("1"))
      source.enable_percentage_of_time(:mixed, 25)

      expect(subject.incompatible_features.map(&:to_s)).to match_array(
        %w[fancy half_time mixed]
      )
      expect(subject.unsupported_gates_for(:fancy)).to eq(Set[:expression])
      expect(subject.unsupported_gates_for(:half_time)).to eq(Set[:percentage_of_time])
      expect(subject.unsupported_gates_for(:mixed)).to eq(Set[:percentage_of_time])
    end

    it "reports multiple unsupported gates for one feature" do
      source.enable_expression(:both, Flipper.property(:plan).eq("basic"))
      source.enable_percentage_of_time(:both, 50)

      expect(subject.unsupported_gates_for(:both)).to eq(
        Set[:expression, :percentage_of_time]
      )
    end
  end

  describe "#fully_migratable?" do
    it "is true when every source feature is expressible by the target" do
      source.enable(:plain_on)
      source.enable_actor(:actors_only, Flipper::Actor.new("1"))

      expect(subject.fully_migratable?).to be(true)
    end

    it "is false when a source feature needs an unsupported gate" do
      source.enable_expression(:fancy, Flipper.property(:plan).eq("basic"))

      expect(subject.fully_migratable?).to be(false)
    end
  end

  describe "#apply" do
    it "writes only the compatible features, in full, to the target" do
      source.enable(:plain_on)
      source.enable_actor(:actors_only, Flipper::Actor.new("1"))
      source.enable_group(:groups_only, :staff)
      source.enable_percentage_of_actors(:rollout, 25)
      source.enable_expression(:fancy, Flipper.property(:plan).eq("basic"))
      source.enable_percentage_of_time(:half_time, 50)

      result = subject.apply

      expect(result).to equal(subject)
      expect(target.features.map(&:key)).to match_array(
        %w[plain_on actors_only groups_only rollout]
      )
      expect(target[:plain_on].boolean_value).to be(true)
      expect(target[:actors_only].actors_value).to eq(Set["1"])
      expect(target[:groups_only].groups_value).to eq(Set["staff"])
      expect(target[:rollout].percentage_of_actors_value).to be(25)
      expect(target.exist?(:fancy)).to be(false)
      expect(target.exist?(:half_time)).to be(false)
    end

    it "skips a feature that also has unsupported gates, even its expressible gates" do
      source.enable_actor(:mixed, Flipper::Actor.new("1"))
      source.enable_group(:mixed, :staff)
      source.enable_expression(:mixed, Flipper.property(:plan).eq("basic"))

      subject.apply

      expect(target.exist?(:mixed)).to be(false)
    end

    it "adds compatible features the target did not know about" do
      source.add(:brand_new)

      subject.apply

      expect(target.exist?(:brand_new)).to be(true)
      expect(target[:brand_new].gate_values).to eq(
        Flipper::GateValues.new(target_adapter.default_config)
      )
    end

    it "preserves the target's existing settings for features it cannot express" do
      target.enable_actor(:fancy, Flipper::Actor.new("keep-me"))
      target.enable_group(:fancy, :existing)
      source.enable_expression(:fancy, Flipper.property(:plan).eq("basic"))

      subject.apply

      expect(target[:fancy].actors_value).to eq(Set["keep-me"])
      expect(target[:fancy].groups_value).to eq(Set["existing"])
      expect(target[:fancy].expression_value).to be_nil
    end

    it "leaves unrelated target features untouched" do
      target.enable(:local_only)
      source.enable(:from_source)

      subject.apply

      expect(target[:local_only].boolean_value).to be(true)
      expect(target[:from_source].boolean_value).to be(true)
    end

    it "leaves target-only features for the full-batch import to remove" do
      target.enable(:gone)
      source.add(:staying)

      subject.apply

      expect(target.exist?(:gone)).to be(true)
      expect(target[:gone].boolean_value).to be(true)
      expect(target.exist?(:staying)).to be(true)

      target.import(source)

      expect(target.exist?(:gone)).to be(false)
      expect(target.exist?(:staying)).to be(true)
    end

    it "never removes a target feature whose source flags are not fully expressible" do
      target.enable(:fancy)
      source.enable_expression(:fancy, Flipper.property(:plan).eq("basic"))

      subject.apply

      expect(target[:fancy].boolean_value).to be(true)
    end

    it "updates compatible features on a second run to match the source" do
      source.enable_actor(:actors_only, Flipper::Actor.new("1"))
      subject.apply

      source.disable_actor(:actors_only, Flipper::Actor.new("1"))
      source.enable_actor(:actors_only, Flipper::Actor.new("2"))
      described_class.new(target_adapter, source_adapter).apply

      expect(target[:actors_only].actors_value).to eq(Set["2"])
    end
  end

  describe "precheck writes" do
    it "does not write anything while building the plan" do
      source.enable(:plain_on)
      source.enable_expression(:fancy, Flipper.property(:plan).eq("basic"))

      expect(target_adapter).not_to receive(:enable)
      expect(target_adapter).not_to receive(:disable)
      expect(target_adapter).not_to receive(:add)
      expect(target_adapter).not_to receive(:remove)
      expect(target_adapter).not_to receive(:clear)
      expect(target_adapter).not_to receive(:import)

      subject.compatible_features
      subject.incompatible_features
      subject.fully_migratable?

      expect(target.features).to be_empty
    end
  end

  describe "source normalization" do
    it "accepts a DSL, an adapter or an export" do
      source.enable(:plain_on)
      export = source_adapter.export

      plan_from_dsl = described_class.new(target_adapter, source)
      plan_from_adapter = described_class.new(target_adapter, source_adapter)
      plan_from_export = described_class.new(target_adapter, export)

      expect(plan_from_dsl.compatible_features).to eq(plan_from_adapter.compatible_features)
      expect(plan_from_export.compatible_features.map(&:to_s)).to eq(["plain_on"])
    end
  end

  describe "targets without gate capability declarations" do
    let(:plain_target) { Flipper::Adapters::Memory.new }

    it "assumes every gate is expressible" do
      source.enable_expression(:fancy, Flipper.property(:plan).eq("basic"))

      plan = described_class.new(plain_target, source_adapter)

      expect(plan.fully_migratable?).to be(true)
      plan.apply
      expect(Flipper.new(plain_target, memoize: false)[:fancy].expression_value).not_to be_nil
    end
  end
end
