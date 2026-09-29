# frozen_string_literal: true

require 'test_helper'
require 'liquid/template_inheritance'

class TemplateInheritanceTest < Minitest::Test
  SKELETON = "<html>{% block body %}default body{% endblock %}|" \
    "{% block footer %}default footer{% endblock %}</html>"

  def inheritance_environment(file_system)
    Liquid::Environment.build(file_system: file_system) do |env|
      Liquid::TemplateInheritance.register!(env)
    end
  end

  def parse_template(source, environment)
    Liquid::Template.parse(source, environment: environment)
  end

  def render_source(source, file_system, cached_partials: {})
    environment = inheritance_environment(file_system)
    template = parse_template(source, environment)
    context = Liquid::Context.build(
      registers: { cached_partials: cached_partials },
      environment: environment,
    )
    template.render!(context)
  end

  def test_child_overrides_only_body
    file_system = StubFileSystem.new('skeleton' => SKELETON)
    output = render_source(
      "{% extends 'skeleton' %}{% block body %}child body{% endblock %}",
      file_system,
    )
    assert_equal("<html>child body|default footer</html>", output)
  end

  def test_child_overrides_only_footer
    file_system = StubFileSystem.new('skeleton' => SKELETON)
    output = render_source(
      "{% extends 'skeleton' %}{% block footer %}child footer{% endblock %}",
      file_system,
    )
    assert_equal("<html>default body|child footer</html>", output)
  end

  def test_block_without_extends_renders_its_own_body
    file_system = StubFileSystem.new({})
    output = render_source("{% block body %}standalone{% endblock %}", file_system)
    assert_equal("standalone", output)
  end

  def test_content_after_extends_outside_blocks_is_ignored
    file_system = StubFileSystem.new('skeleton' => SKELETON)
    output = render_source(
      "{% extends 'skeleton' %}this is dropped{% block body %}kept{% endblock %}also dropped",
      file_system,
    )
    assert_equal("<html>kept|default footer</html>", output)
  end

  def test_super_renders_parent_block_content
    file_system = StubFileSystem.new(
      'skeleton' => "<main>{% block body %}parent body{% endblock %}</main>",
    )
    output = render_source(
      "{% extends 'skeleton' %}{% block body %}child[{% super %}]{% endblock %}",
      file_system,
    )
    assert_equal("<main>child[parent body]</main>", output)
  end

  def test_super_walks_multiple_inheritance_levels
    file_system = StubFileSystem.new(
      'base' => "<main>{% block content %}base{% endblock %}</main>",
      'mid' => "{% extends 'base' %}{% block content %}mid({% super %}){% endblock %}",
    )
    output = render_source(
      "{% extends 'mid' %}{% block content %}leaf({% super %}){% endblock %}",
      file_system,
    )
    assert_equal("<main>leaf(mid(base))</main>", output)
  end

  def test_most_derived_override_wins_in_deep_chains
    file_system = StubFileSystem.new(
      'base' => "{% block content %}base{% endblock %}",
      'mid' => "{% extends 'base' %}{% block content %}mid{% endblock %}",
    )
    output = render_source(
      "{% extends 'mid' %}{% block content %}leaf{% endblock %}",
      file_system,
    )
    assert_equal("leaf", output)
  end

  def test_super_without_parent_block_renders_nothing
    file_system = StubFileSystem.new({})
    output = render_source("{% block body %}[{% super %}]{% endblock %}", file_system)
    assert_equal("[]", output)
  end

  def test_duplicate_block_names_are_rejected
    file_system = StubFileSystem.new('skeleton' => SKELETON)
    error = assert_raises(Liquid::SyntaxError) do
      render_source(
        "{% extends 'skeleton' %}{% block body %}one{% endblock %}{% block body %}two{% endblock %}",
        file_system,
      )
    end
    assert_match(/duplicate block 'body'/, error.message)
  end

  def test_extends_must_be_the_first_tag
    file_system = StubFileSystem.new('skeleton' => SKELETON)
    error = assert_raises(Liquid::SyntaxError) do
      render_source("hello{% extends 'skeleton' %}", file_system)
    end
    assert_match(/'extends' must be the first tag/, error.message)
  end

  def test_extends_must_be_a_top_level_tag
    file_system = StubFileSystem.new('skeleton' => SKELETON)
    environment = inheritance_environment(file_system)
    error = assert_raises(Liquid::SyntaxError) do
      parse_template("{% if true %}{% extends 'skeleton' %}{% endif %}", environment)
    end
    assert_match(/'extends' must be a top-level tag/, error.message)
  end

  def test_extends_requires_a_template_name
    file_system = StubFileSystem.new({})
    environment = inheritance_environment(file_system)
    assert_raises(Liquid::SyntaxError) do
      parse_template("{% extends %}", environment)
    end
  end

  def test_invalid_block_name_is_rejected
    file_system = StubFileSystem.new({})
    environment = inheritance_environment(file_system)
    assert_raises(Liquid::SyntaxError) do
      parse_template("{% block 1bad %}{% endblock %}", environment)
    end
  end

  def test_unknown_tag_after_extends_is_rejected
    file_system = StubFileSystem.new('skeleton' => SKELETON)
    environment = inheritance_environment(file_system)
    assert_raises(Liquid::SyntaxError) do
      parse_template("{% extends 'skeleton' %}{% notatag %}", environment)
    end
  end

  def test_parent_is_loaded_through_the_controlled_file_system
    environment = inheritance_environment(Liquid::BlankFileSystem.new)
    template = parse_template("{% extends 'skeleton' %}", environment)
    assert_raises(Liquid::FileSystemError) do
      template.render!
    end
  end

  def test_alternating_children_do_not_mutate_the_cached_parent
    file_system = StubFileSystem.new('skeleton' => SKELETON)
    environment = inheritance_environment(file_system)
    child_one = parse_template("{% extends 'skeleton' %}{% block body %}C1{% endblock %}", environment)
    child_two = parse_template("{% extends 'skeleton' %}{% block footer %}C2{% endblock %}", environment)
    registers = { cached_partials: {} }

    2.times do
      assert_equal(
        "<html>C1|default footer</html>",
        child_one.render!(Liquid::Context.build(registers: registers, environment: environment)),
      )
      assert_equal(
        "<html>default body|C2</html>",
        child_two.render!(Liquid::Context.build(registers: registers, environment: environment)),
      )
    end

    # The parent was read from the file system only once: both children share
    # the same parsed (cached) parent template.
    assert_equal(1, file_system.file_read_count)
    assert_equal(1, registers[:cached_partials].size)

    # The cached parent itself is untouched by the children that extend it.
    cached_parent = registers[:cached_partials].values.first
    assert_equal(
      "<html>default body|default footer</html>",
      cached_parent.render!,
    )
  end

  def test_circular_inheritance_reports_the_diagnostic_chain
    file_system = StubFileSystem.new(
      'a' => "{% extends 'b' %}{% block body %}a{% endblock %}",
      'b' => "{% extends 'a' %}{% block body %}b{% endblock %}",
    )
    environment = inheritance_environment(file_system)
    template = parse_template("{% extends 'a' %}", environment)

    error = assert_raises(Liquid::TemplateInheritance::InheritanceError) do
      template.render!
    end
    assert_match(/circular template inheritance/, error.message)
    assert_match(/a -> b -> a/, error.message)
  end

  def test_self_inheritance_is_a_cycle
    file_system = StubFileSystem.new(
      'a' => "{% extends 'a' %}",
    )
    environment = inheritance_environment(file_system)
    template = parse_template("{% extends 'a' %}", environment)

    error = assert_raises(Liquid::TemplateInheritance::InheritanceError) do
      template.render!
    end
    assert_match(/a -> a/, error.message)
  end
end
