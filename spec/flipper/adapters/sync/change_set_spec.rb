require "flipper/adapters/sync/change_set"

RSpec.describe Flipper::Adapters::Sync::ChangeSet do
  describe ".full" do
    it "is a complete mirror" do
      change_set = described_class.full({"search" => {boolean: true}})

      expect(change_set).to be_full
      expect(change_set).not_to be_incremental
      expect(change_set.keys).to eq(["search"])
      expect(change_set.deleted?("search")).to be(false)
    end
  end

  describe ".incremental" do
    it "is not a complete mirror" do
      change_set = described_class.incremental({"search" => {actors: Set["1"]}})

      expect(change_set).not_to be_full
      expect(change_set).to be_incremental
    end

    it "records explicit gate removal" do
      change_set = described_class.incremental({"search" => {removed_gates: [:actors]}})
      change = change_set.change_for("search")

      expect(change.changed_gates).to eq([:actors])
      expect(change.removed_gates).to eq([:actors])
      expect(change.gates).to eq({})
    end

    it "records explicit feature removal" do
      change_set = described_class.incremental({"search" => {deleted: true}})

      expect(change_set.deleted?("search")).to be(true)
      expect(change_set.keys).to eq(["search"])
    end

    it "accepts a bare hash of gate values" do
      change_set = described_class.incremental({"search" => {actors: Set["1"]}})
      change = change_set.change_for("search")

      expect(change.deleted?).to be(false)
      expect(change.gates).to eq({actors: Set["1"]})
      expect(change.changed_gates).to eq([:actors])
    end

    it "accepts gates wrapped under :gates" do
      change_set = described_class.incremental({"search" => {gates: {boolean: true}}})
      change = change_set.change_for("search")

      expect(change.gates).to eq({boolean: true})
    end

    it "treats a feature mentioned with no values as untouched" do
      change_set = described_class.incremental({"search" => nil})
      change = change_set.change_for("search")

      expect(change.changed_gates).to eq([])
      expect(change.deleted?).to be(false)
    end
  end

  describe ".from" do
    it "returns the change set it is given" do
      change_set = described_class.incremental({})
      expect(described_class.from(change_set)).to be(change_set)
    end

    it "wraps a bare feature hash as an increment" do
      change_set = described_class.from({"search" => {boolean: true}})

      expect(change_set).to be_incremental
      expect(change_set.keys).to eq(["search"])
    end

    it "reads a version from the hash" do
      change_set = described_class.from({"search" => {boolean: true}, version: 4})
      expect(change_set.raw_version).to eq(4)
    end

    it "reads explicit deletion from the hash" do
      change_set = described_class.from({features: {"search" => {boolean: true}}, deleted: ["stats"]})

      expect(change_set.deleted?("stats")).to be(true)
      expect(change_set.keys).to contain_exactly("search", "stats")
    end

    it "honors :full" do
      change_set = described_class.from({"search" => {boolean: true}}, full: true)
      expect(change_set).to be_full
    end

    it "honors :deleted" do
      change_set = described_class.from({}, deleted: ["search"])
      expect(change_set.deleted?("search")).to be(true)
    end

    it "honors :version" do
      change_set = described_class.from({"search" => {boolean: true}}, version: Flipper::SyncVersion.new("s", 3))
      expect(change_set.raw_version).to eq(Flipper::SyncVersion.new("s", 3))
    end
  end

  describe "#sync_version" do
    let(:local_version) { Flipper::SyncVersion.new("scope", 1) }

    it "passes a SyncVersion through" do
      version = Flipper::SyncVersion.new("other", 2)
      change_set = described_class.full({}, version: version)

      expect(change_set.sync_version(local_version)).to eq(version)
    end

    it "interprets an Integer in the local scope" do
      change_set = described_class.full({}, version: 7)

      expect(change_set.sync_version(local_version)).to eq(Flipper::SyncVersion.new("scope", 7))
    end

    it "parses a scope:number String" do
      change_set = described_class.full({}, version: "other:9")

      expect(change_set.sync_version(local_version)).to eq(Flipper::SyncVersion.new("other", 9))
    end

    it "is nil without a version" do
      expect(described_class.full({}).sync_version(local_version)).to be(nil)
    end
  end

  describe Flipper::Adapters::Sync::ChangeSet::FeatureChange do
    it "merges into the given local values" do
      local = {
        boolean: "true",
        actors: Set["1"],
        groups: Set["staff"],
        expression: nil,
        percentage_of_actors: "10",
        percentage_of_time: nil,
      }
      change = described_class.build({actors: Set["2"], removed_gates: [:groups]})

      merged = change.merged_gate_values(local)

      expect(merged[:boolean]).to eq("true")
      expect(merged[:actors]).to eq(Set["2"])
      expect(merged[:groups]).to be(nil)
      expect(merged[:percentage_of_actors]).to eq("10")
    end

    it "does not mutate the local values it was given" do
      local = {actors: Set["1"]}
      described_class.build({actors: Set["2"]}).merged_gate_values(local)

      expect(local).to eq({actors: Set["1"]})
    end
  end
end