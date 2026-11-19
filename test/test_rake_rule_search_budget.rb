# frozen_string_literal: true
require File.expand_path("../helper", __FILE__)

class TestRakeRuleSearchBudget < Rake::TestCase # :nodoc:
  include Rake

  def setup
    super
    Task.clear
    @runs = []
  end

  def test_default_budget_is_positive_and_configurable
    budget = Rake.application.rule_search_budget
    assert_kind_of Rake::RuleSearchBudget, budget
    assert budget.max_depth > 0
    assert budget.max_candidates > 0
  end

  def test_budget_rejects_non_positive_limits
    assert_raises(ArgumentError) { Rake::RuleSearchBudget.new(max_depth: 0) }
    assert_raises(ArgumentError) { Rake::RuleSearchBudget.new(max_candidates: -1) }
    assert_raises(ArgumentError) { Rake::RuleSearchBudget.new(max_depth: "nope") }
  end

  def test_assignment_accepts_budget_or_hash
    Rake.application.rule_search_budget = { max_depth: 3, max_candidates: 5 }
    assert_equal 3, Rake.application.rule_search_budget.max_depth
    assert_equal 5, Rake.application.rule_search_budget.max_candidates

    budget = Rake::RuleSearchBudget.new(max_depth: 2, max_candidates: 2)
    Rake.application.rule_search_budget = budget
    assert_same budget, Rake.application.rule_search_budget

    assert_raises(ArgumentError) { Rake.application.rule_search_budget = 10 }
  end

  def test_report_records_accepted_candidate
    create_file("abc.c")
    rule ".o" => ".c" do |t| @runs << t.name end

    Task["abc.o"]

    report = Rake.application.last_rule_search_report
    refute_nil report
    assert report.resolved?
    refute report.undetermined?
    assert_equal :rule, report.resolution_reason
    accepted = report.accepted_attempts.find { |a| a.source == "abc.c" }
    refute_nil accepted
    assert_equal :existing_file, accepted.reason
    refute_empty report.attempted_rules
  end

  def test_report_records_rejected_candidate_and_reason
    rule ".o" => ".missing" do |t| @runs << t.name end

    assert_raises(RuntimeError) { Task["abc.o"] }

    report = Rake.application.last_rule_search_report
    refute_nil report
    assert report.not_found?
    refute report.undetermined?
    rejection = report.rejected_attempts.find { |a| a.source == "abc.missing" }
    refute_nil rejection
    assert_equal :missing_candidate, rejection.reason
    refute_empty report.attempted_rules
  end

  def test_budget_exhaustion_is_undetermined_not_missing
    Rake.application.rule_search_budget =
      Rake::RuleSearchBudget.new(max_depth: 32, max_candidates: 2)
    create_file("a.txt")
    rule ".a" => ".txt" do |t| @runs << t.name end
    rule ".b" => ".a" do |t| @runs << t.name end
    rule ".c" => ".b" do |t| @runs << t.name end

    ex = assert_raises(Rake::RuleSearchBudgetExceeded) { Task["a.c"] }
    refute_match(/Don't know how to build/, ex.message)
    assert_match(/undetermined/i, ex.message)
    assert_match(/does not mean the file is missing/i, ex.message)

    report = ex.report
    assert report.budget_exhausted?
    assert report.undetermined?
    refute report.not_found?
    assert_equal 2, report.budget.max_candidates
    assert report.candidate_count > report.budget.max_candidates
    assert_equal :budget_exhausted, report.rejection_reasons.last
    refute_empty ex.attempts
    assert_equal [], @runs, "resolution must not run actions"
  end

  def test_budget_allows_a_fully_specified_search
    create_file("abc.c")
    Rake.application.rule_search_budget =
      Rake::RuleSearchBudget.new(max_depth: 32, max_candidates: 1)
    rule ".o" => ".c" do |t| @runs << t.name end

    Task["abc.o"].invoke
    assert_equal ["abc.o"], @runs
  end

  def test_depth_limit_reported_as_undetermined
    Rake.application.rule_search_budget =
      Rake::RuleSearchBudget.new(max_depth: 2, max_candidates: 100)
    create_file("a.a")
    rule ".b" => ".a" do |t| @runs << t.name end
    rule ".c" => ".b" do |t| @runs << t.name end
    rule ".d" => ".c" do |t| @runs << t.name end

    ex = assert_raises(Rake::RuleRecursionOverflowError) { Task["a.d"] }
    report = ex.report
    refute_nil report
    assert report.depth_exceeded?
    assert report.undetermined?
    assert_equal [], @runs, "resolution must not run actions"
  end

  def test_cycle_reports_the_file_chain_and_rules
    rule ".b" => ".a" do |t| @runs << t.name end
    rule ".a" => ".b" do |t| @runs << t.name end

    ex = assert_raises(Rake::RuleCycleError) { Task["a.b"] }
    description = ex.cycle_description
    refute_nil description
    assert_includes description, "a.b"
    assert_includes description, "a.a"
    assert_match(/a\.b/, ex.message)
    assert_match(/a\.a/, ex.message)

    report = ex.report
    assert report.undetermined?
    assert_equal [], @runs, "resolution must not run actions"
  end

  def test_cycle_on_one_branch_does_not_hide_a_working_rule
    create_file("abc.c")
    rule ".o" => ".loop" do |t| @runs << :bad end
    rule ".loop" => ".o" do |t| @runs << :bad end
    rule ".o" => ".c" do |t| @runs << t.name end

    task = Task["abc.o"]
    report = Rake.application.last_rule_search_report
    assert report.resolved?
    refute report.undetermined?
    refute_nil task.source
    assert_equal "abc.c", task.source
  end

  def test_multistage_rules_still_produce_the_original_prerequisites
    create_file("abc.xml")
    rule ".y" => ".xml" do @runs << "y" end
    rule ".c" => ".y" do @runs << "c" end
    rule ".o" => ".c" do @runs << "o" end
    rule ".exe" => ".o" do @runs << "exe" end

    Task["abc.exe"].invoke
    assert_equal ["y", "c", "o", "exe"], @runs
  end

  def test_multistage_resolution_report_shows_the_rule_chain
    create_file("abc.xml")
    rule ".y" => ".xml" do @runs << "y" end
    rule ".c" => ".y" do @runs << "c" end

    Task["abc.c"]
    report = Rake.application.last_rule_search_report
    assert report.resolved?
    refute report.undetermined?
    synthesized = report.accepted_attempts.select { |a| a.reason == :synthesized_rule }
    refute_empty synthesized
    assert_equal ["abc.y"], synthesized.map(&:source)
    assert_equal 2, report.accepted_attempts.size
  end

  def test_rules_revision_invalidates_stale_report
    create_file("abc.c")
    rule ".o" => ".c" do |t| @runs << t.name end
    Task["abc.o"]
    report = Rake.application.last_rule_search_report
    refute_nil report
    revision = Rake.application.rules_revision

    rule ".x" => ".c" do |t| @runs << t.name end
    assert Rake.application.rules_revision > revision
    assert report.stale?(Rake.application.rules_revision)
    assert_nil Rake.application.last_rule_search_report,
               "a report from another rule configuration must not be reused"

    Task.clear
    assert_nil Rake.application.last_rule_search_report
  end

  def test_report_snapshot_is_a_hash
    create_file("abc.c")
    rule ".o" => ".c" do |t| @runs << t.name end
    Task["abc.o"]

    snapshot = Rake.application.last_rule_search_report.to_h
    assert_equal "abc.o", snapshot[:target]
    assert_equal :resolved, snapshot[:status]
    assert_equal false, snapshot[:undetermined]
    assert snapshot[:attempts].is_a?(Array)
    refute_empty snapshot[:attempts]
  end

  def test_resolution_only_examines_rule_named_candidates
    create_file("abc.c")
    create_file("abc.h")
    create_file("unrelated.cpp")
    rule ".o" => [".c", ".h"] do |t| @runs << t.name end

    Task["abc.o"]
    report = Rake.application.last_rule_search_report
    sources = report.attempts.map(&:source).compact
    assert_equal %w[abc.c abc.h], sources.sort
    assert_equal [], @runs
  end

  def test_proc_dependent_lookup_shares_the_search_budget
    Rake.application.rule_search_budget =
      Rake::RuleSearchBudget.new(max_depth: 32, max_candidates: 1)
    create_file("a.seed")
    rule ".a" => ".seed" do |t| @runs << t.name end
    rule ".b" => proc { |name| Task[name.ext(".a")]; "a.a" } do |t|
      @runs << t.name
    end

    ex = assert_raises(Rake::RuleSearchBudgetExceeded) { Task["a.b"] }
    assert ex.report.undetermined?
    assert_equal 1, ex.report.budget.max_candidates
    assert ex.report.candidate_count > 1,
           "the proc's own lookup consumed the same budget"
    assert_equal [], @runs
  end

  def test_implicit_file_task_is_recorded_without_a_rule
    create_file("hello_world")

    task = Task["hello_world"]
    assert_kind_of Rake::FileTask, task
    report = Rake.application.last_rule_search_report
    refute_nil report
    assert_equal :existing_file, report.resolution_reason
    assert report.resolved?
    assert report.target_file_exists
  end

  def test_existing_file_target_is_not_reported_as_missing
    Rake.application.rule_search_budget =
      Rake::RuleSearchBudget.new(max_depth: 32, max_candidates: 1)
    create_file("a.z")
    rule ".y" => ".x" do |t| @runs << t.name end
    rule ".z" => ".y" do |t| @runs << t.name end

    ex = assert_raises(Rake::RuleSearchBudgetExceeded) { Task["a.z"] }
    refute_match(/Don't know how to build/, ex.message)
    refute_match(/missing$/, ex.message)
    assert ex.target_file_exists,
           "the target file exists even though the plan is undetermined"
    assert ex.report.undetermined?
    assert_equal [], @runs
  end
end
