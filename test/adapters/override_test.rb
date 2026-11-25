require "test_helper"
require "flipper/adapters/poll"
require "flipper/adapters/override"

# A controllable clock so override expiry can be advanced on demand instead of
# sleeping for the real TTL.
class FakeClock
  def initialize(time = 1000.0)
    @time = time.to_f
  end

  attr_reader :time

  def now
    @time
  end

  def advance(seconds)
    @time += seconds.to_f
    self
  end
end

class OverrideAdapterTest < MiniTest::Test
  def setup
    @clock = FakeClock.new(1000.0)

    @remote_adapter = Flipper::Adapters::Memory.new(threadsafe: true)
    @remote = Flipper.new(@remote_adapter)
    # Other flags start on; the target gate starts off on the remote.
    @remote.enable(:analytics)
    @remote.enable(:checkout)
    @remote.disable(:search)

    @local_adapter = Flipper::Adapters::Memory.new(threadsafe: true)
    @poller = Flipper::Poller.new(
      remote_adapter: @remote_adapter,
      start_automatically: false,
      shutdown_automatically: false,
      interval: 3600
    )
    @poll = Flipper::Adapters::Poll.new(@poller, @local_adapter)
    @adapter = Flipper::Adapters::Override.new(@poll, clock: @clock)
    @flipper = Flipper.new(@adapter)
  end

  def teardown
    @poller.stop
  end

  # The full admin scenario: a one-minute local override while Poll keeps
  # pulling two new remote versions, then the override expires and the target
  # gate shows the final remote version while other flags track the remote.
  def test_timed_override_over_two_polls_then_returns_to_final_remote
    # Baseline pulled from the remote on boot.
    assert_equal false, @flipper.enabled?(:search)
    assert_equal true, @flipper.enabled?(:analytics)
    assert_equal :remote, @adapter.source(:search)

    # Admin temporarily enables the target gate for one minute.
    entry = @adapter.enable_override(:search, ttl: 60, source: "admin:123")
    assert_equal 1060.0, entry.expires_at
    assert_equal "admin:123", entry.source

    # First read while the override is active.
    assert_equal :local, @adapter.source(:search)
    assert_equal true, @flipper.enabled?(:search)
    assert_equal "admin:123", @adapter.override_source(:search)
    assert_equal 1060.0, @adapter.expires_at(:search)
    assert @adapter.override?(:search)

    # Remote version 1 arrives via poll while the override is still active.
    @remote.enable(:search)
    @remote.disable(:analytics)
    @poller.sync
    # Query twice during the override: both reads stay a local decision.
    assert_equal true, @flipper.enabled?(:search)
    assert_equal :local, @adapter.source(:search)
    assert_equal true, @flipper.enabled?(:search)
    assert_equal :local, @adapter.source(:search)
    # Other flags keep following the remote through the first poll.
    assert_equal false, @flipper.enabled?(:analytics)
    assert_equal :remote, @adapter.source(:analytics)
    assert_equal true, @flipper.enabled?(:checkout)

    # Remote version 2: target gate goes back off, another flag flips.
    @remote.disable(:search)
    @remote.disable(:checkout)
    @poller.sync
    # Still within the minute, so the local override keeps winning regardless
    # of the two polls.
    assert_equal true, @flipper.enabled?(:search)
    assert_equal :local, @adapter.source(:search)
    assert_equal false, @flipper.enabled?(:checkout)

    # Advance the fake clock past the deadline, then read again.
    @clock.advance(61)
    refute @adapter.override?(:search)
    assert_nil @adapter.expires_at(:search)
    assert_nil @adapter.override_source(:search)
    assert_equal :remote, @adapter.source(:search)
    # The just-pulled config wins: the target shows the final remote version.
    assert_equal false, @flipper.enabled?(:search)
    # Other flags remain driven entirely by the remote.
    assert_equal false, @flipper.enabled?(:analytics)
    assert_equal false, @flipper.enabled?(:checkout)
  end

  def test_override_deadline_is_stored_and_reported
    entry = @adapter.set_override(:search, ttl: 30, source: "ops")
    assert_in_delta 1030.0, entry.expires_at, 0.001
    assert_equal "ops", @adapter.override_source(:search)
    assert_equal :local, @adapter.source(:search)

    @clock.advance(29)
    assert @adapter.override?(:search)
    assert_equal :local, @adapter.source(:search)

    @clock.advance(2)
    refute @adapter.override?(:search)
    assert_equal :remote, @adapter.source(:search)
  end

  def test_override_without_ttl_never_expires
    @adapter.enable_override(:search, source: "manual")
    @clock.advance(10_000)
    assert @adapter.override?(:search)
    assert_nil @adapter.expires_at(:search)
    assert_equal "manual", @adapter.override_source(:search)
  end

  def test_absolute_expires_at
    @adapter.set_override(:search, expires_at: 1045.0)
    @clock.advance(44)
    assert @adapter.override?(:search)
    @clock.advance(2)
    refute @adapter.override?(:search)
  end

  def test_ttl_and_expires_at_are_mutually_exclusive
    assert_raises(ArgumentError) do
      @adapter.set_override(:search, ttl: 10, expires_at: 2000.0)
    end
  end

  def test_disable_override_forces_boolean_off
    @remote.enable(:search)
    @poller.sync
    @adapter.disable_override(:search, ttl: 60)
    assert_equal false, @flipper.enabled?(:search)
    assert_equal :local, @adapter.source(:search)

    @clock.advance(61)
    assert_equal true, @flipper.enabled?(:search)
    assert_equal :remote, @adapter.source(:search)
  end

  def test_remove_override_returns_immediately_to_remote
    @adapter.enable_override(:search, ttl: 60)
    assert_equal true, @flipper.enabled?(:search)

    removed = @adapter.remove_override(:search)
    assert_instance_of Flipper::Adapters::Override::Entry, removed
    assert_equal false, @flipper.enabled?(:search)
    assert_equal :remote, @adapter.source(:search)
    assert_nil @adapter.remove_override(:search)
  end

  def test_clear_overrides_drops_every_local_decision
    @adapter.enable_override(:search, ttl: 60)
    @adapter.enable_override(:checkout, ttl: 60)
    assert_equal 2, @adapter.overrides.size

    @adapter.clear_overrides
    assert_empty @adapter.overrides
    assert_equal :remote, @adapter.source(:search)
    assert_equal :remote, @adapter.source(:checkout)
  end

  def test_normal_writes_pass_through_to_wrapped_adapter
    # A write through the Flipper API does not create an override.
    @flipper.enable(:search)
    assert_equal :remote, @adapter.source(:search)
    refute @adapter.override?(:search)
    assert_equal true, @flipper.enabled?(:search)
  end

  def test_features_includes_locally_overridden_keys
    # :bonus exists only inside the override, never on the remote.
    @adapter.enable_override(:bonus, ttl: 60)
    assert_includes @adapter.features.map(&:to_s), "bonus"
    assert_equal true, @flipper.enabled?(:bonus)

    @clock.advance(61)
    refute_includes @adapter.features.map(&:to_s), "bonus"
  end

  def test_full_gate_snapshot_override
    gates = @adapter.default_config.merge(
      boolean: nil,
      actors: Set["user:1"],
      percentage_of_actors: "25"
    )
    @adapter.set_override(:search, gates, ttl: 60)
    stored = @adapter.get(Flipper::Feature.new(:search, @adapter))
    assert_equal Set["user:1"], stored[:actors]
    assert_equal "25", stored[:percentage_of_actors]
    assert_nil stored[:boolean]
  end

  def test_get_multi_mixes_local_and_remote
    @adapter.enable_override(:search, ttl: 60)
    features = [:search, :analytics, :checkout].map { |key|
      Flipper::Feature.new(key, @adapter)
    }
    values = @adapter.get_multi(features)
    assert_equal "true", values["search"][:boolean]
    assert_equal "true", values["analytics"][:boolean]
    assert_equal "true", values["checkout"][:boolean]
    assert_equal :local, @adapter.source(:search)
    assert_equal :remote, @adapter.source(:analytics)
  end

  def test_get_all_overrides_only_the_targeted_feature
    @adapter.enable_override(:search, ttl: 60)
    all = @adapter.get_all
    # The target gate is masked by the local override.
    assert_equal "true", all["search"][:boolean]
    # Other features keep their untouched remote values (analytics was enabled
    # remotely in setup).
    assert_equal "true", all["analytics"][:boolean]
    assert_equal "true", all["checkout"][:boolean]
  end

  def test_returned_values_are_copies_and_cannot_mutate_the_override
    @adapter.enable_override(:search, ttl: 60)
    feature = Flipper::Feature.new(:search, @adapter)
    first = @adapter.get(feature)
    first[:boolean] = "tampered"
    second = @adapter.get(feature)
    assert_equal "true", second[:boolean]
  end

  def test_remote_advances_while_override_is_active
    # The wrapped Poll adapter keeps syncing; only the overridden feature is masked.
    @adapter.enable_override(:search, ttl: 60)
    @remote.disable(:analytics)
    @remote.enable(:checkout) # already on; flip a different one instead
    @remote.enable_actor(:checkout, Flipper::Actor.new("user:42"))
    @poller.sync

    assert_equal :local, @adapter.source(:search)
    assert_equal false, @flipper.enabled?(:analytics)
    assert_equal true, @flipper.enabled?(:checkout, Flipper::Actor.new("user:42"))
  end
end
