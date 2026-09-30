# frozen_string_literal: true

require 'test_helper'

class DropAccessPolicyTest < Minitest::Test
  # Every field increments a private counter when invoked, so tests can prove
  # that a denied access never calls the underlying method.
  class CountingDrop < Liquid::Drop
    def initialize(log = Hash.new(0))
      super()
      @log = log
    end

    def invocations(name)
      @log[name.to_sym]
    end

    def title
      count(:title)
      'Dr. Jekyll'
    end

    def secret
      count(:secret)
      's3cr3t'
    end

    def nested
      count(:nested)
      NestedCountingDrop.new(@log)
    end

    def items
      count(:items)
      [ItemCountingDrop.new('one', @log), ItemCountingDrop.new('two', @log)]
    end

    def meta
      count(:meta)
      { 'public' => 'p', 'token' => 't0k' }
    end

    private

    def hidden
      count(:hidden)
      'hidden'
    end

    def count(name)
      @log[name] += 1
    end
  end

  class NestedCountingDrop < Liquid::Drop
    def initialize(log)
      super()
      @log = log
    end

    def name
      @log[:nested_name] += 1
      'Hyde'
    end

    def token
      @log[:nested_token] += 1
      't0k'
    end
  end

  class ItemCountingDrop < Liquid::Drop
    def initialize(name, log)
      super()
      @name = name
      @log  = log
    end

    def name
      @log[:item_name] += 1
      @name
    end

    def token
      @log[:item_token] += 1
      't0k'
    end
  end

  def render_template(source, drop, policy: nil, **options)
    options[:drop_access_policy] = policy if policy
    Liquid::Template.parse(source).render({ 'drop' => drop }, **options)
  end

  def test_lax_mode_renders_allowed_fields_and_blanks_denied_ones
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'title')

    output = render_template('{{ drop.title }}|{{ drop.secret }}', drop, policy: policy)

    assert_equal('Dr. Jekyll|', output)
    assert_equal(1, drop.invocations(:title))
    assert_equal(0, drop.invocations(:secret))
  end

  def test_strict_mode_raises_undefined_drop_method_for_denied_field
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'title')

    template = Liquid::Template.parse('{{ drop.title }} {{ drop.secret }}')
    error    = assert_raises(Liquid::UndefinedDropMethod) do
      template.render!({ 'drop' => drop }, strict_variables: true, drop_access_policy: policy)
    end

    assert_match('undefined method secret', error.message)
    assert_equal(1, drop.invocations(:title))
    assert_equal(0, drop.invocations(:secret))
  end

  def test_nested_drop_is_constrained_by_path_rule
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'nested' => ['name'])

    output = render_template('{{ drop.nested.name }}|{{ drop.nested.token }}', drop, policy: policy)

    assert_equal('Hyde|', output)
    assert_equal(1, drop.invocations(:nested_name))
    assert_equal(0, drop.invocations(:nested_token))
  end

  def test_nested_drop_is_constrained_by_its_own_class_rule
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'nested')
    policy.allow(NestedCountingDrop, 'name')

    output = render_template('{{ drop.nested.name }}|{{ drop.nested.token }}', drop, policy: policy)

    assert_equal('Hyde|', output)
    assert_equal(1, drop.invocations(:nested_name))
    assert_equal(0, drop.invocations(:nested_token))
  end

  def test_hash_returned_by_field_stays_constrained
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'meta' => ['public'])

    output = render_template('{{ drop.meta.public }}|{{ drop.meta.token }}', drop, policy: policy)

    assert_equal('p|', output)
  end

  def test_hash_constraint_survives_assignment_to_another_variable
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'meta' => ['public'])

    output = render_template(
      "{% assign m = drop.meta %}{{ m.public }}|{{ m.token }}",
      drop,
      policy: policy,
    )

    assert_equal('p|', output)
  end

  def test_array_elements_stay_constrained_inside_for_loops
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'items' => ['name'])

    output = render_template(
      "{% for item in drop.items %}{{ item.name }}:{{ item.token }};{% endfor %}",
      drop,
      policy: policy,
    )

    assert_equal('one:;two:;', output)
    assert_equal(2, drop.invocations(:item_name))
    assert_equal(0, drop.invocations(:item_token))
  end

  def test_array_elements_stay_constrained_through_index_and_command_access
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'items' => ['name'])

    output = render_template(
      '{{ drop.items[0].name }}|{{ drop.items[0].token }}|{{ drop.items.first.token }}|{{ drop.items.size }}',
      drop,
      policy: policy,
    )

    assert_equal('one|||2', output)
    assert_equal(0, drop.invocations(:item_token))
  end

  def test_private_method_is_never_invoked_even_when_policy_allows_it
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'title', 'hidden')

    output = render_template('{{ drop.hidden }}', drop, policy: policy)

    assert_equal('', output)
    assert_equal(0, drop.invocations(:hidden))
  end

  def test_policy_applies_to_drop_passed_as_render_argument
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'title')

    output = Liquid::Template.parse('{{ title }}|{{ secret }}').render(drop, drop_access_policy: policy)

    assert_equal('Dr. Jekyll|', output)
    assert_equal(1, drop.invocations(:title))
    assert_equal(0, drop.invocations(:secret))
  end

  def test_policy_is_scoped_to_a_single_render
    drop     = CountingDrop.new
    template = Liquid::Template.parse('{{ drop.title }}|{{ drop.secret }}')

    internal = Liquid::DropAccessPolicy.new
    internal.allow(CountingDrop, 'title', 'secret')
    external = Liquid::DropAccessPolicy.new
    external.allow(CountingDrop, 'title')

    assert_equal('Dr. Jekyll|s3cr3t', template.render({ 'drop' => drop }, drop_access_policy: internal))
    assert_equal('Dr. Jekyll|', template.render({ 'drop' => drop }, drop_access_policy: external))
    assert_equal('Dr. Jekyll|s3cr3t', template.render('drop' => drop))
  end

  def test_policy_does_not_modify_drop_class_methods
    drop        = CountingDrop.new
    policy      = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'title')
    invokable   = CountingDrop.invokable_methods.dup

    render_template('{{ drop.secret }}', drop, policy: policy)

    assert_equal(invokable, CountingDrop.invokable_methods)
    assert(CountingDrop.invokable?('secret'))
    assert_equal('s3cr3t', drop.secret)
  end

  def test_denied_hash_key_raises_undefined_variable_in_strict_mode
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'meta' => ['public'])

    template = Liquid::Template.parse('{{ drop.meta.token }}')
    assert_raises(Liquid::UndefinedVariable) do
      template.render!({ 'drop' => drop }, strict_variables: true, drop_access_policy: policy)
    end
  end

  def test_repeated_allow_unions_fields
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'title')
    policy.allow(CountingDrop, 'secret')

    output = render_template('{{ drop.title }}|{{ drop.secret }}', drop, policy: policy)

    assert_equal('Dr. Jekyll|s3cr3t', output)
  end

  def test_deny_all_when_class_is_registered_without_fields
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop)

    output = render_template('{{ drop.title }}|{{ drop.secret }}', drop, policy: policy)

    assert_equal('|', output)
    assert_equal(0, drop.invocations(:title))
    assert_equal(0, drop.invocations(:secret))
  end

  def test_unrelated_drops_are_not_constrained
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(NestedCountingDrop, 'name')

    output = render_template('{{ drop.title }}|{{ drop.secret }}', drop, policy: policy)

    assert_equal('Dr. Jekyll|s3cr3t', output)
  end

  def test_policy_applies_inside_rendered_partials
    drop   = CountingDrop.new
    policy = Liquid::DropAccessPolicy.new
    policy.allow(CountingDrop, 'title')

    file_system = StubFileSystem.new('snippet' => '{{ drop.title }}|{{ drop.secret }}')
    environment = Liquid::Environment.build(file_system: file_system)
    template    = Liquid::Template.parse('{% render "snippet", drop: drop %}', environment: environment)
    output      = template.render({ 'drop' => drop }, drop_access_policy: policy)

    assert_equal('Dr. Jekyll|', output)
    assert_equal(1, drop.invocations(:title))
    assert_equal(0, drop.invocations(:secret))
  end
end
