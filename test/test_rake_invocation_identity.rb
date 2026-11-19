# frozen_string_literal: true
require File.expand_path("../helper", __FILE__)

class TestRakeInvocationIdentity < Rake::TestCase # :nodoc:
  include Rake

  def setup
    super

    Task.clear
  end

  def teardown
    Rake.application.thread_pool.join

    super
  end

  # -- structural identity ---------------------------------------------

  def test_identity_is_structural_not_string_based
    # Two distinct objects with the same to_s must not share an identity.
    a = Object.new
    def a.to_s; "same"; end
    def a.inspect; "same"; end
    b = Object.new
    def b.to_s; "same"; end
    def b.inspect; "same"; end

    t = task(:t, [:arg])
    refute_equal t.identity_for(a), t.identity_for(b)
  end

  def test_identity_equality_and_hash
    t = task(:t, [:a, :b])
    id1 = t.identity_for(1, 2)
    id2 = t.identity_for(1, 2)
    id3 = t.identity_for(1, 3)

    assert_equal id1, id2
    assert_equal id1.hash, id2.hash
    refute_equal id1, id3

    lookup = { id1 => :seen }
    assert_equal :seen, lookup[id2]
    assert_nil lookup[id3]
  end

  def test_identity_binds_values_to_formal_parameter_names
    t = task(:t, [:a, :b])
    identity = t.identity_for(1, 2)

    assert_equal "t", identity.task_name
    assert_equal({ a: 1, b: 2 }, identity.bindings)
    assert_equal [], identity.extras
  end

  def test_identity_distinguishes_tasks_with_same_arguments
    t1 = task(:t1, [:a])
    t2 = task(:t2, [:a])

    refute_equal t1.identity_for(1), t2.identity_for(1)
  end

  def test_identity_includes_fully_scoped_task_name
    namespace :ns do
      task(:inner, [:a])
    end
    task(:inner, [:a])

    scoped = Task["ns:inner"].identity_for(1)
    plain  = Task["inner"].identity_for(1)

    assert_equal "ns:inner", scoped.task_name
    refute_equal scoped, plain
  end

  def test_identity_display_form
    t = task(:t, [:a])
    assert_equal "t[a: 1]", t.identity_for(1).to_s
    assert_equal "t[a: 1, *2]", t.identity_for(1, 2).to_s

    plain = task(:plain)
    assert_equal "plain", plain.identity_for.to_s
  end

  # -- defaults, extras and formal parameter binding --------------------

  def test_declared_defaults_bind_to_formal_parameters
    received = nil
    t = task(:t, [:a, { b: 2 }]) { |_tt, args| received = args.to_hash }
    assert_equal [:a, :b], t.arg_names
    assert_equal({ b: 2 }, t.arg_defaults)

    t.invoke(1)
    assert_equal({ a: 1, b: 2 }, received)
  end

  def test_defaulted_and_explicit_values_share_an_identity
    t = task(:t, [:a, { b: 2 }])
    assert_equal t.identity_for(1), t.identity_for(1, 2)
    refute_equal t.identity_for(1), t.identity_for(1, 3)
  end

  def test_explicit_value_overrides_declared_default
    received = nil
    t = task(:t, [:a, { b: 2 }]) { |_tt, args| received = args.b }
    t.invoke(1, 9)
    assert_equal 9, received
  end

  def test_extra_positional_arguments_are_part_of_the_identity
    t = task(:t, [:a])

    assert_equal [:x], t.identity_for(1, :x).extras
    refute_equal t.identity_for(1), t.identity_for(1, :x)
    refute_equal t.identity_for(1, :x), t.identity_for(1, :y)
  end

  def test_defaults_only_bind_to_declared_names
    ta = Rake::TaskArguments.new([:a], [nil])
    ta.with_defaults(a: 1, unknown: 2)
    assert_equal 1, ta[:a]
    assert_nil ta[:unknown]
    assert_equal({ a: 1 }, ta.to_hash)
  end

  # -- named arguments and unknown argument reporting -------------------

  def test_named_arguments_bind_by_parameter_name
    received = nil
    t = task(:t, [:a, :b]) { |_tt, args| received = args.to_hash }
    t.invoke(b: 2, a: 1)
    assert_equal({ a: 1, b: 2 }, received)
  end

  def test_unknown_named_arguments_are_reported_before_the_action
    ran = false
    t = task(:t, [:a]) { ran = true }

    ex = assert_raises(Rake::TaskArgumentError) { t.invoke(1, bogus: 2) }
    assert_match(/unknown argument :bogus/, ex.message)
    assert_match(/'t'/, ex.message)
    assert_equal false, ran
    assert_equal false, t.already_invoked
  end

  def test_unknown_named_arguments_are_reported_by_identity_for
    t = task(:t, [:a])
    assert_raises(Rake::TaskArgumentError) { t.identity_for(1, bogus: 2) }
  end

  def test_argument_given_both_positionally_and_by_name_is_reported
    ran = false
    t = task(:t, [:a]) { ran = true }

    assert_raises(Rake::TaskArgumentError) { t.invoke(1, a: 2) }
    assert_equal false, ran
  end

  # -- conflict policy on single-instance tasks -------------------------

  def test_same_arguments_reuse_the_instance_without_warning
    runs = []
    t = task(:t, [:a]) { |_tt, args| runs << args.a }

    _, err = capture_output do
      t.invoke(1)
      t.invoke(1)
    end

    assert_equal [1], runs
    assert_equal "", err
  end

  def test_conflicting_arguments_warn_and_reuse_the_first_instance
    runs = []
    t = task(:t, [:a]) { |_tt, args| runs << args.a }

    _, err = capture_output do
      t.invoke(1)
      t.invoke(2)
    end

    assert_equal [1], runs
    assert_match(/already invoked/, err)
    assert_match(/t\[a: 1\]/, err)
    assert_match(/t\[a: 2\]/, err)
  end

  def test_conflicting_arguments_raise_with_error_policy
    runs = []
    t = task(:t, [:a]) { |_tt, args| runs << args.a }
    t.conflict_policy = :error

    t.invoke(1)
    ex = assert_raises(Rake::TaskArgumentError) { t.invoke(2) }
    assert_match(/already invoked as t\[a: 1\]/, ex.message)
    assert_equal [1], runs
  end

  def test_conflicting_arguments_are_silent_with_reuse_policy
    runs = []
    t = task(:t, [:a]) { |_tt, args| runs << args.a }
    t.conflict_policy = :reuse

    _, err = capture_output do
      t.invoke(1)
      t.invoke(2)
    end

    assert_equal [1], runs
    assert_equal "", err
  end

  def test_conflicting_invocation_reraises_a_failed_first_instance
    t = task(:t, [:a]) { raise "boom" }
    t.conflict_policy = :reuse

    assert_raises(RuntimeError) { t.invoke(1) }
    ex = assert_raises(RuntimeError) { t.invoke(2) }
    assert_equal "boom", ex.message
  end

  def test_unknown_policy_values_are_rejected
    t = task(:t)
    assert_raises(Rake::TaskArgumentError) { t.invocation_policy = :bogus }
    assert_raises(Rake::TaskArgumentError) { t.conflict_policy = :bogus }
  end

  # -- multi-instance tasks ---------------------------------------------

  def test_multi_instance_tasks_build_once_per_identity
    runs = []
    t = task(:t, [:a]) { |_tt, args| runs << args.a }
    t.multi_instance!

    _, err = capture_output do
      t.invoke(1)
      t.invoke(2)
      t.invoke(1)
    end

    assert_equal [1, 2], runs
    assert_equal "", err
    assert_equal true, t.already_invoked
    assert_equal [t.identity_for(1), t.identity_for(2)],
      t.invocation_identities
  end

  def test_multi_instance_tasks_can_be_declared_with_an_assignment
    t = task(:t, [:a])
    t.invocation_policy = :multi
    assert t.multi_instance?

    t.invocation_policy = :single
    refute t.multi_instance?
  end

  def test_multi_instance_tasks_reraise_failures_per_identity
    runs = []
    t = task(:t, [:a]) do |_tt, args|
      runs << args.a
      raise "boom" if args.a == 1
    end
    t.invocation_policy = :multi

    assert_raises(RuntimeError) { t.invoke(1) }
    assert_raises(RuntimeError) { t.invoke(1) } # same instance: no rerun
    t.invoke(2)                                # distinct instance: runs

    assert_equal [1, 2], runs
  end

  def test_reenable_forgets_all_invocation_identities
    runs = []
    t = task(:t, [:a]) { |_tt, args| runs << args.a }

    capture_output do
      t.invoke(1)
      t.invoke(2)
    end
    t.reenable

    assert_equal [], t.invocation_identities
    capture_output { t.invoke(2) }
    assert_equal [1, 2], runs
  end

  # -- planning and tracing ---------------------------------------------

  def test_same_invocation_instance_planning
    single = task(:single, [:a])
    assert single.same_invocation_instance?([1], [2])

    multi = task(:multi, [:a])
    multi.invocation_policy = :multi
    assert multi.same_invocation_instance?([1], [1])
    refute multi.same_invocation_instance?([1], [2])
  end

  def test_trace_reports_first_and_repeated_instances
    t = task(:t, [:a]) { }
    _, err = capture_output do
      Rake.application.set_default_options
      Rake.application.options.trace = true

      t.invoke(1)
      t.invoke(1)
    end

    assert_match(%r{Invoke t\[a: 1\] \(first_time\)}, err)
    assert_match(%r{Invoke t\[a: 1\] \(same_instance\)}, err)
  ensure
    Rake.application.options.trace = false
  end

  def test_trace_reports_conflicting_instances
    t = task(:t, [:a]) { }
    _, err = capture_output do
      Rake.application.set_default_options
      Rake.application.options.trace = true

      t.invoke(1)
      t.invoke(2)
    end

    assert_match(%r{Invoke t\[a: 2\] \(conflicting_instance\)}, err)
  ensure
    Rake.application.options.trace = false
  end

  def test_trace_reports_new_instances_of_multi_instance_tasks
    t = task(:t, [:a]) { }
    t.invocation_policy = :multi
    _, err = capture_output do
      Rake.application.set_default_options
      Rake.application.options.trace = true

      t.invoke(1)
      t.invoke(2)
    end

    assert_match(%r{Invoke t\[a: 2\] \(new_instance\)}, err)
  ensure
    Rake.application.options.trace = false
  end

  def test_investigation_reports_invocation_identities
    t = task(:t, [:a]) { }
    t.invoke(1)

    out = t.investigation
    assert_match(/invocation policy: single/, out)
    assert_match(%r{^--t\[a: 1\]$}, out)
  end

  # -- prerequisites and namespaces -------------------------------------

  def test_prerequisite_arguments_bind_to_the_prerequisites_formal_parameters
    received = nil
    task(:pre, [:x, { y: 5 }]) { |_t, args| received = args.to_hash }
    t = task(:t, [:x] => [:pre])

    t.invoke(1)
    assert_equal({ x: 1, y: 5 }, received)
  end

  def test_conflicting_prerequisite_arguments_are_not_silently_reused
    received = []
    task(:shared, [:v]) { |_t, args| received << args.v }
    task(:one, [:v] => [:shared])
    task(:two, [:v] => [:shared])
    task(:all) # forces order: one then two
    Task[:all].enhance(["one", "two"])

    _, err = capture_output { Task[:all].invoke }

    # "one" and "two" both invoke "shared", but with no value for :v the
    # identities match and nothing is reported.
    assert_equal [nil], received
    assert_equal "", err
  end

  def test_namespaced_prerequisite_conflict_is_reported
    received = []
    task(:shared, [:v]) { |_t, args| received << args.v }
    namespace :ns do
      task(:one, [:v] => [:shared])
    end
    task(:top) { }
    Task[:top].enhance(["ns:one"])

    _, err = capture_output do
      Task["ns:one"].invoke(1)
      Task[:shared].invoke(2)
    end

    assert_equal [1], received
    assert_match(/already invoked as shared\[v: 1\]/, err)
  end
end
