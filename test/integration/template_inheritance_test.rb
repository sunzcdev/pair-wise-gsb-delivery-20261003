# frozen_string_literal: true

require 'test_helper'

class TemplateInheritanceTest < Minitest::Test
  def setup
    @environment = Liquid::Environment.build do |environment|
      Liquid::TemplateInheritance.register!(environment)
    end
  end

  def test_child_overrides_only_the_named_block
    partials = {
      'notification' => "Subject: update\n{% block body %}generic body{% endblock %}\n{% block footer %}sent by Acme{% endblock %}",
    }

    assert_equal(
      "Subject: update\nYour order shipped\nsent by Acme",
      render_source("{% extends 'notification' %}{% block body %}Your order shipped{% endblock %}", partials),
    )

    assert_equal(
      "Subject: update\ngeneric body\nunsubscribe",
      render_source("{% extends 'notification' %}{% block footer %}unsubscribe{% endblock %}", partials),
    )
  end

  def test_parent_skeleton_sees_child_assigns
    partials = { 'layout' => "Hello {{ name }}: {% block body %}empty{% endblock %}" }

    assert_equal(
      "Hello world: body",
      render_source("{% extends 'layout' %}{% block body %}body{% endblock %}", partials, assigns: { 'name' => 'world' }),
    )
  end

  def test_super_renders_parent_block_content
    partials = { 'layout' => "[{% block content %}base{% endblock %}]" }

    assert_equal(
      "[child>base]",
      render_source("{% extends 'layout' %}{% block content %}child>{% super %}{% endblock %}", partials),
    )
  end

  def test_multi_level_overrides_and_super_chain
    partials = {
      'base' => "[{% block content %}base{% endblock %}]",
      'mid'  => "{% extends 'base' %}{% block content %}mid({% super %}){% endblock %}",
    }

    # most derived override wins, {% super %} walks up one level at a time
    assert_equal(
      "[child(mid(base))]",
      render_source("{% extends 'mid' %}{% block content %}child({% super %}){% endblock %}", partials),
    )

    # a level that does not override simply passes the ancestor content through
    assert_equal("[mid(base)]", render_source("{% extends 'mid' %}", partials))

    # an override without {% super %} fully replaces the ancestor content
    assert_equal(
      "[just child]",
      render_source("{% extends 'mid' %}{% block content %}just child{% endblock %}", partials),
    )
  end

  def test_super_without_parent_block_renders_nothing
    assert_equal("[a]", render_source("[{% block content %}a{% super %}{% endblock %}]"))
  end

  def test_alternating_children_do_not_mutate_cached_parent
    partials = { 'layout' => "H[{% block body %}default{% endblock %}]F" }
    shared_registers = {
      file_system: StubFileSystem.new(partials),
      cached_partials: {},
    }

    child_a = parse_template("{% extends 'layout' %}{% block body %}A{% endblock %}")
    child_b = parse_template("{% extends 'layout' %}{% block body %}B{% endblock %}")

    2.times do
      assert_equal("H[A]F", child_a.render!({}, registers: shared_registers))
      assert_equal("H[B]F", child_b.render!({}, registers: shared_registers))
    end

    # the parent was parsed once and the cached copy still renders its defaults
    assert_equal(1, shared_registers[:cached_partials].size)
    cached_parent = shared_registers[:cached_partials].values.first
    assert_equal("H[default]F", cached_parent.render!)
  end

  def test_inheritance_cycle_reports_the_chain
    partials = {
      'alpha' => "{% extends 'beta' %}",
      'beta'  => "{% extends 'alpha' %}",
    }
    template = parse_template("{% extends 'alpha' %}")

    error = assert_raises(Liquid::StackLevelError) do
      template.render!({}, registers: { file_system: StubFileSystem.new(partials) })
    end
    assert_match(%r{Circular template inheritance detected: alpha -> beta -> alpha}, error.message)
  end

  def test_self_inheritance_reports_the_chain
    partials = { 'loop' => "{% extends 'loop' %}" }
    template = parse_template("{% extends 'loop' %}")

    error = assert_raises(Liquid::StackLevelError) do
      template.render!({}, registers: { file_system: StubFileSystem.new(partials) })
    end
    assert_match(%r{loop -> loop}, error.message)
  end

  def test_duplicate_block_names_are_rejected
    error = assert_raises(Liquid::SyntaxError) do
      parse_template("{% block body %}a{% endblock %}{% block body %}b{% endblock %}")
    end
    assert_match(%r{Duplicate block name 'body'}, error.message)
  end

  def test_extends_may_only_appear_once
    error = assert_raises(Liquid::SyntaxError) do
      parse_template("{% extends 'a' %}{% extends 'b' %}")
    end
    assert_match(%r{Only one 'extends' tag}, error.message)
  end

  def test_extends_must_come_before_any_output
    template = parse_template("before {% extends 'layout' %}{% block body %}x{% endblock %}")

    error = assert_raises(Liquid::SyntaxError) do
      template.render!({}, registers: { file_system: StubFileSystem.new('layout' => 'layout') })
    end
    assert_match(%r{must be the first tag}, error.message)
  end

  def test_extends_must_be_a_top_level_tag
    error = assert_raises(Liquid::SyntaxError) do
      parse_template("{% if true %}{% extends 'layout' %}{% endif %}")
    end
    assert_match(%r{must be a top-level tag}, error.message)
  end

  def test_block_must_be_a_top_level_tag
    error = assert_raises(Liquid::SyntaxError) do
      parse_template("{% if true %}{% block body %}x{% endblock %}{% endif %}")
    end
    assert_match(%r{must be a top-level tag}, error.message)
  end

  def test_extends_and_block_are_rejected_in_partials
    partials = {
      'snippet_extends' => "{% extends 'layout' %}",
      'snippet_block'   => "{% block body %}x{% endblock %}",
    }
    file_system = StubFileSystem.new(partials)

    error = assert_raises(Liquid::SyntaxError) do
      parse_template("{% render 'snippet_extends' %}").render!({}, registers: { file_system: file_system })
    end
    assert_match(%r{cannot be used inside a partial}, error.message)

    error = assert_raises(Liquid::SyntaxError) do
      parse_template("{% render 'snippet_block' %}").render!({}, registers: { file_system: file_system })
    end
    assert_match(%r{cannot be used inside a partial}, error.message)
  end

  def test_extends_requires_a_quoted_template_name
    error = assert_raises(Liquid::SyntaxError) do
      parse_template("{% extends layout %}")
    end
    assert_match(%r{Valid syntax: extends}, error.message)
  end

  def test_parent_is_loaded_through_the_controlled_file_system
    file_system = StubFileSystem.new('layout' => 'L{% block body %}d{% endblock %}')
    template = parse_template("{% extends 'layout' %}{% block body %}x{% endblock %}")

    assert_equal("Lx", template.render!({}, registers: { file_system: file_system }))
    assert_equal(1, file_system.file_read_count)

    error = assert_raises(Liquid::FileSystemError) do
      parse_template("{% extends 'missing' %}").render!({}, registers: { file_system: Liquid::BlankFileSystem.new })
    end
    assert_match(%r{does not allow includes}, error.message)
  end

  def test_tags_are_not_registered_unless_opted_in
    assert_raises(Liquid::SyntaxError) do
      Liquid::Template.parse("{% extends 'layout' %}")
    end
  end

  private

  def parse_template(source)
    Liquid::Template.parse(source, environment: @environment, line_numbers: true)
  end

  def render_source(source, partials = {}, assigns: {})
    parse_template(source).render!(assigns, registers: { file_system: StubFileSystem.new(partials) })
  end
end
