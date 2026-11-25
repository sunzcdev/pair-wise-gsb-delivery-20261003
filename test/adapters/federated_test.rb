require "test_helper"
require "flipper/adapters/federated"
require "flipper/adapters/operation_logger"

class FederatedAdapterTest < MiniTest::Test
  def setup
    @local_adapter = Flipper::Adapters::Memory.new
    @legacy_adapter = Flipper::Adapters::Memory.new
    @legacy = Flipper.new(@legacy_adapter, memoize: false)

    @adapter = Flipper::Adapters::Federated.new(
      @local_adapter,
      external: { legacy: @legacy_adapter },
      dependencies: {
        checkout_v2: { search: :legacy }
      }
    )

    # FeatureEnabled resolves nested references through the global instance,
    # so point it at the same federated adapter.
    @flipper = Flipper.new(@adapter, memoize: false)
    Flipper.instance = @flipper
  end

  def teardown
    Thread.current[Flipper::Expressions::FeatureEnabled::ORIGIN_STACK_KEY] = nil
    Thread.current[:flipper_evaluating_features] = nil
    Flipper.configuration = nil
  end

  def test_declared_reference_follows_the_external_source
    @legacy.enable(:search)
    @flipper.enable_expression(:checkout_v2, Flipper.feature_enabled(:search))

    assert_equal true, @flipper.enabled?(:checkout_v2)
    refute_includes @local_adapter.features, "search"
  end

  def test_declared_reference_tracks_source_changes_after_switching_adapters
    @legacy.enable(:search)
    @flipper.enable_expression(:checkout_v2, Flipper.feature_enabled(:search))

    assert_equal true, @flipper.enabled?(:checkout_v2)

    @legacy.disable(:search)
    assert_equal false, @flipper.enabled?(:checkout_v2)

    @legacy.enable(:search)
    assert_equal true, @flipper.enabled?(:checkout_v2)
  end

  def test_undeclared_same_name_reference_reports_missing_dependency
    @legacy.enable(:search)
    @flipper.enable_expression(:checkout_v3, Flipper.feature_enabled(:search))

    error = assert_raises(Flipper::MissingDependency) do
      @flipper.enabled?(:checkout_v3)
    end
    assert_equal "checkout_v3", error.feature_name
    assert_equal "search", error.dependency_name
    assert_nil Thread.current[Flipper::Expressions::FeatureEnabled::ORIGIN_STACK_KEY]
  end

  def test_declared_and_undeclared_edges_side_by_side
    @legacy.enable(:search)
    @flipper.enable_expression(:checkout_v2, Flipper.feature_enabled(:search))
    @flipper.enable_expression(:checkout_v3, Flipper.feature_enabled(:search))

    # Declared edge follows the actual external source value.
    assert_equal true, @flipper.enabled?(:checkout_v2)

    # Same referenced name without a declaration is reported, not resolved.
    assert_raises(Flipper::MissingDependency) { @flipper.enabled?(:checkout_v3) }

    @legacy.disable(:search)
    assert_equal false, @flipper.enabled?(:checkout_v2)
    assert_raises(Flipper::MissingDependency) { @flipper.enabled?(:checkout_v3) }
  end

  def test_ordinary_local_gate_rules_keep_evaluating_locally
    @flipper.enable(:checkout_v2)
    assert_equal true, @flipper.enabled?(:checkout_v2)

    @flipper.disable(:checkout_v2)
    assert_equal false, @flipper.enabled?(:checkout_v2)
    assert_includes @local_adapter.features, "checkout_v2"
  end

  def test_local_actor_rules_do_not_query_external_source
    actor = Flipper::Actor.new("user-1")
    @flipper.enable_actor(:checkout_v2, actor)

    assert_equal true, @flipper.enabled?(:checkout_v2, actor)
    assert_equal false, @flipper.enabled?(:checkout_v2, Flipper::Actor.new("user-2"))
  end

  def test_feature_disabled_uses_same_declared_edge
    @legacy.disable(:search)
    @flipper.enable_expression(:checkout_v2, Flipper.feature_disabled(:search))

    assert_equal true, @flipper.enabled?(:checkout_v2)

    @legacy.enable(:search)
    assert_equal false, @flipper.enabled?(:checkout_v2)
  end

  def test_routes_each_edge_to_its_named_external_source
    other_adapter = Flipper::Adapters::Memory.new
    other = Flipper.new(other_adapter, memoize: false)

    adapter = Flipper::Adapters::Federated.new(
      @local_adapter,
      external: { legacy: @legacy_adapter, other: other_adapter },
      dependencies: {
        checkout_v2: { search: :legacy },
        billing_v2: { invoices: :other }
      }
    )
    flipper = Flipper.new(adapter, memoize: false)
    Flipper.instance = flipper

    other.enable(:invoices)
    flipper.enable_expression(:billing_v2, Flipper.feature_enabled(:invoices))

    assert_equal true, flipper.enabled?(:billing_v2)
    refute_includes @legacy_adapter.features, "invoices"
    refute_includes @local_adapter.features, "invoices"

    other.disable(:invoices)
    assert_equal false, flipper.enabled?(:billing_v2)
  end

  def test_rejects_unknown_external_source_in_declaration
    error = assert_raises(ArgumentError) do
      Flipper::Adapters::Federated.new(
        @local_adapter,
        external: { legacy: @legacy_adapter },
        dependencies: { checkout_v2: { search: :nowhere } }
      )
    end
    assert_match(/Unknown external source :nowhere/, error.message)
  end

  def test_adapter_stack_describes_local_and_external_adapters
    assert_equal "federated(local: memory, external: {legacy: memory})",
      @adapter.adapter_stack
  end

  def test_nested_references_are_routed_even_while_memoizing
    # The default DSL wraps the adapter in Memoizable, which must not cache a
    # nested reference ahead of the federated routing.
    @legacy.enable(:search)
    memoized = Flipper.new(@adapter)
    Flipper.instance = memoized
    memoized.enable_expression(:checkout_v2, Flipper.feature_enabled(:search))
    memoized.enable_expression(:checkout_v3, Flipper.feature_enabled(:search))

    memoized.memoize = true
    assert_equal true, memoized.enabled?(:checkout_v2)
    assert_equal true, memoized.enabled?(:checkout_v2)
    assert_raises(Flipper::MissingDependency) { memoized.enabled?(:checkout_v3) }
    memoized.memoize = false

    @legacy.disable(:search)
    memoized.memoize = true
    assert_equal false, memoized.enabled?(:checkout_v2)
    memoized.memoize = false
  end

  def test_undeclared_reference_never_reads_any_external_source
    @legacy.enable(:search)
    instrumented = Flipper::Adapters::OperationLogger.new(@legacy_adapter)

    adapter = Flipper::Adapters::Federated.new(
      @local_adapter,
      external: { legacy: instrumented },
      dependencies: { checkout_v2: { search: :legacy } }
    )
    guarded = Flipper.new(adapter, memoize: false)
    Flipper.instance = guarded
    guarded.enable_expression(:checkout_v3, Flipper.feature_enabled(:search))

    assert_raises(Flipper::MissingDependency) { guarded.enabled?(:checkout_v3) }
    assert_equal 0, instrumented.count(:get)
  end

  def test_undeclared_reference_inside_composed_expression_is_reported
    @flipper.enable_expression(
      :checkout_v3,
      Flipper.any(
        Flipper.feature_enabled(:search),
        Flipper.property(:plan).eq("basic")
      )
    )

    error = assert_raises(Flipper::MissingDependency) do
      @flipper.enabled?(:checkout_v3)
    end
    assert_equal "checkout_v3", error.feature_name
    assert_equal "search", error.dependency_name
  end

  def test_circular_reference_is_broken_without_losing_origin_state
    # A -> B -> A must return false (the existing cycle guard) and leave no
    # origin stack behind, even when both edges are declared externally.
    adapter = Flipper::Adapters::Federated.new(
      @local_adapter,
      external: { legacy: @legacy_adapter },
      dependencies: {
        alpha: { beta: :legacy },
        beta: { alpha: :legacy }
      }
    )
    flipper = Flipper.new(adapter, memoize: false)
    Flipper.instance = flipper

    flipper.enable_expression(:alpha, Flipper.feature_enabled(:beta))
    flipper.enable_expression(:beta, Flipper.feature_enabled(:alpha))

    assert_equal false, flipper.enabled?(:alpha)
    assert_nil Thread.current[Flipper::Expressions::FeatureEnabled::ORIGIN_STACK_KEY]
  end
end
