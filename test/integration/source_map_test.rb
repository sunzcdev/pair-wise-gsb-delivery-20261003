# frozen_string_literal: true

require 'test_helper'

class SourceMapTest < Minitest::Test
  include Liquid

  def render_with_source_map(source, assigns = {}, partials: {}, template_name: 'main', line_numbers: true)
    file_system = StubFileSystem.new(partials)
    environment = Liquid::Environment.build(file_system: file_system)
    template = Liquid::Template.parse(source, line_numbers: line_numbers, environment: environment)
    template.name = template_name
    context = Liquid::Context.build(static_environments: assigns, environment: environment)
    output = template.render(context, source_map: true)
    [output, template.source_map]
  end

  def byte_offset(haystack, needle)
    haystack.b.index(needle.b)
  end

  def test_no_source_map_by_default
    template = Liquid::Template.parse("hi {{ 1 }}")
    assert_equal('hi 1', template.render)
    assert_nil(template.source_map)
  end

  def test_output_is_unchanged_when_source_map_enabled
    source = "Hello {{ name }}!{% if show %} Yes{% endif %}{% for i in (1..2) %}{{ i }}{% endfor %}"
    assigns = { 'name' => 'World', 'show' => true }

    with_map, map = render_with_source_map(source, assigns)
    without_map = Liquid::Template.parse(source).render(assigns)

    assert_equal('Hello World! Yes12', with_map)
    assert_equal(without_map, with_map)
    assert(map.size > 0)
  end

  def test_source_map_instance_can_be_passed_to_render
    map = Liquid::SourceMap.new
    template = Liquid::Template.parse("{{ 'x' }}", line_numbers: true)
    output = template.render({}, source_map: map)

    assert_equal('x', output)
    assert_same(map, template.source_map)
    assert_equal(1, map.size)
  end

  def test_variable_entries_point_at_template_source
    output, map = render_with_source_map("Hello {{ name }}!\nBye {{ name }}\n", { 'name' => 'World' })
    assert_equal("Hello World!\nBye World\n", output)

    entry = map.entry_at(byte_offset(output, 'World'))
    assert_equal('main', entry.template_name)
    assert_equal(1, entry.line_number)
    assert_equal(7, entry.column)
    assert_equal('variable', entry.node_type)
    assert_empty(entry.stack)

    second = map.entry_at(output.b.rindex('World'.b))
    assert_equal(2, second.line_number)
    assert_equal(5, second.column)
  end

  def test_text_nodes_are_attributed_to_their_template
    output, map = render_with_source_map("Hello {{ name }}!", { 'name' => 'World' })

    entry = map.entry_at(0)
    assert_equal('main', entry.template_name)
    assert_equal('text', entry.node_type)
    assert_nil(entry.line_number)
    assert_nil(entry.column)
  end

  def test_columns_are_character_based_for_multibyte_templates
    output, map = render_with_source_map("亲爱的{{ name }}，您好", { 'name' => 'World' })
    assert_equal('亲爱的World，您好', output)

    entry = map.entry_at('亲爱的'.bytesize)
    assert_equal('variable', entry.node_type)
    assert_equal(1, entry.line_number)
    assert_equal(4, entry.column) # 亲=1 爱=2 的=3, {{ starts at column 4
  end

  def test_byte_offsets_inside_multibyte_characters
    output, map = render_with_source_map("亲爱的{{ name }}", { 'name' => 'World' })

    # the second byte of the 3-byte character 亲 still belongs to the text node
    entry = map.entry_at(1)
    assert_equal('text', entry.node_type)
    assert_equal('main', entry.template_name)
    assert_equal(0...'亲爱的'.bytesize, entry.byte_range)
  end

  def test_escaped_output_is_attributed_to_the_variable
    output, map = render_with_source_map("{{ html | escape }}", { 'html' => '<b>hi</b>' })
    assert_equal('&lt;b&gt;hi&lt;/b&gt;', output)

    entry = map.entry_at(byte_offset(output, '&lt;'))
    assert_equal('variable', entry.node_type)
    assert_equal(1, entry.line_number)
    assert_equal(1, entry.column)
    # the whole escaped (and expanded) output belongs to the variable node
    assert_equal(0...output.bytesize, entry.byte_range)
  end

  def test_filter_expanded_output_is_attributed_to_the_variable
    output, map = render_with_source_map("{{ name | upcase | append: '!!!' }}", { 'name' => 'ab' })
    assert_equal('AB!!!', output)

    entry = map.entry_at(4) # the last '!'
    assert_equal('variable', entry.node_type)
    assert_equal(0...5, entry.byte_range)
  end

  def test_loop_iterations_are_recorded_separately
    output, map = render_with_source_map("{% for x in xs %}[{{ x }}]{% endfor %}", { 'xs' => [1, 2, 3] })
    assert_equal('[1][2][3]', output)

    variable_entries = map.entries_in(0...output.bytesize).select { |e| e.node_type == 'variable' }
    assert_equal(3, variable_entries.length)
    assert_equal([1...2, 4...5, 7...8], variable_entries.map(&:byte_range))
    assert_equal([1, 1, 1], variable_entries.map(&:line_number))

    # the {% for %} tag itself covers the whole loop output
    covering = map.entries_at(4)
    assert_equal('variable', covering.first.node_type)
    assert_equal('for', covering.last.node_type)
    assert_equal(0...9, covering.last.byte_range)
  end

  def test_nested_render_records_template_names_and_call_stack
    partials = {
      'item' => "{{ product.title }}",
      'outer' => "<div>{% render 'item', product: product %}</div>",
    }
    output, map = render_with_source_map(
      "A{% render 'outer', product: product %}Z",
      { 'product' => { 'title' => 'Boots' } },
      partials: partials,
    )
    assert_equal('A<div>Boots</div>Z', output)

    entry = map.entry_at(byte_offset(output, 'Boots'))
    assert_equal('item', entry.template_name)
    assert_equal(1, entry.line_number)
    assert_equal(1, entry.column)

    assert_equal(2, entry.stack.length)
    assert_equal('main', entry.stack[0].template_name)
    assert_equal(1, entry.stack[0].line_number)
    assert_equal(2, entry.stack[0].column)
    assert_equal('outer', entry.stack[1].template_name)
    assert_equal(1, entry.stack[1].line_number)
    assert_equal(6, entry.stack[1].column)

    # enclosing render tags cover the same bytes, innermost first
    chain = map.entries_at(byte_offset(output, 'Boots'))
    assert_equal(['item', 'outer', 'main'], chain.map(&:template_name))
    assert_equal(['variable', 'render', 'render'], chain.map(&:node_type))
  end

  def test_include_records_call_stack
    partials = { 'greeting' => "Hi {{ name }}" }
    output, map = render_with_source_map("{% include 'greeting' %}", { 'name' => 'Bob' }, partials: partials)
    assert_equal('Hi Bob', output)

    entry = map.entry_at(byte_offset(output, 'Bob'))
    assert_equal('greeting', entry.template_name)
    assert_equal(1, entry.stack.length)
    assert_equal('main', entry.stack[0].template_name)
    assert_equal(1, entry.stack[0].line_number)
    assert_equal(1, entry.stack[0].column)
  end

  def test_render_for_loop_records_each_iteration
    partials = { 'item' => "({{ item }})" }
    output, map = render_with_source_map(
      "{% render 'item' for items as item %}",
      { 'items' => [1, 2] },
      partials: partials,
    )
    assert_equal('(1)(2)', output)

    entries = map.entries_in(0...output.bytesize).select { |e| e.node_type == 'variable' }
    assert_equal([1...2, 4...5], entries.map(&:byte_range))
    assert_equal(['item', 'item'], entries.map(&:template_name))
    assert_equal(1, entries.first.stack.length)
    assert_equal('main', entries.first.stack[0].template_name)
  end

  def test_empty_output_entries_are_recorded
    output, map = render_with_source_map("{% assign x = 1 %}{{ nothing }}done")
    assert_equal('done', output)

    empty_entries = map.entries.select(&:empty?)
    assert(empty_entries.length >= 2)

    assign_entry = empty_entries.find { |e| e.node_type == 'assign' }
    assert_equal(1, assign_entry.line_number)
    assert_equal(0, assign_entry.start_byte)

    nil_variable = empty_entries.find { |e| e.node_type == 'variable' }
    assert_equal(1, nil_variable.line_number)
    assert_equal(0, nil_variable.start_byte)

    # empty entries are discoverable inside a byte range covering their position
    assert(map.entries_in(0...1).count(&:empty?) >= 2)
  end

  def test_drop_generated_values_are_attributed
    output, map = render_with_source_map(
      "{{ settings.title }}|{{ drop }}",
      {
        'settings' => SettingsDrop.new('title' => 'Hi'),
        'drop' => StringDrop.new('hey'),
      },
    )
    assert_equal('Hi|hey', output)

    title_entry = map.entry_at(0)
    assert_equal('variable', title_entry.node_type)
    assert_equal(0...2, title_entry.byte_range)

    drop_entry = map.entry_at(3)
    assert_equal('variable', drop_entry.node_type)
    assert_equal(3...6, drop_entry.byte_range)
  end

  def test_capture_body_is_not_recorded_until_output
    output, map = render_with_source_map("{% capture c %}in-{{ 1 }}{% endcapture %}[{{ c }}]")
    assert_equal('[in-1]', output)

    # the captured content is attributed to the {{ c }} variable, not to the
    # capture body (which rendered into a side buffer)
    entry = map.entry_at(byte_offset(output, 'in-1'))
    assert_equal('variable', entry.node_type)
    assert_equal(1, entry.line_number)
  end

  def test_liquid_tag_nodes_have_line_and_column
    output, map = render_with_source_map("{% liquid\n  assign x = 1\n  echo x\n%}")
    assert_equal('1', output)

    entry = map.entry_at(0)
    assert_equal('echo', entry.node_type)
    assert_equal(3, entry.line_number)
    assert_equal(1, entry.column)
  end

  def test_source_map_without_line_numbers
    output, map = render_with_source_map("Hi {{ name }}", { 'name' => 'Bob' }, line_numbers: false)
    assert_equal('Hi Bob', output)

    entry = map.entry_at(3)
    assert_equal('variable', entry.node_type)
    assert_equal('main', entry.template_name)
    assert_nil(entry.line_number)
    assert_nil(entry.column)
  end

  def test_entries_in_returns_only_overlapping_entries
    output, map = render_with_source_map("ab{{ x }}cd", { 'x' => 'Y' })
    assert_equal('abYcd', output)

    entries = map.entries_in(2...3)
    assert_equal(['variable'], entries.map(&:node_type))

    assert_empty(map.entries_in(5...10))
  end

  def test_source_map_does_not_copy_rendered_data
    secret = 'S3cr3t-Value'
    output, map = render_with_source_map(
      "{% render 'snippet' %}",
      { 'secret' => secret },
      partials: { 'snippet' => '{{ secret }}' },
    )
    assert_equal(secret, output)

    map.entries.each do |entry|
      refute_includes(entry.inspect, secret)
      refute_includes(entry.to_h.to_s, secret)
    end
  end
end
