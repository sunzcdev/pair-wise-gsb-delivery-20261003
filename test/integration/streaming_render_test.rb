# frozen_string_literal: true

require 'test_helper'

class StreamingRenderTest < Minitest::Test
  include Liquid

  class CountingDrop < Liquid::Drop
    attr_reader :accesses

    def initialize
      super
      @accesses = 0
    end

    def first_value
      @accesses += 1
      'first'
    end

    def second_value
      @accesses += 1
      'second'
    end

    def third_value
      @accesses += 1
      'third'
    end
  end

  class RaisingDrop < Liquid::Drop
    def good
      'good'
    end

    def bad
      raise 'boom'
    end
  end

  def test_streaming_full_consumption_matches_render
    source = <<~LIQUID.chomp
      {%- assign x = 41 | plus: 1 -%}
      [{{ x }}]
      {%- for i in (1..3) -%}
      {{ i | times: 2 }}
      {%- endfor -%}
      {%- if x == 42 %} yes{%- endif -%}
    LIQUID
    template = Template.parse(source)

    assert_equal(template.render, template.render_each.to_a.join)
  end

  def test_render_tag_streams_with_same_semantics
    file_system = StubFileSystem.new('snippet' => "partial:{{ thing }}")
    environment = Liquid::Environment.build(file_system: file_system)
    template   = Template.parse("a{% render 'snippet', thing: 'x' %}b", environment: environment)

    assert_equal(template.render, template.render_each.to_a.join)
    assert_equal('apartial:xb', template.render_each.to_a.join)
  end

  def test_capture_buffers_locally_and_emits_no_inner_chunks
    template = Template.parse("{% capture greeting %}Hello {{ 'World' | upcase }}{% endcapture %}[{{ greeting }}]")

    assert_equal(['[', 'Hello WORLD', ']'], template.render_each.to_a)
  end

  def test_full_consumption_accesses_everything
    drop     = CountingDrop.new
    template = Template.parse("a{{ data.first_value }}b{{ data.second_value }}c{{ data.third_value }}d")

    assert_equal('afirstbsecondcthirdd', template.render_each('data' => drop).to_a.join)
    assert_equal(3, drop.accesses)
  end

  def test_taking_first_chunks_only_renders_those_chunks
    drop     = CountingDrop.new
    template = Template.parse("a{{ data.first_value }}b{{ data.second_value }}c{{ data.third_value }}d")

    assert_equal(['a', 'first', 'b'], template.render_each('data' => drop).first(3))
    assert_equal(1, drop.accesses)
  end

  def test_no_drop_access_in_background_after_stopping
    drop     = CountingDrop.new
    template = Template.parse("a{{ data.first_value }}b{{ data.second_value }}c{{ data.third_value }}d")

    enum = template.render_each('data' => drop)
    assert_equal('a', enum.next)
    assert_equal(0, drop.accesses)
    assert_equal('first', enum.next)
    assert_equal(1, drop.accesses)

    # the consumer stopped; nothing may keep rendering in the background
    3.times { assert_equal(1, drop.accesses) }
  end

  def test_block_form_stops_rendering_on_break
    drop     = CountingDrop.new
    template = Template.parse("a{{ data.first_value }}b{{ data.second_value }}c")
    chunks   = []

    template.render_each('data' => drop) do |chunk|
      chunks << chunk
      break
    end

    assert_equal(['a'], chunks)
    assert_equal(0, drop.accesses)
    assert_equal([], template.errors)
  end

  def test_exception_is_raised_at_the_consumption_position
    template = Template.parse("a{{ data.good }}b{{ data.bad }}c{{ data.good }}d")
    enum     = template.render_each!('data' => RaisingDrop.new)

    assert_equal('a', enum.next)
    assert_equal('good', enum.next)
    assert_equal('b', enum.next)
    error = assert_raises(RuntimeError) { enum.next }
    assert_equal('boom', error.message)
  end

  def test_exception_is_raised_from_block_form_at_its_position
    template = Template.parse("a{{ data.good }}b{{ data.bad }}c")
    chunks   = []

    error = assert_raises(RuntimeError) do
      template.render_each!('data' => RaisingDrop.new) { |chunk| chunks << chunk }
    end
    assert_equal('boom', error.message)
    assert_equal(['a', 'good', 'b'], chunks)
  end

  def test_handled_error_is_delivered_as_chunk_at_its_position
    template = Template.parse("a{{ data.good }}b{{ data.bad }}c")
    chunks   = template.render_each('data' => RaisingDrop.new).to_a

    assert_equal(['a', 'good', 'b'], chunks.first(3))
    assert_match(/Liquid error/, chunks[3])
    assert_equal('c', chunks[4])
    assert_equal(1, template.errors.size)
  end

  def test_resource_limits_track_only_what_was_consumed
    template = Template.parse("{% for i in (1..10) %}{{ i }}{% endfor %}")

    full_context = Liquid::Context.build
    template.render_each(full_context).to_a
    full_score = full_context.resource_limits.render_score

    partial_context = Liquid::Context.build
    template.render_each(partial_context).first(2)
    partial_score = partial_context.resource_limits.render_score

    assert(full_score > 0)
    assert(partial_score < full_score)
  end

  def test_render_length_limit_interrupts_the_stream
    environment = Liquid::Environment.build do |env|
      env.default_resource_limits = { render_length_limit: 5 }
    end
    template = Template.parse("0123456789abcdef", environment: environment)
    chunks   = template.render_each.to_a

    assert_equal('0123456789abcdef', chunks.first)
    assert_match(/Memory limits exceeded/, chunks.last)
    assert_equal(1, template.errors.size)
    assert_instance_of(Liquid::MemoryError, template.errors.first)
  end

  def test_render_each_without_block_returns_lazy_enumerator
    drop     = CountingDrop.new
    template = Template.parse("{{ data.first_value }}")
    enum     = template.render_each('data' => drop)

    assert_instance_of(Enumerator, enum)
    assert_equal(0, drop.accesses)
    assert_equal(['first'], enum.to_a)
    assert_equal(1, drop.accesses)
  end

  def test_unparsed_template_streams_nothing
    assert_equal([], Template.new.render_each.to_a)
  end
end
