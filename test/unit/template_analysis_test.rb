# frozen_string_literal: true

require 'test_helper'

class TemplateAnalysisTest < Minitest::Test
  def analyze(source, partials: {})
    Liquid::TemplateAnalysis.analyze(Liquid::Template.parse(source), partials: partials)
  end

  def test_simple_output_is_an_external_read
    result = analyze("{{ product.title }}")

    assert_equal(["product.title"], result.external_variables)
    assert_empty(result.optional_variables)
    assert_empty(result.internal_assignments)
  end

  def test_for_loop_variable_is_internal_not_external
    result = analyze("{% for item in products %}{{ item.title }} {{ forloop.index }}{% endfor %}")

    assert_equal(["products"], result.external_variables)
    assert_empty(result.optional_variables)
    assert_empty(result.reads.select { |read| read.path.first == "item" || read.path.first == "forloop" })
  end

  def test_nested_loops_shadow_outer_loop_variable
    result = analyze(<<~LIQUID)
      {% for x in outer %}
        {% for x in inner %}{{ x.deep }}{% endfor %}
        {{ x.shallow }}
      {% endfor %}
      {{ x }}
    LIQUID

    assert_equal(["outer", "x"], result.external_variables)
    assert_equal(["inner"], result.optional_variables)
  end

  def test_shadowing_difference_changes_result
    read_before_assign = analyze("{{ x }}{% assign x = 'a' %}")
    assign_before_read = analyze("{% assign x = 'a' %}{{ x }}")

    assert_equal(["x"], read_before_assign.external_variables)
    assert_empty(assign_before_read.external_variables)
    assert_equal({ "x" => :unconditional }, assign_before_read.internal_assignments)
  end

  def test_capture_defines_variable
    result = analyze("{% capture c %}{{ value }}{% endcapture %}{{ c }}")

    assert_equal(["value"], result.external_variables)
    assert_equal({ "c" => :unconditional }, result.internal_assignments)
  end

  def test_increment_and_decrement_are_internal
    result = analyze("{% increment counter %}{% decrement counter %}{{ counter }}")

    assert_empty(result.external_variables)
    assert_equal({ "counter" => :unconditional }, result.internal_assignments)
  end

  def test_conditional_branch_reads_are_optional_but_condition_is_required
    result = analyze("{% if user %}{{ user.name }}{% elsif admin %}{{ admin.mail }}{% else %}{{ fallback }}{% endif %}")

    assert_equal(["user", "admin"], result.external_variables)
    assert_equal(["user.name", "admin.mail", "fallback"], result.optional_variables)
  end

  def test_unless_and_case_branches_are_optional
    result = analyze(<<~LIQUID)
      {% unless hidden %}{{ shown }}{% endunless %}
      {% case state %}{% when 'a' %}{{ alpha }}{% when other %}{{ beta }}{% else %}{{ gamma }}{% endcase %}
    LIQUID

    assert_equal(["hidden", "state", "other"], result.external_variables)
    assert_equal(["shown", "alpha", "beta", "gamma"], result.optional_variables)
  end

  def test_conditional_assignment_makes_later_read_optional
    result = analyze("{% if flag %}{% assign x = 1 %}{% endif %}{{ x }}")

    assert_equal(["flag"], result.external_variables)
    assert_equal(["x"], result.optional_variables)
    assert_equal({ "x" => :conditional }, result.internal_assignments)
  end

  def test_dynamic_subscript_yields_constrained_unknown_path
    result = analyze("{{ products[handle].title }}")

    assert_equal(["handle", "products.*.title"], result.external_variables)
    assert_equal(["products.*.title"], result.dynamic_paths)
    refute_includes(result.external_variables, "products.handle.title")
  end

  def test_static_subscript_is_a_fixed_path
    result = analyze("{{ products['featured'].title }} {{ list[0] }}")

    assert_equal(["products.featured.title", "list.0"], result.external_variables)
    assert_empty(result.dynamic_paths)
  end

  def test_filter_arguments_and_ranges_are_reads
    result = analyze("{{ x | default: fallback }}{% for i in (1..n) %}{{ i }}{% endfor %}")

    assert_equal(["x", "fallback", "n"], result.external_variables)
    assert_empty(result.optional_variables)
  end

  def test_tablerow_binds_loop_variable
    result = analyze("{% tablerow cell in grid cols: cols %}{{ cell }} {{ tablerowloop.col }}{% endtablerow %}")

    assert_equal(["grid", "cols"], result.external_variables)
    assert_empty(result.optional_variables)
  end

  def test_reads_of_external_roots_inside_loops_are_optional
    result = analyze("{% for item in products %}{{ user.name }}{% endfor %}")

    assert_equal(["products"], result.external_variables)
    assert_equal(["user.name"], result.optional_variables)
  end

  def test_cycle_reads_its_expressions
    result = analyze("{% cycle a, 'b' %}")

    assert_equal(["a"], result.external_variables)
  end

  def test_include_shares_caller_context
    result = analyze(
      "{% assign local = 1 %}{% include 'snippet', extra: local %}",
      partials: { "snippet" => "{{ local }} {{ global }} {{ extra }} {{ snippet }}" },
    )

    assert_equal(["snippet", "global"], result.external_variables)
    assert_equal({ "local" => :unconditional }, result.internal_assignments)
    assert(result.partial_references.first.resolved?)
    assert_equal(:include, result.partial_references.first.type)
  end

  def test_plain_include_reads_variable_named_after_partial
    result = analyze("{% include 'product' %}", partials: { "product" => "{{ product.title }}" })

    assert_equal(["product"], result.external_variables)
    assert_empty(result.optional_variables)
  end

  def test_render_has_isolated_parameter_scope
    result = analyze(
      "{% assign local = 1 %}{% render 'snippet', passed: local %}",
      partials: { "snippet" => "{{ local }} {{ passed }} {% assign inner = 1 %}" },
    )

    assert_equal(["local"], result.external_variables)
    assert_equal({ "local" => :unconditional }, result.internal_assignments)
  end

  def test_render_with_alias_binds_parameter
    result = analyze(
      "{% render 'item' with products as entry %}",
      partials: { "item" => "{{ entry.title }} {{ products }}" },
    )

    assert_equal(["products"], result.external_variables)
    assert_empty(result.optional_variables)
  end

  def test_render_for_marks_partial_reads_optional
    result = analyze(
      "{% render 'item' for products as entry %}",
      partials: { "item" => "{{ entry.title }} {{ global }}" },
    )

    assert_equal(["products"], result.external_variables)
    assert_equal(["global"], result.optional_variables)
  end

  def test_unresolved_partial_is_reported_not_raised
    result = analyze("{% render 'missing' %}{% include dynamic_name %}")

    assert_equal(["dynamic_name"], result.external_variables)
    references = result.partial_references
    assert_equal(2, references.size)
    assert(references.none?(&:resolved?))
    assert_equal("missing", references.first.name)
    assert_nil(references.last.name)
  end

  def test_recursive_partials_do_not_loop_forever
    result = analyze("{% include 'self' %}", partials: { "self" => "{{ outer }}{% include 'self' %}" })

    assert_equal(["self", "outer"], result.external_variables)
  end

  def test_result_does_not_depend_on_rendering
    template = Liquid::Template.parse("{% assign x = 1 %}{{ x }}")
    result = Liquid::TemplateAnalysis.analyze(template)

    assert_empty(result.external_variables)
    assert_equal("1", template.render)
  end
end
