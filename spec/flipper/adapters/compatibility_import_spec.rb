require "flipper/adapters/memory"
require "flipper/adapters/limited"

RSpec.describe "compatibility import" do
  let(:source_adapter) { Flipper::Adapters::Memory.new }
  let(:source) { Flipper.new(source_adapter, memoize: false) }
  let(:target_adapter) do
    Flipper::Adapters::Limited.new(supported_gates: %i[boolean actors groups percentage_of_actors])
  end
  let(:target) { Flipper.new(target_adapter, memoize: false) }

  before do
    source.enable(:plain_on)
    source.enable_actor(:actors_only, Flipper::Actor.new("1"))
    source.enable_expression(:fancy, Flipper.property(:plan).eq("basic"))
    source.enable_percentage_of_time(:half_time, 50)
  end

  describe "#import_compatibility_plan" do
    it "returns a readable plan without touching the target adapter" do
      expect(target_adapter).not_to receive(:enable)
      expect(target_adapter).not_to receive(:add)

      plan = target.import_compatibility_plan(source)

      expect(plan).to be_a(Flipper::Adapters::Sync::CompatibilityPlan)
      expect(plan.compatible_features.map(&:to_s)).to match_array(%w[plain_on actors_only])
      expect(plan.incompatible_features.map(&:to_s)).to match_array(%w[fancy half_time])
      expect(plan.unsupported_gates_for(:fancy)).to eq(Set[:expression])
      expect(plan.unsupported_gates_for(:half_time)).to eq(Set[:percentage_of_time])
      expect(plan.fully_migratable?).to be(false)
      expect(target.features).to be_empty
    end
  end

  describe "#import_compatible" do
    it "applies exactly the compatibility range and returns the plan" do
      plan = target.import_compatible(source)

      expect(plan).to be_a(Flipper::Adapters::Sync::CompatibilityPlan)
      expect(target.features.map(&:key)).to match_array(%w[plain_on actors_only])
      expect(target[:plain_on].boolean_value).to be(true)
      expect(target[:actors_only].actors_value).to eq(Set["1"])
      expect(target.exist?(:fancy)).to be(false)
      expect(target.exist?(:half_time)).to be(false)
    end

    it "keeps the target's existing settings for features outside the range" do
      target.enable_actor(:fancy, Flipper::Actor.new("keep-me"))

      target.import_compatible(source)

      expect(target[:fancy].actors_value).to eq(Set["keep-me"])
      expect(target[:fancy].expression_value).to be_nil
    end
  end

  describe "#import (full batch)" do
    it "still attempts every feature and surfaces the target's gating error" do
      expect { target.import(source) }.to raise_error(
        Flipper::Adapters::Limited::UnsupportedGateError
      )
    end

    it "migrates everything when the target can express all gates" do
      full_target = Flipper.new(Flipper::Adapters::Memory.new)

      full_target.import(source)

      expect(full_target.features.map(&:key)).to match_array(
        %w[plain_on actors_only fancy half_time]
      )
      expect(full_target[:fancy].expression_value).not_to be_nil
      expect(full_target[:half_time].percentage_of_time_value).to be(50)
    end
  end
end
