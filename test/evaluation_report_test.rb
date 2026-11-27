require 'test_helper'

# The fidelity oracle temporarily redefines Now/Random.call with deterministic
# sources; the redefinition/removal warnings are expected and intentional.
module FlipperReportStubWarningFilter
  def warn(message)
    return if message.include?("method redefined; discarding old call") ||
      message.include?("previous definition of call was here")
    super
  end
end
Warning.singleton_class.prepend(FlipperReportStubWarningFilter)

class EvaluationReportTest < MiniTest::Test
  def setup
    @flipper = Flipper.new(Flipper::Adapters::Memory.new)
    @feature = @flipper[:search]
  end

  # An actor whose group predicates and property reads are counted so a test can
  # prove exactly how many times each input was touched during one explanation.
  class CountingActor
    attr_reader :admin_calls, :age_calls

    def initialize(id, admin:, age:)
      @id = id
      @admin = admin
      @age = age
      @admin_calls = 0
      @age_calls = 0
    end

    def flipper_id
      @id
    end

    def flipper_properties
      { "age" => age }
    end

    def admin?
      @admin_calls += 1
      @admin
    end

    def age
      @age_calls += 1
      @age
    end
  end

  def test_returns_an_evaluation_report
    report = @feature.explain
    assert_instance_of Flipper::EvaluationReport, report
  end

  def test_report_result_matches_enabled_boolean_when_fully_on
    @feature.enable
    actor = Flipper::Actor.new("User;1")

    report = @feature.explain(actor)

    assert_equal true, report.result
    assert_equal true, report.enabled?
    assert_equal @feature.enabled?(actor), report.result
  end

  def test_report_result_matches_enabled_boolean_when_fully_off
    @feature.disable
    actor = Flipper::Actor.new("User;1")

    report = @feature.explain(actor)

    assert_equal false, report.result
    assert_equal @feature.enabled?(actor), report.result
    assert_nil report.open_gate_name
  end

  def test_preserves_real_gate_order
    @feature.disable
    report = @feature.explain(Flipper::Actor.new("User;1"))

    assert_equal(
      [:boolean, :expression, :actor, :percentage_of_actors, :percentage_of_time, :group],
      report.entries.map(&:name)
    )
  end

  def test_first_open_gate_short_circuits_remaining_gates_without_calling_them
    @feature.enable
    report = @feature.explain(Flipper::Actor.new("User;1"))

    assert_equal :boolean, report.open_gate_name
    assert_equal [:boolean], report.evaluated_gate_names
    assert_equal(
      [:expression, :actor, :percentage_of_actors, :percentage_of_time, :group],
      report.skipped_gate_names
    )
    report.entries.each do |entry|
      if entry.skipped?
        assert_equal :previous_gate_open, entry.details[:reason]
        assert_nil entry.result
      end
    end
  end

  def test_boolean_gate_records_stored_value
    @feature.enable
    entry = @feature.explain.gate(:boolean)

    assert entry.evaluated?
    assert_equal true, entry.result
    assert_equal true, entry.details[:value]
  end

  def test_actor_gate_records_matched_actor
    actor = Flipper::Actor.new("User;7")
    @feature.enable_actor(actor)

    entry = @feature.explain(actor).gate(:actor)

    assert_equal true, entry.result
    assert_includes entry.details[:matched_actors], "User;7"
    assert_includes entry.details[:enabled_actors], "User;7"
  end

  def test_actor_gate_is_skipped_without_actors
    @feature.enable_actor(Flipper::Actor.new("User;7"))
    entry = @feature.explain.gate(:actor)

    assert entry.skipped?
    assert_equal :no_actors, entry.details[:reason]
  end

  def test_group_block_is_called_once_and_recorded
    Flipper.register(:report_admins) { |actor| actor.admin? }
    @feature.enable_group(:report_admins)
    actor = CountingActor.new("User;1", admin: true, age: 30)

    report = @feature.explain(actor)

    assert_equal 1, actor.admin_calls
    entry = report.gate(:group)
    assert_equal true, entry.result
    check = entry.details[:checks].first
    assert_equal "report_admins", check[:group]
    assert_equal "User;1", check[:actor]
    assert_equal true, check[:result]
  end

  def test_group_block_runs_for_each_enabled_group_until_a_match_with_real_short_circuit
    calls = []
    Flipper.register(:report_first) { |_actor| calls << :first; false }
    Flipper.register(:report_second) { |_actor| calls << :second; true }
    Flipper.register(:report_third) { |_actor| calls << :third; true }
    @feature.enable_group(:report_first)
    @feature.enable_group(:report_second)
    @feature.enable_group(:report_third)

    actor = Flipper::Actor.new("User;1")
    report = @feature.explain(actor)

    assert_equal [:first, :second], calls
    assert_equal :group, report.open_gate_name
    # The third group block never ran because :second opened the gate.
    assert_equal [:report_first, :report_second], report.gate(:group).details[:checks].map { |c| c[:group].to_sym }
  end

  def test_group_gate_skipped_when_earlier_gate_opens_and_block_never_runs
    Flipper.register(:report_late) { |actor| actor.admin? }
    @feature.enable_group(:report_late)
    @feature.enable_percentage_of_time(50)
    actor = CountingActor.new("User;1", admin: false, age: 30)

    report = @feature.explain(actor, random: ->(_max = nil) { 0.01 })

    assert_equal :percentage_of_time, report.open_gate_name
    assert report.gate(:group).skipped?
    assert_equal 0, actor.admin_calls
  end

  def test_percentage_of_time_uses_injected_random_and_records_it
    @feature.enable_percentage_of_time(50)

    open_report = @feature.explain(random: ->(_max = nil) { 0.01 })
    closed_report = @feature.explain(random: ->(_max = nil) { 0.99 })

    assert_equal true, open_report.result
    assert_equal 0.01, open_report.gate(:percentage_of_time).details[:random]
    assert_equal false, closed_report.result
    assert_equal 0.99, closed_report.gate(:percentage_of_time).details[:random]
  end

  def test_injected_random_is_called_with_no_arguments_for_percentage_of_time
    seen_args = :not_set
    random = ->(max = nil) { seen_args = max; 0.99 }
    @feature.enable_percentage_of_time(50)

    @feature.explain(random: random)

    assert_nil seen_args
  end

  def test_percentage_of_actors_gate_is_deterministic_and_recorded
    @feature.enable_percentage_of_actors(50)
    actor = Flipper::Actor.new("User;1")

    report = @feature.explain(actor)

    assert_equal @feature.enabled?(actor), report.result
    entry = report.gate(:percentage_of_actors)
    assert entry.evaluated?
    assert_kind_of Integer, entry.details[:bucket]
    assert_equal 50_000, entry.details[:threshold]
  end

  def test_expression_any_short_circuits_and_never_reads_unused_clock
    clock_calls = 0
    clock = -> { clock_calls += 1; Time.now.utc }
    expression = Flipper.any(
      Flipper.property(:age).gte(18),
      Flipper.now.lte(Flipper.time("2020-01-01"))
    )
    @feature.enable_expression(expression)
    actor = CountingActor.new("User;1", admin: false, age: 30)

    report = @feature.explain(actor, clock: clock)

    assert_equal true, report.result
    assert_equal 0, clock_calls
    node_names = expression_nodes(report).map(&:name)
    assert_equal ["Property", "GreaterThanOrEqualTo", "Any"], node_names
    refute_includes node_names, "Now"
  end

  def test_expression_all_short_circuits_and_never_reads_unused_random
    random_calls = 0
    random = ->(max = nil) { random_calls += 1; 0.5 }
    expression = Flipper.all(
      Flipper.property(:age).lt(10),
      Flipper.random(100).lt(50)
    )
    @feature.enable_expression(expression)
    actor = CountingActor.new("User;1", admin: false, age: 30)

    report = @feature.explain(actor, random: random)

    assert_equal false, report.result
    assert_equal 0, random_calls
    node_names = expression_nodes(report).map(&:name)
    assert_equal ["Property", "LessThan", "All"], node_names
    refute_includes node_names, "Random"
  end

  def test_property_is_only_read_when_its_branch_runs
    expression = Flipper.any(
      Flipper.property(:age).gte(18),
      Flipper.property(:age).gte(99)
    )
    @feature.enable_expression(expression)
    actor = CountingActor.new("User;1", admin: false, age: 30)

    report = @feature.explain(actor)

    # First Any branch is true, so the second Property(age) never evaluates.
    assert_equal 1, actor.age_calls
    assert_equal 1, expression_nodes(report).count { |node| node.name == "Property" }
  end

  def test_clock_is_read_when_used_and_each_call_returns_a_distinct_value
    ticks = [Time.utc(2026, 1, 1), Time.utc(2026, 1, 2), Time.utc(2026, 1, 3)]
    clock = -> { ticks.shift }
    expression = Flipper.now.gte(Flipper.time("2020-01-01"))
    @feature.enable_expression(expression)

    report = @feature.explain(Flipper::Actor.new("User;1"), clock: clock)
    now_node = expression_nodes(report).find { |node| node.name == "Now" }

    assert_equal Time.utc(2026, 1, 1), now_node.result
    assert_equal true, report.result
  end

  def test_random_expression_uses_injected_source_with_its_real_argument
    seen = []
    random = ->(max = nil) { seen << max; 10 }
    expression = Flipper.random(100).gte(50)
    @feature.enable_expression(expression)

    report = @feature.explain(Flipper::Actor.new("User;1"), random: random)

    assert_equal [100], seen
    random_node = expression_nodes(report).find { |node| node.name == "Random" }
    assert_equal 10, random_node.result
    assert_equal false, report.result
  end

  def test_expression_report_result_matches_enabled_for_deterministic_expressions
    # These expressions only read actor properties, so the normal enabled? path
    # and the explanation compute the same boolean without any clock/random
    # injection involved.
    expressions = [
      Flipper.property(:age).gte(18),
      Flipper.any(Flipper.property(:age).lt(5), Flipper.property(:age).gte(65)),
      Flipper.all(Flipper.property(:age).gte(0), Flipper.property(:age).lt(120)),
      Flipper.property(:age).not_equal(42),
    ]

    [1, 10, 17, 18, 42, 64, 65, 200].each do |age|
      actor = Flipper::Actor.new("User;#{age}", "age" => age)
      expressions.each do |expression|
        @feature.clear
        @feature.enable_expression(expression)
        report = @feature.explain(actor)
        assert_equal(
          @feature.enabled?(actor),
          report.result,
          "expression #{expression.value.inspect} age=#{age}"
        )
      end
    end
  end

  def test_time_gated_expression_decision_follows_injected_clock
    expression = Flipper.now.gte(Flipper.time("2026-06-01"))
    @feature.enable_expression(expression)
    actor = Flipper::Actor.new("User;1")

    before = @feature.explain(actor, clock: -> { Time.utc(2026, 1, 1) })
    after = @feature.explain(actor, clock: -> { Time.utc(2026, 12, 1) })

    assert_equal false, before.result
    assert_equal true, after.result
    # The gate-level boolean is exactly the root node's boolean.
    [before, after].each do |report|
      root = expression_nodes(report).last
      assert_equal root.name, "GreaterThanOrEqualTo"
      assert_equal report.result, !!root.result
    end
  end

  def test_random_expression_decision_follows_injected_random
    expression = Flipper.random(100).lt(25)
    @feature.enable_expression(expression)
    actor = Flipper::Actor.new("User;1")

    low = @feature.explain(actor, random: ->(_max = nil) { 10 })
    high = @feature.explain(actor, random: ->(_max = nil) { 90 })

    assert_equal true, low.result
    assert_equal false, high.result
    assert_equal 10, expression_nodes(low).find { |n| n.name == "Random" }.result
    assert_equal 90, expression_nodes(high).find { |n| n.name == "Random" }.result
  end

  def test_expression_without_actor_records_nodes
    expression = Flipper.now.gte(Flipper.time("2020-01-01"))
    @feature.enable_expression(expression)

    report = @feature.explain(clock: -> { Time.utc(2030) })

    assert_equal true, report.result
    assert_equal ["Now", "Time", "GreaterThanOrEqualTo"], report.gate(:expression).details[:nodes].map(&:name)
  end

  def test_multiple_actors_record_per_actor_expression_results
    expression = Flipper.property(:age).gte(18)
    @feature.enable_expression(expression)
    young = Flipper::Actor.new("User;1", "age" => 10)
    old = Flipper::Actor.new("User;2", "age" => 40)

    report = @feature.explain(young, old)

    results = report.gate(:expression).details[:actor_results]
    assert_equal [["User;1", false], ["User;2", true]], results.map { |r| [r[:actor], r[:result]] }
    assert_equal true, report.result
    assert_equal @feature.enabled?(young, old), report.result
  end

  def test_unknown_function_closes_gate_and_is_recorded
    # Write raw adapter data for a function this Flipper version cannot build,
    # mirroring an expression synced from a newer release.
    @feature.adapter.add(@feature)
    @feature.adapter.get(@feature)[:expression] = { "FutureFunction" => [] }

    report = @feature.explain(Flipper::Actor.new("User;1"))

    assert_equal false, report.result
    assert_equal :unknown_function, report.gate(:expression).details[:reason]
  end

  def test_dsl_and_module_level_explain
    @feature.enable
    assert_instance_of Flipper::EvaluationReport, @flipper.explain(:search)
    Flipper.instance = @flipper
    assert_instance_of Flipper::EvaluationReport, Flipper.explain(:search)
  ensure
    Flipper.instance = nil
  end

  def test_normal_enabled_path_does_not_build_a_report
    @feature.enable
    context = Flipper::FeatureCheckContext.new(
      feature_name: :search,
      values: @feature.gate_values,
      actors: [Flipper::Types::Actor.new(Flipper::Actor.new("1"))]
    )

    assert_nil context.report
    assert @feature.enabled?(Flipper::Actor.new("1"))
  end

  def test_report_to_s_includes_decision_and_gates
    @feature.enable
    string = @feature.explain.to_s

    assert_includes string, "search enabled"
    assert_includes string, "boolean: true"
    assert_includes string, "skipped"
  end

  def test_evaluator_matches_native_expression_evaluation_across_a_tree_matrix
    properties = { "age" => 30, "name" => "Grace", "missing" => nil }
    context = { feature_name: :search, properties: properties, actor: nil }
    clock = -> { Time.utc(2026, 11, 28) }
    random = ->(max = nil) { max == 100 ? 42 : 7 }

    expressions = [
      Flipper.property(:age).gte(18),
      Flipper.property(:age).lt(18),
      Flipper.property(:name).eq("Grace"),
      Flipper.property(:name).neq("Bob"),
      Flipper.property(:missing).eq(""),
      Flipper.boolean(Flipper.property(:missing)).eq(false),
      Flipper.any(Flipper.property(:age).lt(5), Flipper.property(:age).gte(65)),
      Flipper.any(Flipper.property(:age).lt(40), Flipper.property(:age).gte(65)),
      Flipper.all(Flipper.property(:age).gte(0), Flipper.property(:age).lt(120)),
      Flipper.all(Flipper.property(:age).gte(0), Flipper.property(:age).lt(20)),
      Flipper.any(
        Flipper.all(Flipper.property(:age).gte(0), Flipper.property(:age).lt(10)),
        Flipper.property(:age).gte(18)
      ),
      Flipper.now.gte(Flipper.time("2020-01-01")),
      Flipper.now.lt(Flipper.time("2020-01-01")),
      Flipper.random(100).lt(50),
      Flipper.random(100).gte(50),
      Flipper.boolean(Flipper.property(:missing)),
      Flipper.number(Flipper.property(:age)).eq(30),
      Flipper.string(Flipper.property(:age)).eq("30"),
    ]

    expressions.each do |expression|
      native = with_stubbed_sources(clock, random) do
        !!expression.evaluate(context)
      end

      report = Flipper::EvaluationReport.new(feature_name: :search, values: Flipper::GateValues.new({}), actors: nil)
      evaluator = Flipper::EvaluationReport::ExpressionEvaluator.new(
        context: context.merge(report: report),
        clock: clock,
        random: random
      )
      explained = !!evaluator.evaluate(expression)

      assert_equal native, explained, "expression #{expression.value.inspect}"
    end
  end

  def test_evaluator_records_no_nodes_for_constants_and_unwrapped_values
    report = Flipper::EvaluationReport.new(feature_name: :search, values: Flipper::GateValues.new({}), actors: nil)
    evaluator = Flipper::EvaluationReport::ExpressionEvaluator.new(
      context: { feature_name: :search, properties: {}, actor: nil, report: report }
    )

    assert_equal 42, evaluator.evaluate(Flipper.constant(42))
    assert_equal "x", evaluator.evaluate(Flipper.constant("x"))
    assert_equal [], evaluator.nodes
  end

  def test_evaluator_short_circuit_skips_nested_branch_nodes
    report = Flipper::EvaluationReport.new(feature_name: :search, values: Flipper::GateValues.new({}), actors: nil)
    evaluator = Flipper::EvaluationReport::ExpressionEvaluator.new(
      context: { feature_name: :search, properties: { "age" => 30 }, actor: nil, report: report }
    )
    expression = Flipper.any(
      Flipper.property(:age).gte(18),
      Flipper.all(Flipper.property(:age).lt(5), Flipper.now.lt(Flipper.time("2020-01-01")))
    )

    assert_equal true, evaluator.evaluate(expression)
    names = evaluator.nodes.map(&:name)

    assert_equal ["Property", "GreaterThanOrEqualTo", "Any"], names
    refute_includes names, "All"
    refute_includes names, "Now"
  end

  def test_percentage_of_actors_expression_uses_feature_name_context
    # The expression form buckets on feature_name + text just like the gate.
    expression = Flipper.property(:flipper_id).percentage_of_actors(50)
    @feature.enable_expression(expression)
    actor = Flipper::Actor.new("User;1")

    report = @feature.explain(actor)
    node = expression_nodes(report).find { |n| n.name == "PercentageOfActors" }

    refute_nil node
    assert_equal [true, false].include?(node.result), true
    assert_equal @feature.enabled?(actor), report.result
  end

  def test_report_boolean_matches_enabled_across_all_deterministic_gates
    # Percentage of time is excluded because each check draws a fresh random
    # number; it is covered separately with an injected source.
    srand 4242
    150.times do |i|
      flipper = Flipper.new(Flipper::Adapters::Memory.new)
      feature = flipper[:differential]
      actor = Flipper::Actor.new("User;#{i}", "age" => rand(0..100), "plan" => ["free", "pro"].sample)

      case rand(5)
      when 0 then feature.enable
      when 1 then feature.enable_actor(actor)
      when 2 then feature.enable_percentage_of_actors(rand(0..100))
      when 3 then feature.enable_expression(Flipper.property(:age).gte(rand(0..100)))
      when 4
        feature.enable_expression(
          Flipper.any(
            Flipper.property(:age).gte(80),
            Flipper.all(Flipper.property(:plan).eq("pro"), Flipper.property(:age).gte(18))
          )
        )
      end

      assert_equal feature.enabled?(actor), feature.explain(actor).result, "iteration #{i}"
    end
  end

  def test_percentage_of_time_report_matches_the_injected_random_draw
    srand 99
    100.times do
      @feature.clear
      @feature.enable_percentage_of_time(rand(1..99))
      draw = rand
      report = @feature.explain(random: ->(_max = nil) { draw })
      expected = draw < (@feature.percentage_of_time_value / 100.0)

      assert_equal expected, report.result
      assert_equal draw, report.gate(:percentage_of_time).details[:random]
    end
  end

  def test_feature_enabled_expression_matches_the_native_evaluation_path
    @flipper.enable(:dependency_feature)
    expression = Flipper.feature_enabled(:dependency_feature)
    @feature.enable_expression(expression)
    actor = Flipper::Actor.new("User;1")

    report = @feature.explain(actor)
    node = expression_nodes(report).find { |n| n.name == "FeatureEnabled" }

    # The reporting evaluator must produce exactly the boolean the native
    # FeatureEnabled function returns in this evaluation context.
    refute_nil node
    assert_equal @feature.enabled?(actor), report.result
    assert_equal report.result, !!node.result
  end

  def test_circular_feature_dependencies_are_false_in_the_report_too
    @flipper.enable_expression(:loop_a, Flipper.feature_enabled(:loop_b))
    @flipper.enable_expression(:loop_b, Flipper.feature_enabled(:loop_a))

    report = @flipper[:loop_a].explain(Flipper::Actor.new("User;1"))

    assert_equal false, report.result
  end

  private

  def expression_nodes(report)
    details = report.gate(:expression).details
    if details[:nodes]
      details[:nodes]
    else
      details[:actor_results].flat_map { |result| result[:nodes] }
    end
  end

  # Stubs Now/Random function classes for the duration of the block so the
  # native evaluator reads the same sources as the reporting evaluator. The
  # original singleton methods are aliased aside and restored in ensure, so no
  # state leaks between assertions.
  def with_stubbed_sources(clock, random)
    now_fn = Flipper::Expressions::Now
    random_fn = Flipper::Expressions::Random
    now_fn.singleton_class.alias_method(:_original_call, :call)
    random_fn.singleton_class.alias_method(:_original_call, :call)
    now_fn.define_singleton_method(:call) { clock.call }
    random_fn.define_singleton_method(:call) { |max = 0| random.call(max) }
    yield
  ensure
    now_fn.singleton_class.alias_method(:call, :_original_call)
    random_fn.singleton_class.alias_method(:call, :_original_call)
    now_fn.singleton_class.remove_method(:_original_call)
    random_fn.singleton_class.remove_method(:_original_call)
  end
end
