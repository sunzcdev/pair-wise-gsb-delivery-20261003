# frozen_string_literal: true

require 'test_helper'

class StaticAnalysisTest < Minitest::Test
  def analyze(source, partials: nil)
    Liquid::StaticAnalysis.analyze(source, partials: partials)
  end

  def paths(reads)
    reads.map(&:to_s)
  end

  def test_simple_external_read
    result = analyze("{{ user.name }}")
    assert_equal(["user.name"], paths(result.required))
    assert_empty(result.optional)
    assert_empty(result.internal)
  end

  def test_assign_binds_name_for_later_reads
    result = analyze("{% assign user = 'me' %}{{ user.name }}")
    assert_empty(result.required)
    assert_equal([:assign], result.internal['user'])
  end

  def test_read_before_assign_is_external
    result = analyze("{{ user.name }}{% assign user = 'me' %}")
    assert_equal(["user.name"], paths(result.required))
    assert_equal([:assign], result.internal['user'])
  end

  def test_capture_binds_name
    result = analyze("{% capture greeting %}Hello {{ name }}{% endcapture %}{{ greeting }}")
    assert_equal(["name"], paths(result.required))
    assert_equal([:capture], result.internal['greeting'])
  end

  def test_for_loop_variable_is_not_an_external_input
    result = analyze("{% for product in products %}{{ product.title }} {{ forloop.index }}{% endfor %}")
    assert_equal(["products"], paths(result.required))
    assert_equal([:for], result.internal['product'])
    assert_equal([:for], result.internal['forloop'])
  end

  def test_for_loop_variable_does_not_leak_out_of_the_loop
    result = analyze("{% for product in products %}{% endfor %}{{ product.title }}")
    assert_equal(["product.title", "products"], paths(result.required))
  end

  def test_nested_loops_have_independent_bindings
    result = analyze(<<~LIQUID)
      {% for section in sections %}
        {% for item in section.items %}{{ item.title }} {{ section.title }}{% endfor %}
        {{ item.title }}
      {% endfor %}
    LIQUID
    assert_equal(["sections"], paths(result.required))
    assert_equal(["item.title"], paths(result.optional))
  end

  def test_for_else_block_reads_are_optional_and_unscoped
    result = analyze("{% for item in items %}{{ item }}{% else %}{{ item }} {{ fallback }}{% endfor %}")
    assert_equal(["items"], paths(result.required))
    assert_equal(["fallback", "item"], paths(result.optional))
  end

  def test_for_limit_offset_and_range_are_read
    result = analyze("{% for i in (first..last) limit: max offset: start %}{{ i }}{% endfor %}")
    assert_equal(["first", "last", "max", "start"], paths(result.required))
    assert_equal([:for], result.internal['i'])
  end

  def test_reads_inside_if_branches_are_optional
    result = analyze("{% if user %}{{ user.name }}{% elsif admin %}{{ admin.mail }}{% else %}{{ guest }}{% endif %}")
    assert_equal(["admin", "user"], paths(result.required))
    assert_equal(["admin.mail", "guest", "user.name"], paths(result.optional))
  end

  def test_reads_inside_unless_and_case_are_optional
    result = analyze(<<~LIQUID)
      {% unless hidden %}{{ secret }}{% endunless %}
      {% case state %}{% when 'a' %}{{ alpha }}{% when other %}{{ beta }}{% else %}{{ gamma }}{% endcase %}
    LIQUID
    assert_equal(["hidden", "other", "state"], paths(result.required))
    assert_equal(["alpha", "beta", "gamma", "secret"], paths(result.optional))
  end

  def test_required_read_dominates_optional_read
    result = analyze("{{ user.name }}{% if flag %}{{ user.name }}{% endif %}")
    assert_equal(["flag", "user.name"], paths(result.required))
    assert_empty(result.optional)
  end

  def test_dynamic_subscript_yields_bounded_unknown_path
    result = analyze("{{ items[key].title }}")
    assert_equal(["items.*", "key"], paths(result.required))
    assert(result.required.find { |path| path.root == 'items' }.dynamic?)
  end

  def test_static_subscripts_are_fixed_fields
    result = analyze("{{ items['title'] }} {{ items[0] }} {{ items.first }}")
    assert_equal(["items.0", "items.first", "items.title"], paths(result.required))
  end

  def test_dynamic_subscript_on_internal_variable_is_not_external
    result = analyze("{% for item in items %}{{ item[key] }}{% endfor %}")
    assert_equal(["items"], paths(result.required))
    assert_equal(["key"], paths(result.optional))
  end

  def test_filter_arguments_are_read
    result = analyze("{{ price | money: currency, locale: shop.locale }}")
    assert_equal(["currency", "price", "shop.locale"], paths(result.required))
  end

  def test_tablerow_binds_loop_variable
    result = analyze("{% tablerow cell in grid cols: cols %}{{ cell }} {{ tablerowloop.row }}{% endtablerow %}")
    assert_equal(["cols", "grid"], paths(result.required))
    assert_equal([:tablerow], result.internal['cell'])
    assert_equal([:tablerow], result.internal['tablerowloop'])
  end

  def test_increment_and_decrement_are_internal_counters
    result = analyze("{% increment counter %}{% decrement counter %}{{ counter }}")
    assert_empty(result.required)
    assert_equal([:decrement, :increment], result.internal['counter'])
  end

  def test_cycle_reads_its_expressions
    result = analyze("{% cycle odd, 'even' %}")
    assert_equal(["odd"], paths(result.required))
  end

  def test_comment_raw_and_doc_contribute_no_reads
    result = analyze(<<~LIQUID)
      {% comment %}{{ hidden }}{% endcomment %}
      {% raw %}{{ hidden }}{% endraw %}
      {% doc %}{{ hidden }}{% enddoc %}
    LIQUID
    assert_empty(result.required)
    assert_empty(result.optional)
  end

  def test_include_shares_the_outer_scope
    partials = { 'message' => "{{ greeting }}, {{ name }}" }
    result = analyze("{% assign greeting = 'hi' %}{% include 'message' %}", partials: partials)
    assert_equal(["message", "name"], paths(result.required))
    assert_empty(result.optional)
  end

  def test_include_binds_alias_and_attributes_for_the_partial
    partials = { 'item' => "{{ entry.title }} {{ size }}" }
    result = analyze("{% include 'item' with product as entry, size: 'm' %}", partials: partials)
    assert_equal(["product"], paths(result.required))
    assert_equal([:include], result.internal['entry'])
    assert_equal([:include], result.internal['size'])
  end

  def test_include_bindings_do_not_leak_back
    partials = { 'item' => "{% assign temp = 1 %}" }
    result = analyze("{% include 'item' %}{{ temp }}", partials: partials)
    assert_equal(["item", "temp"], paths(result.required))
  end

  def test_render_isolates_the_partial_scope
    partials = { 'card' => "{{ greeting }} {{ product.title }} {{ shop }}" }
    result = analyze(<<~LIQUID, partials: partials)
      {% assign greeting = 'hi' %}
      {% render 'card', product: product %}
    LIQUID
    # greeting is assigned in the outer template but NOT visible inside the
    # rendered partial, so the partial reads it from the global environment.
    assert_equal(["greeting", "product", "shop"], paths(result.required))
    assert_equal([:assign], result.internal['greeting'])
    assert_equal([:render], result.internal['product'])
  end

  def test_render_for_loop_binds_alias_and_forloop
    partials = { 'row' => "{{ entry }} {{ forloop.index }}" }
    result = analyze("{% render 'row' for rows as entry %}", partials: partials)
    assert_equal(["rows"], paths(result.required))
    assert_equal([:render], result.internal['entry'])
    assert_equal([:render], result.internal['forloop'])
  end

  def test_unresolvable_partials_are_reported_not_rendered
    result = analyze("{% include 'missing' %}{% render 'absent' %}{% include dynamic_name %}")
    assert_equal(["dynamic_name", "missing"], paths(result.required))
    assert_equal(
      [
        { 'name' => 'missing', 'tag' => 'include', 'reason' => 'missing_partial' },
        { 'name' => 'absent', 'tag' => 'render', 'reason' => 'missing_partial' },
        { 'name' => nil, 'tag' => 'include', 'reason' => 'dynamic_name' },
      ],
      result.unresolved_partials.map(&:to_h),
    )
  end

  def test_recursive_partials_do_not_loop_forever
    partials = { 'loop' => "{% include 'loop' %}{{ value }}" }
    result = analyze("{% include 'loop' %}", partials: partials)
    assert_equal(["loop", "value"], paths(result.required))
    assert_equal('recursive', result.unresolved_partials.last.reason.to_s)
  end

  def test_shadowing_pair_explains_different_inputs
    # The two templates differ only in whether the loop variable shadows the
    # outer `user` name, and the analysis explains why they need different
    # inputs.
    shadowed = analyze("{% for user in users %}{{ user.name }}{% endfor %}")
    assert_equal(["users"], paths(shadowed.required))
    assert_equal([:for], shadowed.internal['user'])

    unshadowed = analyze("{% for member in users %}{{ user.name }}{% endfor %}")
    assert_equal(["users"], paths(unshadowed.required))
    assert_equal(["user.name"], paths(unshadowed.optional))
    refute(unshadowed.internal?('user'))
  end

  def test_analysis_is_purely_syntactic_and_never_renders
    # strict_variables would raise on `missing` at render time; analysis only
    # parses and walks the syntax tree.
    template = Liquid::Template.parse("{{ missing }}", error_mode: :strict)
    result = Liquid::StaticAnalysis.analyze(template)
    assert_equal(["missing"], paths(result.required))
  end

  def test_to_h_returns_string_keyed_serializable_result
    result = analyze("{% assign a = b %}{% if c %}{{ d }}{% endif %}")
    assert_equal(
      {
        'required' => ["b", "c"],
        'optional' => ["d"],
        'internal' => { 'a' => ['assign'] },
        'unresolved_partials' => [],
      },
      result.to_h,
    )
  end
end
