# frozen_string_literal: true
require File.expand_path("../helper", __FILE__)
require "json"

class TestRakeExecutionPlan < Rake::TestCase # :nodoc:
  def setup
    super
    @app = Rake.application
  end

  def plan(*targets)
    @app.execution_plan(targets)
  end

  def define_task(*args, &block)
    @app.define_task(Rake::Task, *args, &block)
  end

  def test_lists_targets_and_dependencies
    define_task(:compile) { }
    define_task(:test => [:compile]) { }
    define_task(:build => [:compile, :test]) { }

    result = plan("build")

    assert_equal ["build"], result.target_names
    assert_equal [{ requested: "build", name: "build", status: :ok }], result.targets
    assert result.ok?
    assert_equal %w[compile test build], result.each_node.map(&:name)
    assert_equal %w[compile test], result["build"].prerequisites
    assert_equal [], result["build"].order_only_prerequisites
    assert_equal 1, result["build"].action_count
  end

  def test_does_not_run_actions
    ran = []
    define_task(:compile) { ran << :compile }
    define_task(:build => [:compile]) { ran << :build }

    plan("build")

    assert_equal [], ran
  end

  def test_does_not_change_already_invoked_or_failure_state
    ran = []
    define_task(:leaf) { ran << :leaf }
    define_task(:mid => [:leaf]) do
      ran << :mid
      raise "mid failed"
    end
    define_task(:top => [:mid])

    assert_raises(RuntimeError) { @app["top"].invoke }
    failure = @app["top"].instance_variable_get(:@invocation_exception)
    assert failure
    assert @app["top"].already_invoked
    assert @app["leaf"].already_invoked
    assert_equal [:leaf, :mid], ran

    result = plan("top")

    assert @app["top"].already_invoked
    assert @app["leaf"].already_invoked
    assert result["top"].already_invoked
    assert result["mid"].already_invoked
    assert result["leaf"].already_invoked
    assert_same failure, @app["top"].instance_variable_get(:@invocation_exception)

    assert_raises(RuntimeError) { @app["top"].invoke }
    assert_equal [:leaf, :mid], ran
  end

  def test_order_only_prerequisites_are_marked
    define_task(:setup)
    define_task(:build => [:compile], order_only: [:setup])
    define_task(:compile)

    result = plan("build")

    assert_equal ["compile"], result["build"].prerequisites
    assert_equal ["setup"], result["build"].order_only_prerequisites
    assert_equal %w[compile setup build], result.each_node.map(&:name)
  end

  def test_duplicate_dependencies_keep_a_single_identity
    define_task(:compile)
    define_task(:build => [:compile, "rake:compile"])

    result = plan("build")
    node = result["build"]

    assert_equal ["compile"], node.prerequisites
    assert_equal 1, result.each_node.count { |n| n.name == "compile" }
    assert_equal 1, node.duplicate_prerequisites.size
    assert_equal "rake:compile", node.duplicate_prerequisites.first[:name]
    assert_equal "compile", node.duplicate_prerequisites.first[:identity]
  end

  def test_namespace_lookup_matches_real_resolution
    define_task("copy")
    @app.in_namespace("nest") do
      define_task("copy")
      define_task(:xx => :copy)
    end

    result = plan("nest:xx")

    assert_equal ["nest:copy"], result["nest:xx"].prerequisites
    assert_equal :task, result["nest:copy"].type
    refute result["copy"]
  end

  def test_scope_hint_and_rake_prefix_resolution
    define_task(:shared)
    @app.in_namespace("outer") do
      define_task(:shared)
      @app.in_namespace("inner") do
        define_task(:use_top => ["rake:shared"])
        define_task(:use_outer => ["^shared"])
      end
    end

    top = plan("outer:inner:use_top")
    outer = plan("outer:inner:use_outer")

    assert_equal ["shared"], top.nodes["outer:inner:use_top"].prerequisites
    assert_equal ["outer:shared"], outer.nodes["outer:inner:use_outer"].prerequisites
  end

  def test_implicit_rule_is_expanded_and_attributed
    create_file("abc.c")
    @app.create_rule(".o" => ".c") { |t| }

    result = plan("abc.o")
    node = result["abc.o"]

    assert_equal :file_task, node.type
    assert_equal ["abc.c"], node.prerequisites
    assert_equal :rule, node.derivation[:kind]
    assert_equal '/\.o$/', node.derivation[:pattern]
    assert_equal [".c"], node.derivation[:extensions]
    refute node.dynamic
    assert result["abc.c"]
    assert_equal :existing_file, result["abc.c"].derivation[:kind]
  end

  def test_rule_without_existing_source_is_unresolved
    @app.create_rule(".o" => ".c") { |t| }

    result = plan("missing.o")

    refute result.ok?
    assert_equal :unresolved, result.targets.first[:status]
    assert_match(/Don't know how to build task 'missing\.o'/, result.targets.first[:message])
  end

  def test_dynamic_rule_source_is_not_evaluated_and_stays_unknown
    create_file("abc.c")
    called = false
    @app.create_rule(".o" => [proc { |fn| called = true; fn.ext(".c") }]) { |t| }

    result = plan("abc.o")

    refute called, "plan must not call rule Proc dependents"
    refute result.ok?
    assert_equal :unknown, result.targets.first[:status]
    assert_nil result["abc.o"]
    assert_equal ["abc.o"], result.unknown.map { |entry| entry[:name] }
    assert_match(/unknown/, result.to_s)
  end

  def test_method_rule_source_is_not_evaluated
    create_file("abc.c")
    finder = Object.new
    def finder.source_for(name)
      name.ext(".c")
    end
    @app.create_rule(".o" => finder.method(:source_for)) { |t| }

    result = plan("abc.o")

    assert_equal :unknown, result.targets.first[:status]
    assert_nil result["abc.o"]
  end

  def test_tasks_created_by_an_action_are_not_predicted
    define_task(:generate) do
      @app.define_task(Rake::Task, :generated)
    end
    define_task(:all => [:generate])

    result = plan("all")

    refute result["generated"]
    assert_equal [:generate], result["all"].prerequisites.map { |name| name.to_sym }
  end

  def test_deferred_rule_is_reported_without_enhancing_the_task
    create_file("abc.c")
    @app.create_rule(".o" => ".c") { |t| }
    define_task("abc.o")

    task = @app["abc.o"]
    assert_equal 0, task.actions.size
    assert_equal [], task.prerequisites

    result = plan("abc.o")

    assert_equal :task, result["abc.o"].type
    assert_equal '/\.o$/', result["abc.o"].deferred_rule[:pattern]
    assert_equal ["abc.c"], result["abc.o"].deferred_rule[:sources]
    assert_equal [], result["abc.o"].deferred_rule[:order_only]
    assert_equal 0, task.actions.size
    assert_equal [], task.prerequisites
  end

  def test_rule_order_only_edges_are_reported
    create_file("abc.c")
    define_task(:setup)
    @app.create_rule(".o" => ".c", order_only: [:setup]) { |t| }

    result = plan("abc.o")

    assert_equal ["setup"], result["abc.o"].order_only_prerequisites
    assert_match(/order-only: setup/, result.to_s)
  end

  def test_dynamic_deferred_rule_stays_unknown
    create_file("abc.c")
    called = false
    @app.create_rule(".o" => [proc { |fn| called = true; fn.ext(".c") }]) { |t| }
    define_task("abc.o")

    result = plan("abc.o")

    refute called
    refute result.ok?
    assert result["abc.o"].dynamic
    assert_equal :unknown, result["abc.o"].status
    assert_equal "/\\.o$/", result["abc.o"].deferred_rule[:pattern]
    assert_equal [], result["abc.o"].deferred_rule[:sources]
  end

  def test_cycle_is_reported_with_a_readable_path
    define_task(:a => [:b])
    define_task(:b => [:c])
    define_task(:c => [:a])

    result = plan("a")

    refute result.ok?
    assert_equal ["a => b => c => a"], result.cycles
    assert_match(/a => b => c => a/, result.to_s)
  end

  def test_cycle_through_order_only_edge_is_reported
    define_task(:a, order_only: [:b])
    define_task(:b => [:a])

    result = plan("a")

    assert_equal ["a => b => a"], result.cycles
  end

  def test_self_cycle_is_reported
    define_task(:loop => [:loop])

    result = plan("loop")

    assert_equal ["loop => loop"], result.cycles
  end

  def test_unknown_target_is_reported_and_does_not_abort
    define_task(:known)

    result = plan("known", "nope")

    refute result.ok?
    assert_equal ["known", "nope"], result.targets.map { |target| target[:requested] }
    assert_equal :unresolved, result.targets.last[:status]
    assert result["known"]
    assert_match(/nope/, result.to_s)
  end

  def test_unresolved_prerequisite_is_reported
    define_task(:build => [:does_not_exist])

    result = plan("build")
    node = result["build"]

    refute result.ok?
    assert_equal :unresolved, node.status
    assert_equal ["does_not_exist"], node.prerequisites
    assert_equal "does_not_exist", result.unresolved.first[:name]
    assert_equal "build", result.unresolved.first[:required_by]
  end

  def test_to_h_is_serializable_and_mentions_limitations
    define_task(:compile)
    define_task(:build => [:compile])

    hash = plan("build").to_h

    assert_equal "ok", hash[:status]
    assert_equal %w[compile build], hash[:nodes].map { |node| node[:name] }
    assert_equal [], hash[:cycles]
    assert_equal [], hash[:unresolved]
    assert_equal [], hash[:unknown]
    refute_empty hash[:limitations]
    assert_equal "build", hash[:targets].first[:name]
  end

  def test_to_h_serializes_to_json
    create_file("abc.c")
    @app.create_rule(".o" => ".c") { |t| }
    define_task(:build => ["abc.o"])

    json = JSON.generate(plan("build").to_h)
    data = JSON.parse(json)

    assert_equal "ok", data["status"]
    assert_equal %w[abc.c abc.o build], data["nodes"].map { |node| node["name"] }
    assert_equal({ "kind" => "rule", "pattern" => '/\.o$/', "extensions" => [".c"], "dynamic" => false },
      data["nodes"][1]["derivation"])
  end

  def test_unknown_names_do_not_create_tasks
    define_task(:known)

    before = @app.tasks.map(&:name)
    plan("known")
    plan("not_defined")

    assert_equal before, @app.tasks.map(&:name)
  end

  def test_rule_expansion_uses_real_task_resolution
    create_file("abc.c")
    @app.create_rule(".o" => ".c") { |t| }

    refute Rake::Task.task_defined?("abc.o")
    plan("abc.o")

    # Like a real lookup, expanding an implicit rule stores the synthesized
    # file task in the application, with the rule action attached but not run.
    assert Rake::Task.task_defined?("abc.o")
    assert_equal ["abc.c"], @app["abc.o"].prerequisites
    assert_equal 1, @app["abc.o"].actions.size
  end

  def test_build_plan_alias
    define_task(:known)

    assert_equal @app.execution_plan(["known"]).to_h, @app.build_plan(["known"]).to_h
  end
end
