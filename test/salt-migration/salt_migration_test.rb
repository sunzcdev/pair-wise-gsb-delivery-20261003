# frozen_string_literal: true

# Verifies the salt-migration rollout plan against a real Flipper instance
# (in-memory adapter). The offline hash math lives in
# script/salt-migration/offline_recompute.rb; this test proves that the
# production primitives (percentage-of-actors gates, expression gates and
# FeatureEnabled) behave exactly like that math, with no per-actor state.
#
# Run without bundler (the repo has no lockfile in this environment):
#   ruby -Ilib -Itest test/salt-migration/salt_migration_test.rb

$LOAD_PATH.unshift(File.expand_path("../../lib", __dir__))
require "zlib"
require "set"
require "flipper"
require "flipper/actor"
require "minitest/autorun"

class SaltMigrationTest < Minitest::Test
  OLD = :search
  NEW = :search_v2
  WRAPPER = :search_next

  # Same fixed population as the offline script.
  ACTOR_IDS = (1..5_000).map { |id| "User;#{id}" }.freeze
  ACTORS = ACTOR_IDS.map { |id| Flipper::Actor.new(id) }.freeze

  SCALING_FACTOR = 1_000
  BUCKETS = 100 * SCALING_FACTOR

  # Must match script/salt-migration/offline_recompute.rb.
  PLAN = [
    [10, nil],
    [10, 0],
    [10, 2],
    [10, 5],
    [10, 10],
    [10, 25],
    [10, 50],
    [10, 100],
    [nil, 100],
  ].freeze

  def setup
    @flipper = Flipper.new(Flipper::Adapters::Memory.new)
    Flipper.instance = @flipper # FeatureEnabled delegates through Flipper.enabled?
  end

  def teardown
    Flipper.instance = nil
    Thread.current[:flipper_evaluating_features] = nil
  end

  # Reference implementation, copied verbatim from the two production paths:
  # gates/percentage_of_actors.rb and expressions/percentage_of_actors.rb.
  def bucket(salt, actor_id)
    Zlib.crc32("#{salt}#{actor_id}") % BUCKETS
  end

  def ref_member?(salt, pct, actor_id)
    return false if pct.nil? || pct <= 0
    return true  if pct >= 100
    bucket(salt, actor_id) < pct * SCALING_FACTOR
  end

  def ref_hits(old_pct, new_pct)
    ACTOR_IDS.each_with_object(Set.new) do |id, hits|
      hits << id if ref_member?(OLD.to_s, old_pct, id) ||
                    ref_member?(NEW.to_s, new_pct, id)
    end
  end

  # Option A: call sites evaluate the union of the two feature conventions.
  def option_a_hits
    ACTORS.each_with_object(Set.new) do |actor, hits|
      on = @flipper.enabled?(OLD, actor) || @flipper.enabled?(NEW, actor)
      hits << actor.flipper_id if on
    end
  end

  # Option B: one wrapper feature ORs the two conventions via FeatureEnabled.
  def wrapper_hits
    ACTORS.each_with_object(Set.new) do |actor, hits|
      hits << actor.flipper_id if @flipper.enabled?(WRAPPER, actor)
    end
  end

  def configure(old_pct, new_pct)
    if old_pct.nil?
      @flipper.disable_percentage_of_actors(OLD)
    else
      @flipper.enable_percentage_of_actors(OLD, old_pct)
    end

    if new_pct.nil?
      @flipper.disable_percentage_of_actors(NEW)
    else
      @flipper.enable_percentage_of_actors(NEW, new_pct)
    end
  end

  # 1. The real gate reproduces the offline math on both salts.
  def test_percentage_gate_matches_offline_hash_on_both_salts
    @flipper.enable_percentage_of_actors(OLD, 10)
    @flipper.enable_percentage_of_actors(NEW, 10)

    ACTORS.each do |actor|
      assert_equal(
        ref_member?(OLD.to_s, 10, actor.flipper_id),
        @flipper.enabled?(OLD, actor),
        "old-salt gate mismatch for #{actor.flipper_id}"
      )
      assert_equal(
        ref_member?(NEW.to_s, 10, actor.flipper_id),
        @flipper.enabled?(NEW, actor),
        "new-salt gate mismatch for #{actor.flipper_id}"
      )
    end
  end

  # 2. Replaying the whole plan through BOTH production OR shapes: hit sets
  #    equal the recomputed sets, are monotonic, and never drop an actor.
  def test_transition_never_removes_an_actor_and_matches_recomputation
    # Option B wrapper, declared once; the percentages underneath move only.
    @flipper.enable(WRAPPER, Flipper.any(
      Flipper.feature_enabled(OLD),
      Flipper.feature_enabled(NEW),
    ))

    previous_a = nil
    previous_b = nil

    PLAN.each do |old_pct, new_pct|
      configure(old_pct, new_pct)
      expected = ref_hits(old_pct, new_pct)

      actual_a = option_a_hits
      actual_b = wrapper_hits

      assert_equal(expected, actual_a,
        "Option A hit set mismatch at old=#{old_pct.inspect} new=#{new_pct.inspect}")
      assert_equal(expected, actual_b,
        "Option B hit set mismatch at old=#{old_pct.inspect} new=#{new_pct.inspect}")
      assert_equal(actual_a, actual_b, "Option A and B must agree at every stage")

      [actual_a, actual_b].each do |actual|
        if previous_a
          assert_empty(previous_a - actual_a, "Option A exited actors at old=#{old_pct} new=#{new_pct}")
        end
        if previous_b
          assert_empty(previous_b - actual_b, "Option B exited actors at old=#{old_pct} new=#{new_pct}")
        end
      end

      previous_a = actual_a
      previous_b = actual_b
    end

    assert_equal(ACTOR_IDS.to_set, previous_a)
    assert_equal(ACTOR_IDS.to_set, previous_b)
  end

  # 3. Contrast with a naive cut-over, which demonstrably drops actors.
  def test_naive_salt_swap_would_have_dropped_actors
    @flipper.enable_percentage_of_actors(OLD, 10)
    before = ACTORS.each_with_object(Set.new) do |actor, hits|
      hits << actor.flipper_id if @flipper.enabled?(OLD, actor)
    end

    # Cut over with only the new-salt rule at the same 10%.
    @flipper.disable_percentage_of_actors(OLD)
    @flipper.enable_percentage_of_actors(NEW, 10)
    after_swap = ACTORS.each_with_object(Set.new) do |actor, hits|
      hits << actor.flipper_id if @flipper.enabled?(NEW, actor)
    end

    refute_empty(before - after_swap, "a real salt reshuffle must move some actors out")

    # Restoring the union at parity keeps the entire original cohort.
    @flipper.enable_percentage_of_actors(OLD, 10)
    union = option_a_hits
    assert_empty(before - union, "union plan must keep every actor from the old cohort")
  end

  # 4. Option B': a single expression feature can hold BOTH salts inline.
  #    PercentageOfActors inside an expression salts with the evaluating feature
  #    name (context[:feature_name]); FeatureEnabled brings in the old salt.
  def test_inline_two_salt_expression_matches_recomputation
    old_pct = 10
    new_pct = 5
    @flipper.enable_percentage_of_actors(OLD, old_pct)

    @flipper.enable(WRAPPER, Flipper.any(
      Flipper.feature_enabled(OLD),
      Flipper.property(:flipper_id).percentage_of_actors(new_pct),
    ))

    ACTORS.each do |actor|
      expected = ref_member?(OLD.to_s, old_pct, actor.flipper_id) ||
                 ref_member?(WRAPPER.to_s, new_pct, actor.flipper_id)
      assert_equal(expected, @flipper.enabled?(WRAPPER, actor),
        "inline expression mismatch for #{actor.flipper_id}")
    end

    assert_includes(@flipper.feature(WRAPPER).enabled_gate_names, :expression)
  end

  # 5. No per-actor state is ever written: percentage gates store one integer,
  #    the wrapper stores one expression. Only deterministic configuration.
  def test_adapter_holds_no_actor_state
    @flipper.enable_percentage_of_actors(OLD, 10)
    @flipper.enable_percentage_of_actors(NEW, 25)
    @flipper.enable(WRAPPER, Flipper.any(
      Flipper.feature_enabled(OLD),
      Flipper.feature_enabled(NEW),
    ))

    [OLD, NEW, WRAPPER].each do |name|
      values = @flipper.feature(name).gate_values
      assert_empty(Array(values.actors), "#{name} must not record individual actors")
    end

    assert_equal(10, @flipper.feature(OLD).gate_values.percentage_of_actors)
    assert_equal(25, @flipper.feature(NEW).gate_values.percentage_of_actors)
  end
end
