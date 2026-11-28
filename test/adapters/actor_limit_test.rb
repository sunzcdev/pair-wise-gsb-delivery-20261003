require "test_helper"
require "flipper/test/shared_adapter_test"
require "flipper/adapters/actor_limit"

class Flipper::Adapters::ActorLimitTest < MiniTest::Test
  prepend Flipper::Test::SharedAdapterTests

  def setup
    @memory = Flipper::Adapters::Memory.new
    @adapter = Flipper::Adapters::ActorLimit.new(@memory, 5)
  end

  def test_enable_fails_when_limit_exceeded
    5.times { |i| @feature.enable Flipper::Actor.new("User;#{i}") }

    assert_raises Flipper::Adapters::ActorLimit::LimitExceeded do
      @feature.enable Flipper::Actor.new("User;6")
    end
  end

  # A full set can still swap a member because the swap does not grow it, and
  # the total is bounded for the whole change.
  def test_replace_at_limit_succeeds_and_keeps_total_bounded
    limit = 2
    adapter = Flipper::Adapters::ActorLimit.new(Flipper::Adapters::Memory.new, limit)
    flipper = Flipper.new(adapter)
    flipper.enable_actor :pilot, Flipper::Actor.new("old")
    flipper.enable_actor :pilot, Flipper::Actor.new("kept")

    result = flipper.replace_actor(:pilot, Flipper::Actor.new("old"), Flipper::Actor.new("new"))

    assert_equal true, result
    assert_equal Set["kept", "new"], flipper[:pilot].actors_value
    refute flipper.enabled?(:pilot, Flipper::Actor.new("old"))
    assert flipper.enabled?(:pilot, Flipper::Actor.new("new"))
    assert flipper.enabled?(:pilot, Flipper::Actor.new("kept"))
    assert_equal limit, flipper[:pilot].actors_value.size
  end

  # A change that is not actually a swap (the member to remove is not enabled)
  # is refused instead of silently adding a member that would grow the set.
  def test_replace_that_is_not_a_swap_is_refused
    adapter = Flipper::Adapters::ActorLimit.new(Flipper::Adapters::Memory.new, 1)
    flipper = Flipper.new(adapter)
    flipper.enable_actor :pilot, Flipper::Actor.new("old")

    assert_raises Flipper::ActorSetConflict do
      flipper.replace_actor(:pilot, Flipper::Actor.new("ghost"), Flipper::Actor.new("new"))
    end

    assert_equal Set["old"], flipper[:pilot].actors_value
    assert flipper.enabled?(:pilot, Flipper::Actor.new("old"))
    refute flipper.enabled?(:pilot, Flipper::Actor.new("new"))
  end

  # Adapters that only expose per-member writes fall back to add-then-remove,
  # so a failed add leaves the original member enabled.
  def test_failed_add_keeps_the_original_member_enabled
    per_member = Class.new do
      include Flipper::Adapter

      def initialize
        @data = {}
        @fail_values = []
      end

      attr_reader :data

      def fail_values=(values)
        @fail_values = values
      end

      def features
        @data.keys.to_set
      end

      def add(feature)
        @data[feature.key] ||= default_config.dup
        true
      end

      def get(feature)
        @data[feature.key] || default_config.dup
      end

      def enable(feature, gate, thing)
        raise IOError, "store unavailable" if @fail_values.include?(thing.value.to_s)

        @data[feature.key] ||= default_config.dup
        (@data[feature.key][gate.key] ||= Set.new) << thing.value.to_s
        true
      end

      def disable(feature, gate, thing)
        @data[feature.key][gate.key].delete(thing.value.to_s) if @data[feature.key]
        true
      end
    end

    store = per_member.new
    store.fail_values = ["new"]
    flipper = Flipper.new(Flipper::Adapters::ActorLimit.new(store, 2))
    flipper.enable_actor :pilot, Flipper::Actor.new("old")

    assert_raises(IOError) do
      flipper.replace_actor(:pilot, Flipper::Actor.new("old"), Flipper::Actor.new("new"))
    end

    assert_equal Set["old"], flipper[:pilot].actors_value
    assert flipper.enabled?(:pilot, Flipper::Actor.new("old"))
    refute flipper.enabled?(:pilot, Flipper::Actor.new("new"))
  end

  def test_stale_expected_members_raise_retryable_conflict_and_write_nothing
    adapter = Flipper::Adapters::ActorLimit.new(Flipper::Adapters::Memory.new, 1)
    flipper = Flipper.new(adapter)
    flipper.enable_actor :pilot, Flipper::Actor.new("old")

    error = assert_raises Flipper::ActorSetConflict do
      flipper.replace_actor(
        :pilot,
        Flipper::Actor.new("old"),
        Flipper::Actor.new("new"),
        expected: ["old", "ghost"],
      )
    end

    assert error.retryable?
    assert_equal Set["old"], flipper[:pilot].actors_value
    assert flipper.enabled?(:pilot, Flipper::Actor.new("old"))
  end

  # Two admins replace at the same time on a set with zero slots left, one of
  # them working from a stale member list. The real gate results say who won
  # and who was told to retry, and the total never exceeds the limit.
  def test_racing_replacements_at_zero_remaining_slots_yield_one_winner_and_one_retry
    adapter = Flipper::Adapters::ActorLimit.new(Flipper::Adapters::Memory.new, 1)
    flipper = Flipper.new(adapter)
    flipper.enable_actor :pilot, Flipper::Actor.new("old")

    # Both admins rendered the page while "old" was the only member.
    observed = flipper[:pilot].actors_value.to_a

    go = Queue.new
    results = Queue.new
    threads = 2.times.map do |index|
      Thread.new do
        go.pop
        new_actor = Flipper::Actor.new("new#{index}")
        begin
          flipper.replace_actor(:pilot, Flipper::Actor.new("old"), new_actor, expected: observed)
          results << [new_actor, :ok]
        rescue Flipper::ActorSetConflict
          results << [new_actor, :retry]
        end
      end
    end
    2.times { go << true }
    threads.each(&:join)

    outcomes = []
    outcomes << results.pop until results.empty?
    winners = outcomes.select { |_actor, status| status == :ok }
    losers = outcomes.select { |_actor, status| status == :retry }

    assert_equal 1, winners.size, "exactly one replacement should win"
    assert_equal 1, losers.size, "exactly one replacement should be told to retry"

    winner_actor = winners.first.first
    loser_actor = losers.first.first

    # The real gate results show who was actually replaced in.
    assert_equal Set[winner_actor.flipper_id], flipper[:pilot].actors_value
    assert_equal 1, flipper[:pilot].actors_value.size
    assert flipper.enabled?(:pilot, winner_actor)
    refute flipper.enabled?(:pilot, loser_actor)
    refute flipper.enabled?(:pilot, Flipper::Actor.new("old"))
  end

  # One admin read the page before another change, so their member list is
  # already stale. That replacement loses deterministically and changes nothing
  # while the one with fresh members applies.
  def test_replacement_with_a_stale_member_list_loses_to_the_fresh_one
    adapter = Flipper::Adapters::ActorLimit.new(Flipper::Adapters::Memory.new, 1)
    flipper = Flipper.new(adapter)
    flipper.enable_actor :pilot, Flipper::Actor.new("old")

    stale = ["old", "ghost"]
    fresh = flipper[:pilot].actors_value.to_a

    assert_raises(Flipper::ActorSetConflict) do
      flipper.replace_actor(:pilot, Flipper::Actor.new("old"), Flipper::Actor.new("new_stale"), expected: stale)
    end
    assert_equal Set["old"], flipper[:pilot].actors_value

    assert_equal true, flipper.replace_actor(
      :pilot,
      Flipper::Actor.new("old"),
      Flipper::Actor.new("new_fresh"),
      expected: fresh,
    )

    assert_equal Set["new_fresh"], flipper[:pilot].actors_value
    assert flipper.enabled?(:pilot, Flipper::Actor.new("new_fresh"))
    refute flipper.enabled?(:pilot, Flipper::Actor.new("new_stale"))
    refute flipper.enabled?(:pilot, Flipper::Actor.new("old"))
  end

  def test_replace_passes_through_a_wrapper    memory = Flipper::Adapters::Memory.new
    adapter = Flipper::Adapters::ActorLimit.new(memory, 5)
    flipper = Flipper.new(adapter)
    flipper.enable_actor :stats, Flipper::Actor.new("old")

    assert_equal true, adapter.replace_actor(
      flipper[:stats],
      Flipper::Actor.new("old"),
      Flipper::Actor.new("new"),
    )
    assert_equal Set["new"], flipper[:stats].actors_value
  end
end
