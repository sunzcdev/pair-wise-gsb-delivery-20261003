# frozen_string_literal: true

require "test_helper"

require "redis_client/hash_ring"
require "digest/sha1"

class RedisClient
  class HashRingTest < RedisClientTestCase
    include ClientTestHelper

    setup do
      @nodes = [
        new_client(db: 1, id: "cache-1"),
        new_client(db: 2, id: "cache-2"),
        new_client(db: 3, id: "cache-3"),
      ]
      @ring = RedisClient.ring(@nodes)
    end

    def test_node_for
      assert_equal @nodes[2], @ring.node_for("foo")
      assert_equal @nodes[0], @ring.node_for("bar")
      assert_equal @nodes[1], @ring.node_for("baz")

      assert_equal @nodes[0], @nodes[0].node_for("baz")
    end

    def test_custom_digest
      @ring = RedisClient.ring(@nodes, digest: Digest::SHA1)

      assert_equal @nodes[0], @ring.node_for("foo")
      assert_equal @nodes[2], @ring.node_for("bar")
      assert_equal @nodes[1], @ring.node_for("baz999")
    end

    def test_nodes_for
      mapping = @ring.nodes_for("foo", "bar", "baz", "egg", "spam", "plop")
      assert_equal 3, mapping.size

      assert_equal ["baz", "spam", "plop"], mapping[@nodes[1]]
      assert_equal ["bar", "egg"], mapping[@nodes[0]]
      assert_equal ["foo"], mapping[@nodes[2]]

      assert_equal mapping, @ring.nodes_for(["foo", "bar", "baz", "egg", "spam", "plop"])

      assert_equal({ @nodes[0] => ["foo", "bar"] }, @nodes[0].nodes_for("foo", "bar"))
    end

    def test_nodes
      assert_equal @nodes, @ring.nodes
      assert_equal [@nodes[0]], @nodes[0].nodes
    end

    def test_default_weights_are_equal
      assert_equal({ "cache-1" => 1, "cache-2" => 1, "cache-3" => 1 }, @ring.weights)
      assert_equal @nodes.size * RedisClient::HashRing::POINTS_PER_SERVER, @ring.keyspace_points.size
    end

    def test_explicit_weights_add_virtual_points
      weighted = RedisClient.ring(@nodes, weights: { "cache-1" => 3, "cache-2" => 1, "cache-3" => 1 })

      assert_equal({ "cache-1" => 3, "cache-2" => 1, "cache-3" => 1 }, weighted.weights)
      expected_points = (3 + 1 + 1) * RedisClient::HashRing::POINTS_PER_SERVER
      assert_equal expected_points, weighted.keyspace_points.size
    end

    def test_weights_accept_node_objects_and_symbols
      by_node = RedisClient.ring(@nodes, weights: { @nodes[0] => 2 })
      by_symbol = RedisClient.ring(@nodes, weights: { "cache-1": 2 })

      assert_equal({ "cache-1" => 2, "cache-2" => 1, "cache-3" => 1 }, by_node.weights)
      assert_equal by_node.layout_fingerprint, by_symbol.layout_fingerprint
    end

    def test_weights_accept_parallel_array
      ring = RedisClient.ring(@nodes, weights: [3, 1, 1])

      assert_equal({ "cache-1" => 3, "cache-2" => 1, "cache-3" => 1 }, ring.weights)

      error = assert_raises(ArgumentError) { RedisClient.ring(@nodes, weights: [1, 2]) }
      assert_match(/count/, error.message)
    end

    def test_hash_style_nodes
      ring = RedisClient::HashRing.new([{ id: "one" }, { "id" => "two" }])

      assert_equal({ "one" => 1, "two" => 1 }, ring.weights)
      assert_equal ["one", "two"], ring.node_ids
    end

    def test_node_without_identity_rejected
      error = assert_raises(ArgumentError) { RedisClient::HashRing.new([Object.new]) }
      assert_match(/stable identity/, error.message)
    end

    def test_duplicate_weights_for_same_node_rejected
      error = assert_raises(ArgumentError) do
        RedisClient.ring(@nodes, weights: { "cache-1" => 2, :"cache-1" => 3 })
      end
      assert_match(/duplicate weight/, error.message)
    end

    def test_layout_fingerprint_ignores_input_order_with_weights_and_seed
      a = RedisClient.ring(@nodes, weights: { "cache-1" => 3, "cache-2" => 1, "cache-3" => 2 }, seed: "s")
      b = RedisClient.ring(@nodes.reverse, weights: { "cache-3" => 2, "cache-1" => 3, "cache-2" => 1 }, seed: "s")

      assert_equal a.layout_fingerprint, b.layout_fingerprint
    end

    def test_migration_plan_equality_and_class_level_compare
      remaining = RedisClient.ring([@nodes[0], @nodes[1]])
      plan = @ring.migration_plan(remaining)

      assert_equal plan, @ring.compare(remaining)
      assert_equal plan, @ring.migration_report(remaining)
      assert_equal plan, RedisClient::HashRing.migration_plan(@ring, remaining)
      assert_equal plan, RedisClient::HashRing.compare(@ring, remaining)
    end

    def test_weight_from_node
      node = Struct.new(:id, :weight) do
        def config
          Struct.new(:server_url).new("redis://#{id}:6379")
        end
      end.new("heavy", 4)

      ring = RedisClient::HashRing.new([node])

      assert_equal({ "heavy" => 4 }, ring.weights)
      assert_equal 4 * RedisClient::HashRing::POINTS_PER_SERVER, ring.keyspace_points.size
    end

    def test_layout_is_order_independent
      reordered = RedisClient.ring(@nodes.reverse)

      assert_equal @ring.layout_fingerprint, reordered.layout_fingerprint

      %w(foo bar baz egg spam plop).each do |key|
        assert_equal @ring.node_for(key), reordered.node_for(key)
      end
    end

    def test_seed_changes_layout_deterministically
      seeded = RedisClient.ring(@nodes, seed: "shard-2026")
      reordered = RedisClient.ring(@nodes.reverse, seed: "shard-2026")

      assert_equal "shard-2026", seeded.seed
      assert_equal seeded.layout_fingerprint, reordered.layout_fingerprint
      refute_equal @ring.layout_fingerprint, seeded.layout_fingerprint
    end

    def test_seeded_layout_survives_process_restart
      script = <<~RUBY
        $LOAD_PATH.unshift #{File.expand_path("../../lib", __dir__).dump}
        require "redis_client/hash_ring"
        node = Struct.new(:id) { def config; Struct.new(:server_url).new("redis://\#{id}:6379"); end }
        ring = RedisClient::HashRing.new([node.new("a"), node.new("b")], seed: "stable")
        print ring.layout_fingerprint
      RUBY

      outputs = 2.times.map do
        IO.popen([RbConfig.ruby, "-e", script], &:read)
      end

      assert_equal outputs[0], outputs[1]
      refute_empty outputs[0]
    end

    def test_duplicate_nodes_rejected_before_layout
      error = assert_raises(ArgumentError) { RedisClient.ring([@nodes[0], @nodes[0]]) }
      assert_match(/duplicate node id/, error.message)
    end

    def test_zero_and_invalid_weights_rejected
      [0, -1, 1.5, "2", nil].each do |weight|
        error = assert_raises(ArgumentError) do
          RedisClient.ring(@nodes, weights: { "cache-1" => weight })
        end
        assert_match(/weight/, error.message)
      end
    end

    def test_weights_for_unknown_node_rejected
      error = assert_raises(ArgumentError) { RedisClient.ring(@nodes, weights: { "cache-4" => 2 }) }
      assert_match(/unknown node/, error.message)
    end

    def test_migration_plan_is_read_only
      weighted = RedisClient.ring(@nodes, weights: { "cache-1" => 3, "cache-2" => 1, "cache-3" => 1 })
      before = [@ring.layout_fingerprint, weighted.layout_fingerprint, @ring.weights, weighted.weights]

      plan = @ring.migration_plan(weighted)
      plan.to_h
      plan.describe

      assert_equal before, [@ring.layout_fingerprint, weighted.layout_fingerprint, @ring.weights, weighted.weights]
    end

    def test_migration_plan_reports_keyspace_intervals
      weighted = RedisClient.ring(@nodes, weights: { "cache-1" => 3, "cache-2" => 1, "cache-3" => 1 })
      plan = @ring.migration_plan(weighted)

      refute plan.empty?
      assert_equal plan.changed_keyspace, plan.sum(&:size)
      assert_operator plan.changed_fraction, :>, 0.0
      assert_operator plan.changed_fraction, :<=, 1.0

      plan.each do |interval|
        assert_operator interval.finish, :>, interval.start
        refute_nil interval.from
        refute_nil interval.to
      end

      assert_match(/no keys are read, moved or deleted/, plan.describe)
      assert_equal RedisClient::HashRing::KEYSPACE, plan.to_h[:keyspace]
    end

    def test_identical_layouts_have_empty_plan
      same = RedisClient.ring(@nodes.reverse)
      plan = @ring.migration_plan(same)

      assert plan.empty?
      assert_equal 0, plan.changed_keyspace
      assert_equal 0.0, plan.changed_fraction
    end

    def test_removed_node_has_deterministic_successor
      remaining = RedisClient.ring([@nodes[0], @nodes[1]])
      plan = @ring.migration_plan(remaining)

      assert_equal ["cache-3"], plan.removed_nodes
      assert_empty plan.added_nodes

      removed_intervals = plan.select(&:removed?)
      refute_empty removed_intervals
      removed_intervals.each do |interval|
        refute_nil interval.to, "removed keyspace must have a successor"
        assert_includes ["cache-1", "cache-2"], interval.to
      end

      assert_equal plan.successors, @ring.migration_plan(RedisClient.ring([@nodes[1], @nodes[0]])).successors
    end

    def test_successor_is_independent_of_input_order
      plan_a = @ring.migration_plan(RedisClient.ring([@nodes[0], @nodes[1]]))
      plan_b = @ring.migration_plan(RedisClient.ring([@nodes[1], @nodes[0]]))

      assert_equal plan_a.to_h, plan_b.to_h
    end

    def test_added_node_only_claims_keyspace
      with_new = RedisClient.ring([@nodes[0], @nodes[1], @nodes[2], fake_node("cache-4")])
      plan = @ring.migration_plan(with_new)

      assert_equal ["cache-4"], plan.added_nodes
      assert_empty plan.removed_nodes

      plan.select(&:added?).each do |interval|
        assert_equal "cache-4", interval.to
      end
    end

    def test_comparison_requires_a_ring
      assert_raises(ArgumentError) { @ring.migration_plan(@nodes) }
    end

    def test_empty_ring_has_no_owner_and_empty_plan
      empty = RedisClient.ring([])

      assert_nil empty.node_for("foo")
      assert_nil empty.node_id_for("foo")
      assert_empty empty.nodes
      assert_empty empty.weights
      assert empty.migration_plan(empty).empty?
      assert_equal @ring.migration_plan(empty).changed_keyspace, RedisClient::HashRing::KEYSPACE
    end

    def test_weight_for_and_nodes_by_id
      ring = RedisClient.ring(@nodes, weights: { "cache-1" => 5 })

      assert_equal 5, ring.weight_for("cache-1")
      assert_equal 5, ring.weight_for(@nodes[0])
      assert_equal 5, ring.weight_for(:"cache-1")
      assert_equal 1, ring.weight_for("cache-2")
      assert_nil ring.weight_for("cache-4")
      assert_equal @nodes, ring.nodes_by_id.values_at("cache-1", "cache-2", "cache-3")
    end

    def test_migration_intervals_are_disjoint_and_cover_changes
      weighted = RedisClient.ring(@nodes, weights: { "cache-1" => 3, "cache-2" => 1, "cache-3" => 1 })
      plan = @ring.migration_plan(weighted)

      sorted = plan.intervals.sort_by(&:start)
      sorted.each_cons(2) do |previous, following|
        assert_operator previous.finish, :<=, following.start, "intervals must not overlap"
      end
      assert_equal plan.changed_keyspace, sorted.sum(&:size)

      rng = Random.new(42)
      500.times do
        hash = rng.rand(RedisClient::HashRing::KEYSPACE)
        covering = plan.intervals.select { |interval| interval.covers?(hash) }
        changed = @ring.node_id_for_hash(hash) != weighted.node_id_for_hash(hash)
        assert_equal changed, !covering.empty?
        assert_equal 1, covering.size if changed
      end
    end

    def test_legacy_equal_weight_interface_is_unchanged
      legacy = RedisClient::HashRing.new(@nodes)

      assert_equal @ring.layout_fingerprint, legacy.layout_fingerprint
      assert_equal({ "cache-1" => 1, "cache-2" => 1, "cache-3" => 1 }, legacy.weights)
    end

    private

    def new_client(**overrides)
      RedisClient.config(**tcp_config.merge(overrides)).new_pool
    end

    def fake_node(id)
      node = Struct.new(:id) do
        def config
          Struct.new(:server_url).new("redis://#{id}:6379")
        end
      end
      node.new(id)
    end
  end
end
