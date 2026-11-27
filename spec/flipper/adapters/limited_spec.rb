require "flipper/adapters/limited"

RSpec.describe Flipper::Adapters::Limited do
  context "with every gate supported" do
    subject { described_class.new }

    it_should_behave_like "a flipper adapter"
  end

  let(:supported_gates) { %i[boolean actors groups percentage_of_actors] }
  subject(:adapter) { described_class.new(supported_gates: supported_gates) }
  let(:flipper) { Flipper.new(adapter, memoize: false) }

  describe "#supported_gates" do
    it "exposes the gates it was told it can store as a frozen set of symbols" do
      expect(adapter.supported_gates).to eq(Set[*supported_gates.map(&:to_sym)])
      expect(adapter.supported_gates).to be_frozen
    end

    it "defaults to every gate when none are given" do
      expect(described_class.new.supported_gates).to eq(
        Set[:boolean, :actors, :groups, :expression, :percentage_of_actors, :percentage_of_time]
      )
    end

    it "raises when asked to declare an unknown gate" do
      expect { described_class.new(supported_gates: [:nope]) }.to raise_error(ArgumentError, /nope/)
    end
  end

  describe "gating" do
    it "stores the gates it declares support for" do
      flipper.enable(:on)
      flipper.enable_actor(:actors, Flipper::Actor.new("1"))
      flipper.enable_group(:groups, :staff)
      flipper.enable_percentage_of_actors(:rollout, 25)

      expect(flipper[:on].boolean_value).to be(true)
      expect(flipper[:actors].actors_value).to eq(Set["1"])
      expect(flipper[:groups].groups_value).to eq(Set["staff"])
      expect(flipper[:rollout].percentage_of_actors_value).to be(25)
    end

    it "raises a clear error when an unsupported gate is enabled" do
      expect { flipper.enable_expression(:fancy, Flipper.property(:plan).eq("basic")) }
        .to raise_error(described_class::UnsupportedGateError, /expression/)
      expect { flipper.enable_percentage_of_time(:half_time, 50) }
        .to raise_error(described_class::UnsupportedGateError, /percentage_of_time/)
    end

    it "does not store the rejected gate value (only the empty feature shell remains)" do
      expect do
        flipper.enable_expression(:fancy, Flipper.property(:plan).eq("basic"))
      end.to raise_error(described_class::UnsupportedGateError)

      expect(adapter.get(flipper[:fancy])).to eq(adapter.default_config)
    end

    it "still allows disabling a supported gate after an unsupported one is rejected" do
      flipper.enable_actor(:actors, Flipper::Actor.new("1"))
      expect do
        flipper.enable_percentage_of_time(:actors, 50)
      end.to raise_error(described_class::UnsupportedGateError)

      flipper.disable_actor(:actors, Flipper::Actor.new("1"))
      expect(flipper[:actors].actors_value).to eq(Set.new)
    end

    it "supports add, remove, clear, get and get_all like an ordinary adapter" do
      flipper.add(:search)
      expect(adapter.features).to eq(Set["search"])
      expect(adapter.get(flipper[:search])).to eq(adapter.default_config)

      flipper.enable_actor(:search, Flipper::Actor.new("7"))
      expect(adapter.get_multi([flipper[:search]])).to eq(
        "search" => adapter.default_config.merge(actors: Set["7"])
      )

      flipper[:search].clear
      expect(adapter.get(flipper[:search])).to eq(adapter.default_config)

      flipper.remove(:search)
      expect(adapter.features).to eq(Set.new)
    end
  end

  describe "unknown fields" do
    it "exposes gate values produced by another adapter without losing unknown fields" do
      source = {
        "fancy" => adapter.default_config.merge(
          expression: { "Equal" => [{ "Property" => "plan" }, "basic"] },
          percentage_of_time: "50",
          custom_field: "preserved",
        ),
      }

      seeded = described_class.new(source, supported_gates: supported_gates)

      gates = seeded.get_all.fetch("fancy")
      expect(gates[:expression]).not_to be_nil
      expect(gates[:percentage_of_time]).to eq("50")
      expect(gates[:custom_field]).to eq("preserved")
    end

    it "exposes unknown fields written straight into its source hash" do
      full = Flipper::Adapters::Memory.new
      full_flipper = Flipper.new(full, memoize: false)
      full_flipper.enable_expression(:fancy, Flipper.property(:plan).eq("basic"))
      full_flipper.enable_percentage_of_time(:half_time, 50)

      seeded = described_class.new(full.get_all, supported_gates: supported_gates)

      expect(seeded.get_all.fetch("fancy")[:expression]).not_to be_nil
      expect(seeded.get_all.fetch("half_time")[:percentage_of_time]).to eq("50")
    end

    it "lets the old full-batch import surface its gating error instead of silently filtering" do
      full = Flipper::Adapters::Memory.new
      full_flipper = Flipper.new(full, memoize: false)
      full_flipper.enable_expression(:fancy, Flipper.property(:plan).eq("basic"))

      expect { adapter.import(full) }.to raise_error(described_class::UnsupportedGateError)
    end
  end
end
