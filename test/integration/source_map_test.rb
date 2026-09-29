# frozen_string_literal: true

require 'test_helper'

class SourceMapTest < Minitest::Test
  class SourceMapFileSystem
    def initialize(partials)
      @partials = partials
    end

    def read_template_file(template_path)
      @partials.fetch(template_path)
    end
  end

  class GreetingDrop < Liquid::Drop
    def initialize(name)
      super()
      @name = name
    end

    def to_s
      "Hello, #{@name}!"
    end
  end

  def parse_template(source, partials: {}, **options)
    environment = Liquid::Environment.build(file_system: SourceMapFileSystem.new(partials))
    template = Liquid::Template.parse(source, source_map: true, environment: environment, **options)
    template.name = 'main'
    template
  end

  def test_output_is_unchanged_when_source_map_enabled
    source = "Hello {{ name }}! {% if show %}yes{% endif %}"
    with_map = parse_template(source).render('name' => 'World', 'show' => true)
    without_map = Liquid::Template.parse(source).render('name' => 'World', 'show' => true)
    assert_equal('Hello World! yes', with_map)
    assert_equal(without_map, with_map)
  end

  def test_text_and_variable_entries_tile_the_output
    template = parse_template("Hello {{ name }}!")
    output = template.render('name' => 'World')
    map = template.source_map

    assert_equal(output.bytesize, map.byte_length)

    text = map.lookup(0)
    assert_equal([0, 6], [text.start_byte, text.end_byte])
    assert_equal(:text, text.node_type)
    assert_equal('main', text.template_name)
    assert_equal(1, text.line_number)
    assert_equal(1, text.column)

    variable = map.lookup(6)
    assert_equal([6, 11], [variable.start_byte, variable.end_byte])
    assert_equal(:variable, variable.node_type)
    assert_equal(1, variable.line_number)
    assert_equal(7, variable.column) # "{{" starts at byte offset 6

    assert_equal(:text, map.lookup(11).node_type)
  end

  def test_multiline_positions
    template = parse_template("line one\nline {{ two }}\nline three")
    output = template.render('two' => '2')
    map = template.source_map

    entry = map.lookup(output.index('2'))
    assert_equal(:variable, entry.node_type)
    assert_equal(2, entry.line_number)
    assert_equal(6, entry.column)
  end

  def test_utf8_boundary_bytes
    template = parse_template("你好，{{ name }}！")
    output = template.render('name' => '世界')
    assert_equal("你好，世界！", output)
    map = template.source_map

    # "你好，" is 9 bytes, so the variable output starts at byte 9
    variable = map.lookup(9)
    assert_equal(:variable, variable.node_type)
    assert_equal([9, 15], [variable.start_byte, variable.end_byte])
    assert_equal(10, variable.column) # "{{" starts at byte offset 9

    # every byte inside a multi-byte character maps to the same entry
    assert_equal(:text, map.lookup(0).node_type)
    assert_equal(:text, map.lookup(8).node_type)
    9.upto(14) do |byte|
      assert_equal(variable, map.lookup(byte), "byte #{byte} should belong to the variable")
    end
    assert_equal(:text, map.lookup(15).node_type)
    assert_equal(map.byte_length, output.bytesize)
  end

  def test_filter_expanded_output_is_attributed_to_the_variable
    template = parse_template("{{ html | escape }}")
    output = template.render('html' => '<b>你好</b>')
    assert_equal('&lt;b&gt;你好&lt;/b&gt;', output)
    map = template.source_map

    # the escaped output is much longer than the input, but still belongs
    # to the single variable node
    entry = map.lookup(0)
    assert_equal(:variable, entry.node_type)
    assert_equal(0, entry.start_byte)
    assert_equal(output.bytesize, entry.end_byte)
    assert_equal(entry, map.lookup(output.bytesize - 1))
  end

  def test_loop_iterations_are_recorded_separately
    template = parse_template("{% for i in (1..3) %}[{{ i }}]{% endfor %}")
    output = template.render
    assert_equal("[1][2][3]", output)
    map = template.source_map

    variables = map.entries.select { |entry| entry.node_type == :variable }
    assert_equal(3, variables.size)
    assert_equal([[1, 2], [4, 5], [7, 8]], variables.map { |entry| [entry.start_byte, entry.end_byte] })
    assert_equal(['1', '2', '3'], variables.map { |entry| output.byteslice(entry.start_byte...entry.end_byte) })
  end

  def test_nested_render_call_stack
    partials = {
      'outer' => "A{% render 'inner' %}B",
      'inner' => "C",
    }
    template = parse_template("M{% render 'outer' %}N", partials: partials)
    output = template.render
    assert_equal("MACBN", output)
    map = template.source_map

    inner = map.lookup(output.index('C'))
    assert_equal('inner', inner.template_name)
    assert_equal(1, inner.line_number)
    assert_equal(2, inner.stack.size)
    assert_equal(['outer', 1], [inner.stack[0].template_name, inner.stack[0].line_number])
    assert_equal(['inner', 1], [inner.stack[1].template_name, inner.stack[1].line_number])

    outer = map.lookup(output.index('A'))
    assert_equal('outer', outer.template_name)
    assert_equal(1, outer.stack.size)
    assert_equal('outer', outer.stack[0].template_name)

    root = map.lookup(output.index('M'))
    assert_equal('main', root.template_name)
    assert_empty(root.stack)
    assert_equal('main', map.root_template_name)
  end

  def test_render_for_loop_records_each_partial_render
    partials = { 'row' => "({{ row }})" }
    template = parse_template("{% render 'row' for items as row %}", partials: partials)
    output = template.render('items' => [1, 2])
    assert_equal("(1)(2)", output)
    map = template.source_map

    rows = map.entries.select { |entry| entry.template_name == 'row' && entry.node_type == :variable }
    assert_equal(2, rows.size)
    assert_equal([[1, 2], [4, 5]], rows.map { |entry| [entry.start_byte, entry.end_byte] })
    assert(rows.all? { |entry| entry.stack.size == 1 && entry.stack[0].template_name == 'row' })
  end

  def test_include_tag_pushes_frame
    partials = { 'inc' => "I" }
    template = parse_template("{% include 'inc' %}", partials: partials)
    output = template.render
    assert_equal("I", output)

    entry = template.source_map.lookup(0)
    assert_equal('inc', entry.template_name)
    assert_equal(1, entry.stack.size)
    assert_equal(['inc', 1], [entry.stack[0].template_name, entry.stack[0].line_number])
  end

  def test_empty_output_still_records_zero_length_entries
    template = parse_template("a{{ missing }}b")
    output = template.render
    assert_equal("ab", output)
    map = template.source_map

    empty = map.entries.select(&:empty?)
    assert_equal(1, empty.size)
    assert_equal([1, 1], [empty[0].start_byte, empty[0].end_byte])
    assert_equal(:variable, empty[0].node_type)

    # lookup prefers the non-empty entry at a boundary, entries_at exposes both
    assert_equal(:text, map.lookup(1).node_type)
    assert_equal(2, map.entries_at(1).size)
  end

  def test_drop_output_is_attributed_to_the_variable
    template = parse_template("{{ greeting }}")
    output = template.render('greeting' => GreetingDrop.new('世界'))
    assert_equal("Hello, 世界!", output)
    map = template.source_map

    entry = map.lookup(0)
    assert_equal(:variable, entry.node_type)
    assert_equal(output.bytesize, entry.end_byte)
    assert_equal(entry, map.lookup(output.bytesize - 1))
  end

  def test_map_does_not_copy_rendered_business_data
    secret = "敏感数据-very-secret"
    template = parse_template("{{ secret }}")
    template.render('secret' => secret)
    map = template.source_map

    stored = map.entries.flat_map(&:to_a).join
    refute_includes(stored, secret)
    assert_equal(map.byte_length, secret.bytesize)
  end

  def test_query_returns_entries_overlapping_a_byte_range
    template = parse_template("ab{{ x }}cd{{ y }}ef")
    output = template.render('x' => '1', 'y' => '2')
    assert_equal("ab1cd2ef", output)
    map = template.source_map

    entries = map.query(1, 6) # covers "b1cd2"
    assert_equal([0, 2, 3, 5], entries.map(&:start_byte))
    assert_equal([:text, :variable, :text, :variable], entries.map(&:node_type))
  end

  def test_source_map_can_be_enabled_at_render_time
    template = Liquid::Template.parse("a{{ b }}")
    template.name = 'main'
    output = template.render({ 'b' => 'x' }, source_map: true)
    assert_equal("ax", output)

    map = template.source_map
    assert_equal(2, map.entries.size)
    assert_equal(:variable, map.lookup(1).node_type)
  end

  def test_source_map_is_nil_when_not_enabled
    template = Liquid::Template.parse("a{{ b }}")
    template.render('b' => 'x')
    assert_nil(template.source_map)
  end

  def test_chinese_escape_and_nested_render_boundary_bytes
    partials = { 'snippet' => "<em>{{ title | escape }}</em>" }
    template = parse_template("前缀{% render 'snippet', title: title %}后缀", partials: partials)
    output = template.render('title' => '<中文>')
    assert_equal("前缀<em>&lt;中文&gt;</em>后缀", output)
    map = template.source_map

    # "前缀" is 6 bytes: snippet output starts at byte 6
    assert_equal('main', map.lookup(5).template_name)

    em_open = map.lookup(6)
    assert_equal('snippet', em_open.template_name)
    assert_equal(:text, em_open.node_type)
    assert_equal(1, em_open.stack.size)
    assert_equal(['snippet', 1], [em_open.stack[0].template_name, em_open.stack[0].line_number])

    # escaped variable: "&lt;中文&gt;" is 14 bytes, from byte 10 to 24
    variable = map.lookup(10)
    assert_equal(:variable, variable.node_type)
    assert_equal('snippet', variable.template_name)
    assert_equal([10, 24], [variable.start_byte, variable.end_byte])
    assert_equal(5, variable.column) # "<em>" is 4 bytes, "{{" starts at byte offset 4
    assert_equal(variable, map.lookup(23))

    assert_equal(:text, map.lookup(24).node_type) # "</em>"

    # "后缀" starts right after the snippet output at byte 29
    suffix = map.lookup(29)
    assert_equal('main', suffix.template_name)
    assert_equal(:text, suffix.node_type)
    assert_equal(suffix, map.lookup(output.bytesize - 1))

    assert_equal(output.bytesize, map.byte_length)
  end
end
