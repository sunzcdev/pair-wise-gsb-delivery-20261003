require "flipper/adapters/memory"
require "flipper/adapters/operation_logger"
require "flipper/instrumenters/memory"
require "flipper/adapters/sync/change_set"
require "flipper/adapters/sync/synchronizer"

RSpec.describe Flipper::Adapters::Sync::Synchronizer, "applying updates" do
  let(:local) { Flipper::Adapters::Memory.new }
  let(:remote) { Flipper::Adapters::Memory.new }
  let(:local_flipper) { Flipper.new(local) }
  let(:remote_flipper) { Flipper.new(remote) }
  let(:instrumenter) { Flipper::Instrumenters::Memory.new }

  subject { described_class.new(local, remote, instrumenter: instrumenter) }

  ChangeSet = Flipper::Adapters::Sync::ChangeSet
  SyncVersion = Flipper::SyncVersion

  def version(number, scope: "scope")
    SyncVersion.new(scope, number)
  end

  def feature_keys
    local.features.to_a.sort
  end

  def gate_values(key)
    local.get(Flipper::Feature.new(key, local))
  end

  describe "increments" do
    before do
      local_flipper.enable(:search)
      local_flipper[:search].enable_group(:staff)
      local_flipper[:search].enable_percentage_of_time(25)
    end

    it "leaves every value the increment does not mention as it was" do
      subject.apply(ChangeSet.incremental({"search" => {actors: Set["1"]}}))

      values = gate_values("search")
      expect(values[:actors]).to eq(Set["1"])
      expect(values[:groups]).to eq(Set["staff"])
      expect(values[:percentage_of_time]).to eq("25")
      expect(values[:boolean]).to eq("true")
    end

    it "clears only the gates it explicitly removes" do
      subject.apply(ChangeSet.incremental({"search" => {removed_gates: [:groups]}}))

      values = gate_values("search")
      expect(values[:groups]).to eq(Set[])
      expect(values[:percentage_of_time]).to eq("25")
      expect(values[:actors]).to eq(Set[])
    end

    it "clears a gate mentioned with an empty value" do
      subject.apply(ChangeSet.incremental({"search" => {groups: Set[]}}))

      expect(gate_values("search")[:groups]).to eq(Set[])
    end

    it "removes a whole feature only when it says so" do
      subject.apply(ChangeSet.incremental({"search" => {deleted: true}}))

      expect(feature_keys).to eq([])
    end

    it "does not create a feature it only clears gates for" do
      subject.apply(ChangeSet.incremental({"unknown" => {removed_gates: [:groups]}}))

      expect(feature_keys).to eq(["search"])
    end

    it "creates a feature it mentions values for" do
      subject.apply(ChangeSet.incremental({"brand_new" => {boolean: true}}))

      expect(feature_keys).to eq(%w(brand_new search))
      expect(local_flipper[:brand_new].boolean_value).to be(true)
    end

    it "keeps the boolean gate from wiping out the gates it did not mention" do
      local_flipper[:search].enable_actor(Flipper::Actor.new("7"))

      subject.apply(ChangeSet.incremental({"search" => {boolean: true}}))

      expect(local_flipper[:search].boolean_value).to be(true)
      expect(local_flipper[:search].actors_value).to eq(Set["7"])
    end

    it "keeps the untouched gates when the boolean gate is turned off" do
      local_flipper[:search].enable_actor(Flipper::Actor.new("7"))

      subject.apply(ChangeSet.incremental({"search" => {boolean: false}}))

      expect(local_flipper[:search].boolean_value).to be(false)
      expect(local_flipper[:search].actors_value).to eq(Set["7"])
      expect(local_flipper[:search].groups_value).to eq(Set["staff"])
    end
  end

  describe "complete mirrors" do
    it "still overwrites, dropping features and gates it no longer reports" do
      local_flipper.enable(:search)
      local_flipper[:search].enable_group(:staff)
      local_flipper.add(:stats)

      subject.apply(ChangeSet.full({"search" => local.default_config.merge(boolean: true)}))

      expect(feature_keys).to eq(["search"])
      expect(gate_values("search")[:groups]).to eq(Set[])
    end

    it "is what a plain sync uses" do
      remote_flipper.enable(:search)
      remote_flipper.add(:stats)
      local_flipper.enable(:stats)

      subject.call

      expect(feature_keys).to eq(%w(search stats))
      expect(local_flipper[:search].boolean_value).to be(true)
    end
  end

  describe "versions" do
    it "skips an increment older than the local version" do
      local.sync_version = version(10)

      subject.apply(ChangeSet.incremental({"search" => {boolean: true}}, version: version(9)))

      expect(local_flipper[:search].boolean_value).to be(false)
      expect(local.sync_version).to eq(version(10))

      events = instrumenter.events_by_name("synchronizer_stale.flipper")
      expect(events.size).to be(1)
      expect(events[0].payload).to eq(
        local_version: version(10),
        remote_version: version(9),
      )
    end

    it "treats a replayed increment as done" do
      local.sync_version = version(10)

      expect(local).not_to receive(:enable)
      subject.apply(ChangeSet.incremental({"search" => {boolean: true}}, version: version(10)))

      events = instrumenter.events_by_name("synchronizer_replay.flipper")
      expect(events.size).to be(1)
    end

    it "records the version of the update it settles" do
      subject.apply(ChangeSet.incremental({"search" => {boolean: true}}, version: version(11)))

      expect(local.sync_version).to eq(version(11))
    end

    it "keeps a version from another scope from being treated as older" do
      local.sync_version = version(10)
      other = version(3, scope: "other")

      subject.apply(ChangeSet.incremental({"search" => {boolean: true}}, version: other))

      expect(local_flipper[:search].boolean_value).to be(true)
      expect(local.sync_version).to eq(other)
    end
  end

  describe "the scenario from the request" do
    it "settles every change of one update together" do
      # Mirror A, B and C.
      remote_flipper.enable(:a)
      remote_flipper.enable(:b)
      remote_flipper.enable(:c)
      remote.sync_version = version(1)
      subject.call
      expect(feature_keys).to eq(%w(a b c))

      # A version that updates A and deletes B.
      remote_flipper[:a].enable_actor(Flipper::Actor.new("1"))
      remote_flipper[:b].remove
      remote.sync_version = version(2)
      subject.call

      expect(feature_keys).to eq(%w(a c))
      expect(local_flipper[:a].actors_value).to eq(Set["1"])
      expect(local_flipper[:a].boolean_value).to be(true)

      # Read once.
      expect(local_flipper.features.map(&:key).sort).to eq(%w(a c))
      expect(local_flipper[:b].exist?).to be(false)

      # A complete version that adds D.
      remote_flipper.enable(:d)
      remote.sync_version = version(3)
      subject.call
      expect(feature_keys).to eq(%w(a c d))

      # The previous version's increment arrives again. Readers must still see
      # the complete newer version.
      subject.apply(ChangeSet.incremental(
        {"a" => {actors: Set["1"]}, "b" => {boolean: true}},
        version: version(2),
      ))

      expect(feature_keys).to eq(%w(a c d))
      expect(local_flipper[:b].exist?).to be(false)
      expect(local.sync_version).to eq(version(3))
    end

    it "only shows the previous complete version until a failing update is retried" do
      local_flipper.enable(:a)
      local_flipper.enable(:c)
      local.sync_version = version(10)

      before = local.get_all

      exploding = Flipper::Adapters::OperationLogger.new(local)
      failing = described_class.new(exploding, remote, instrumenter: instrumenter)

      # The stand-in raises on the first remove of the update. Retrying that
      # same operation for the rollback succeeds.
      raised = false
      allow(exploding).to receive(:remove).and_wrap_original do |original, *args|
        unless raised
          raised = true
          raise IOError, "stubbed failure"
        end
        original.call(*args)
      end

      expect {
        failing.apply(ChangeSet.incremental(
          {"a" => {removed_gates: [:boolean]}, "c" => {deleted: true}, "d" => {boolean: true}},
          version: version(11),
        ))
      }.to raise_error(IOError)

      expect(local.get_all).to eq(before)
      expect(local.sync_version).to eq(version(10))
      expect(feature_keys).to eq(%w(a c))
      expect(local_flipper[:d].exist?).to be(false)
    end
  end

  describe "failures" do
    it "does not record the version of an update it could not finish" do
      local_flipper.enable(:search)
      local.sync_version = version(5)

      exploding = Flipper::Adapters::OperationLogger.new(local)
      failing = described_class.new(exploding, remote, instrumenter: instrumenter)
      allow(exploding).to receive(:enable).and_raise(IOError)

      expect {
        failing.apply(ChangeSet.incremental({"search" => {actors: Set["2"]}}, version: version(6)))
      }.to raise_error(IOError)

      expect(local.sync_version).to eq(version(5))
      expect(gate_values("search")[:actors]).to eq(Set[])
    end

    it "leaves the previous complete version readable with raise disabled" do
      local_flipper.enable(:search)
      local.sync_version = version(5)

      failing = described_class.new(local, remote, instrumenter: instrumenter, raise: false)
      allow(local).to receive(:get_all).and_raise(IOError)

      expect {
        failing.apply(ChangeSet.incremental({"search" => {actors: Set["2"]}}, version: version(6)))
      }.not_to raise_error

      expect(local_flipper[:search].boolean_value).to be(true)
      expect(local.sync_version).to eq(version(5))

      events = instrumenter.events_by_name("synchronizer_exception.flipper")
      expect(events.size).to be(1)
    end
  end
end