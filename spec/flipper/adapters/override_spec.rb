require "flipper"
require "flipper/adapters/poll"
require "flipper/adapters/override"

RSpec.describe Flipper::Adapters::Override do
  # A controllable clock so TTL expiry can be advanced on demand instead of
  # sleeping for the real override duration.
  let(:clock) do
    Class.new do
      attr_reader :time

      def initialize
        @time = 1000.0
      end

      def now
        @time
      end

      def advance(seconds)
        @time += seconds.to_f
        self
      end
    end.new
  end

  let(:remote_adapter) { Flipper::Adapters::Memory.new(threadsafe: true) }
  let(:remote) { Flipper.new(remote_adapter) }
  let(:local_adapter) { Flipper::Adapters::Memory.new(threadsafe: true) }
  let(:poller) do
    Flipper::Poller.new(
      remote_adapter: remote_adapter,
      start_automatically: false,
      shutdown_automatically: false,
      interval: 3600
    )
  end
  let(:poll) { Flipper::Adapters::Poll.new(poller, local_adapter) }
  let(:adapter) { described_class.new(poll, clock: clock) }
  let(:flipper) { Flipper.new(adapter) }

  before do
    # Other flags start on; the target gate starts off on the remote.
    remote.enable(:analytics)
    remote.enable(:checkout)
    remote.disable(:search)
  end

  after { poller.stop }

  it "masks the target gate for the TTL while two polls land, then shows the final remote version" do
    # Baseline pulled from the remote on boot.
    expect(flipper.enabled?(:search)).to be(false)
    expect(adapter.source(:search)).to eq(:remote)

    # Admin temporarily enables the target gate for one minute.
    entry = adapter.enable_override(:search, ttl: 60, source: "admin:123")
    expect(entry.expires_at).to eq(1060.0)
    expect(entry.source).to eq("admin:123")

    # First query while the override is active: an explicitly local decision.
    expect(adapter.source(:search)).to eq(:local)
    expect(adapter.override?(:search)).to be(true)
    expect(adapter.expires_at(:search)).to eq(1060.0)
    expect(adapter.override_source(:search)).to eq("admin:123")
    expect(flipper.enabled?(:search)).to be(true)

    # Remote version 1 arrives while the override remains active.
    remote.enable(:search)
    remote.disable(:analytics)
    poller.sync

    # Query twice during the override: both reads stay a local decision even
    # though the remote just changed underneath.
    expect(flipper.enabled?(:search)).to be(true)
    expect(adapter.source(:search)).to eq(:local)
    expect(flipper.enabled?(:search)).to be(true)
    expect(adapter.source(:search)).to eq(:local)

    # Other flags keep following the remote through the first poll.
    expect(flipper.enabled?(:analytics)).to be(false)
    expect(adapter.source(:analytics)).to eq(:remote)
    expect(flipper.enabled?(:checkout)).to be(true)

    # Remote version 2: the target gate goes back off and another flag flips.
    remote.disable(:search)
    remote.disable(:checkout)
    poller.sync

    # Still inside the minute, so the local override wins regardless of polls.
    expect(flipper.enabled?(:search)).to be(true)
    expect(adapter.source(:search)).to eq(:local)
    expect(flipper.enabled?(:checkout)).to be(false)

    # Advance the fake clock past the deadline.
    clock.advance(61)

    expect(adapter.override?(:search)).to be(false)
    expect(adapter.expires_at(:search)).to be_nil
    expect(adapter.override_source(:search)).to be_nil
    expect(adapter.source(:search)).to eq(:remote)

    # The override is gone, so the just-pulled config wins.
    expect(flipper.enabled?(:search)).to be(false)
    # Other flags were always driven by the remote.
    expect(flipper.enabled?(:analytics)).to be(false)
    expect(flipper.enabled?(:checkout)).to be(false)
  end

  describe "#set_override" do
    it "records an explicit deadline and source" do
      entry = adapter.set_override(:search, ttl: 30, source: "ops")
      expect(entry).to be_a(described_class::Entry)
      expect(entry.expires_at).to eq(1030.0)
      expect(entry.source).to eq("ops")
      expect(adapter.source(:search)).to eq(:local)
    end

    it "expires exactly at the TTL boundary" do
      adapter.set_override(:search, ttl: 30)
      clock.advance(29)
      expect(adapter.override?(:search)).to be(true)
      clock.advance(2)
      expect(adapter.override?(:search)).to be(false)
      expect(adapter.source(:search)).to eq(:remote)
    end

    it "accepts an absolute expires_at" do
      adapter.set_override(:search, expires_at: 1045.0)
      clock.advance(44)
      expect(adapter.override?(:search)).to be(true)
      clock.advance(2)
      expect(adapter.override?(:search)).to be(false)
    end

    it "rejects ttl and expires_at together" do
      expect {
        adapter.set_override(:search, ttl: 10, expires_at: 2000.0)
      }.to raise_error(ArgumentError)
    end

    it "treats a missing deadline as never expiring" do
      adapter.set_override(:search, source: "manual")
      clock.advance(10_000)
      expect(adapter.override?(:search)).to be(true)
      expect(adapter.expires_at(:search)).to be_nil
    end
  end

  describe "#enable_override and #disable_override" do
    it "forces the boolean gate on while active" do
      adapter.enable_override(:search, ttl: 60, source: "admin")
      expect(flipper.enabled?(:search)).to be(true)
      clock.advance(61)
      expect(flipper.enabled?(:search)).to be(false)
    end

    it "can force a boolean off against the remote" do
      remote.enable(:search)
      poller.sync
      adapter.disable_override(:search, ttl: 60)
      expect(flipper.enabled?(:search)).to be(false)
      expect(adapter.source(:search)).to eq(:local)
      clock.advance(61)
      expect(flipper.enabled?(:search)).to be(true)
      expect(adapter.source(:search)).to eq(:remote)
    end
  end

  describe "#remove_override" do
    it "returns immediately to the remote value" do
      adapter.enable_override(:search, ttl: 60)
      expect(adapter.remove_override(:search)).to be_a(described_class::Entry)
      expect(flipper.enabled?(:search)).to be(false)
      expect(adapter.source(:search)).to eq(:remote)
      expect(adapter.remove_override(:search)).to be_nil
    end
  end

  describe "#clear_overrides" do
    it "drops every local decision" do
      adapter.enable_override(:search, ttl: 60)
      adapter.enable_override(:checkout, ttl: 60)
      expect(adapter.overrides.size).to eq(2)
      adapter.clear_overrides
      expect(adapter.overrides).to be_empty
      expect(adapter.source(:search)).to eq(:remote)
    end
  end

  it "passes normal Flipper writes through to the wrapped adapter without overriding" do
    flipper.enable(:search)
    expect(adapter.override?(:search)).to be(false)
    expect(adapter.source(:search)).to eq(:remote)
    expect(flipper.enabled?(:search)).to be(true)
  end

  it "includes locally overridden keys in features and drops them after expiry" do
    adapter.enable_override(:bonus, ttl: 60)
    expect(adapter.features.map(&:to_s)).to include("bonus")
    expect(flipper.enabled?(:bonus)).to be(true)
    clock.advance(61)
    expect(adapter.features.map(&:to_s)).not_to include("bonus")
  end

  it "serves a full gate snapshot override" do
    gates = adapter.default_config.merge(
      actors: Set["user:1"],
      percentage_of_actors: "25"
    )
    adapter.set_override(:search, gates, ttl: 60)
    stored = adapter.get(Flipper::Feature.new(:search, adapter))
    expect(stored[:actors]).to eq(Set["user:1"])
    expect(stored[:percentage_of_actors]).to eq("25")
  end

  it "mixes local and remote values in get_multi" do
    adapter.enable_override(:search, ttl: 60)
    features = [:search, :analytics, :checkout].map { |key|
      Flipper::Feature.new(key, adapter)
    }
    values = adapter.get_multi(features)
    expect(values["search"][:boolean]).to eq("true")
    expect(values["analytics"][:boolean]).to eq("true")
    expect(adapter.source(:search)).to eq(:local)
    expect(adapter.source(:analytics)).to eq(:remote)
  end

  it "get_all only replaces the overridden feature" do
    adapter.enable_override(:search, ttl: 60)
    all = adapter.get_all
    expect(all["search"][:boolean]).to eq("true")
    expect(all["analytics"][:boolean]).to eq("true")
    expect(all["checkout"][:boolean]).to eq("true")
  end

  it "returns copies of stored gate values" do
    adapter.enable_override(:search, ttl: 60)
    feature = Flipper::Feature.new(:search, adapter)
    first = adapter.get(feature)
    first[:boolean] = "tampered"
    expect(adapter.get(feature)[:boolean]).to eq("true")
  end

  it "keeps serving the latest remote for other flags during an override" do
    adapter.enable_override(:search, ttl: 60)
    remote.disable(:analytics)
    remote.enable_actor(:checkout, Flipper::Actor.new("user:42"))
    poller.sync

    expect(adapter.source(:search)).to eq(:local)
    expect(flipper.enabled?(:analytics)).to be(false)
    expect(flipper.enabled?(:checkout, Flipper::Actor.new("user:42"))).to be(true)
  end
end
