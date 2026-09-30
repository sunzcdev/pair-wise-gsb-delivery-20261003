# frozen_string_literal: true

require 'test_helper'

class DropAccessPolicyTest < Minitest::Test
  # A drop whose fields count their own invocations in a private ivar, so
  # tests can prove that denied paths never invoke the underlying method.
  class CountingDrop < Liquid::Drop
    def initialize
      super
      @call_counts = Hash.new(0)
    end

    def name
      @call_counts[:name] += 1
      "Alice"
    end

    def secret
      @call_counts[:secret] += 1
      "s3cret"
    end

    def child
      @call_counts[:child] += 1
      @child ||= CountingDrop.new
    end

    def children
      @call_counts[:children] += 1
      @children ||= [CountingDrop.new, CountingDrop.new]
    end

    def details
      @call_counts[:details] += 1
      { "color" => "red", "secret" => "hidden" }
    end

    def liquid_method_missing(method)
      @call_counts[:method_missing] += 1
      "catchall: #{method}"
    end

    private

    def call_count(field)
      @call_counts[field]
    end
  end

  class UnrelatedDrop < Liquid::Drop
    def anything
      "anything"
    end
  end

  def render_template(template, drop, **options)
    Liquid::Template.parse(template).render({ "drop" => drop }, **options)
  end

  def test_allows_listed_fields_and_denies_others_without_calling_them
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new(CountingDrop => ["name"])

    output = render_template("{{ drop.name }}|{{ drop.secret }}", drop, drop_access_policy: policy)

    assert_equal("Alice|", output)
    assert_equal(1, drop.send(:call_count, :name))
    assert_equal(0, drop.send(:call_count, :secret))
  end

  def test_denied_field_raises_in_strict_mode_without_calling_it
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new(CountingDrop => ["name"])

    error = assert_raises(Liquid::UndefinedDropMethod) do
      Liquid::Template.parse("{{ drop.secret }}").render!(
        { "drop" => drop },
        drop_access_policy: policy,
        strict_variables: true,
      )
    end

    assert_match("undefined method secret", error.message)
    assert_equal(0, drop.send(:call_count, :secret))
  end

  def test_denied_field_does_not_fall_back_to_liquid_method_missing
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new(CountingDrop => ["name"])

    output = render_template("{{ drop.unknown }}", drop, drop_access_policy: policy)

    assert_equal("", output)
    assert_equal(0, drop.send(:call_count, :method_missing))
  end

  def test_allowed_field_still_uses_liquid_method_missing
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new(CountingDrop => ["unknown"])

    output = render_template("{{ drop.unknown }}", drop, drop_access_policy: policy)

    assert_equal("catchall: unknown", output)
    assert_equal(1, drop.send(:call_count, :method_missing))
  end

  def test_nested_drop_is_constrained_by_path
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new(CountingDrop => ["child.name"])

    output = render_template(
      "{{ drop.child.name }}|{{ drop.child.secret }}",
      drop,
      drop_access_policy: policy,
    )

    assert_equal("Alice|", output)
    child = drop.child
    assert_equal(1, child.send(:call_count, :name))
    assert_equal(0, child.send(:call_count, :secret))
  end

  def test_drops_inside_returned_arrays_are_constrained
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new(CountingDrop => ["children.name"])

    output = render_template(
      "{% for c in drop.children %}{{ c.name }}|{{ c.secret }};{% endfor %}",
      drop,
      drop_access_policy: policy,
    )

    assert_equal("Alice|;Alice|;", output)
    drop.children.each do |child|
      assert_equal(1, child.send(:call_count, :name))
      assert_equal(0, child.send(:call_count, :secret))
    end
  end

  def test_returned_hashes_are_narrowed_to_allowed_keys
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new(CountingDrop => ["details.color"])

    output = render_template(
      "{{ drop.details.color }}|{{ drop.details.secret }}",
      drop,
      drop_access_policy: policy,
    )

    assert_equal("red|", output)
    assert_equal(2, drop.send(:call_count, :details))
  end

  def test_narrowed_hash_raises_in_strict_mode
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new(CountingDrop => ["details.color"])

    assert_raises(Liquid::UndefinedVariable) do
      Liquid::Template.parse("{{ drop.details.secret }}").render!(
        { "drop" => drop },
        drop_access_policy: policy,
        strict_variables: true,
      )
    end
  end

  def test_drop_classes_without_rules_are_unconstrained
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new(CountingDrop => ["name"])

    output = Liquid::Template.parse("{{ other.anything }}|{{ drop.secret }}").render(
      { "other" => UnrelatedDrop.new, "drop" => drop },
      drop_access_policy: policy,
    )

    assert_equal("anything|", output)
    assert_equal(0, drop.send(:call_count, :secret))
  end

  def test_policy_applies_per_render_and_does_not_rewrite_drop_methods
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new(CountingDrop => ["name"])

    assert_equal("", render_template("{{ drop.secret }}", drop, drop_access_policy: policy))
    assert_equal(0, drop.send(:call_count, :secret))

    # The same drop renders unrestricted once the policy is gone.
    assert_equal("s3cret", render_template("{{ drop.secret }}", drop))
    assert_equal(1, drop.send(:call_count, :secret))

    # A policy render does not change the class-level invokable methods.
    assert_includes(CountingDrop.invokable_methods, "secret")
  end

  def test_policy_applies_inside_rendered_partials
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new(CountingDrop => ["name"])

    file_system = StubFileSystem.new("snippet" => "{{ drop.name }}|{{ drop.secret }}")
    environment = Liquid::Environment.build(file_system: file_system)
    template    = Liquid::Template.parse("{% render 'snippet', drop: drop %}", environment: environment)
    registers   = Liquid::Registers.new(file_system: file_system)
    context     = Liquid::Context.build(
      static_environments: { "drop" => drop },
      registers: registers,
      environment: environment,
    )

    output = template.render(context, drop_access_policy: policy)

    assert_equal("Alice|", output)
    assert_equal(1, drop.send(:call_count, :name))
    assert_equal(0, drop.send(:call_count, :secret))
  end

  def test_rules_apply_to_subclasses
    subclass = Class.new(CountingDrop)
    drop     = subclass.new
    policy   = Liquid::DropAccessPolicy.new(CountingDrop => ["name"])

    output = render_template("{{ drop.name }}|{{ drop.secret }}", drop, drop_access_policy: policy)

    assert_equal("Alice|", output)
    assert_equal(0, drop.send(:call_count, :secret))
  end
end
