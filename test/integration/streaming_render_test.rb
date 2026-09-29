# frozen_string_literal: true

require 'test_helper'

class StreamingRenderTest < Minitest::Test
  include Liquid

  class CountingDrop < Liquid::Drop
    attr_reader :access_count

    def initialize(name)
      super()
      @name = name
      @access_count = 0
    end

    def value
      @access_count += 1
      @name
    end
  end

  class RaisingDrop < Liquid::Drop
    def boom
      raise 'boom'
    end
  end

  module ShoutFilter
    def shout(input)
      "#{input}!"
    end
  end

  def test_full_consumption_matches_render_output
    source = "{% assign v = 'x' | upcase %}{{ v }}|" \
      "{% for i in (1..3) %}[{{ i }}]{% endfor %}|" \
      "{% capture c %}inner {{ v }}{% endcapture %}{{ c }}|" \
      "{% render 'snippet', who: 'bob' %}"

    file_system = StubFileSystem.new('snippet' => 'hi {{ who }}')
    environment = Liquid::Environment.build(file_system: file_system)
    template = Liquid::Template.parse(source, environment: environment)

    chunks = template.render_each.to_a

    assert(chunks.all? { |chunk| chunk.is_a?(String) && !chunk.empty? })
    assert_equal(template.render, chunks.join)
  end

  def test_capture_buffers_locally_and_emits_nothing_at_capture_position
    template = Liquid::Template.parse("{% capture c %}hidden{% endcapture %}shown {{ c }}")

    assert_equal(['shown ', 'hidden'], template.render_each.to_a)
  end

  def test_enumerator_is_lazy_until_consumed
    drops = [CountingDrop.new('v1')]
    template = Liquid::Template.parse("{% for d in drops %}{{ d.value }}{% endfor %}")

    template.render_each('drops' => drops)

    assert_equal(0, drops.sum(&:access_count))
  end

  def test_early_stop_does_not_touch_remaining_drops
    drops = (1..10).map { |i| CountingDrop.new("v#{i}") }
    template = Liquid::Template.parse("{% for d in drops %}{{ d.value }}{% endfor %}")

    enum = template.render_each('drops' => drops)
    assert_equal('v1', enum.next)
    assert_equal(1, drops.sum(&:access_count))

    GC.start
    assert_equal(1, drops.sum(&:access_count))

    assert_equal('v2', enum.next)
    assert_equal(2, drops.sum(&:access_count))
  end

  def test_full_consumption_touches_every_drop_exactly_once
    drops = (1..10).map { |i| CountingDrop.new("v#{i}") }
    template = Liquid::Template.parse("{% for d in drops %}{{ d.value }}{% endfor %}")

    assert_equal('v1v2v3v4v5v6v7v8v9v10', template.render_each('drops' => drops).to_a.join)
    assert_equal(10, drops.sum(&:access_count))
  end

  def test_resource_limits_follow_consumption
    template = Liquid::Template.parse("{% for i in (1..10) %}0123456789{% endfor %}")
    template.resource_limits.render_length_limit = 25

    enum = template.render_each
    assert_equal('0123456789', enum.next)
    refute(template.resource_limits.reached?)

    assert_equal('0123456789', enum.next)
    assert_equal('0123456789', enum.next)
    assert_raises(StopIteration) { enum.next }
    assert(template.resource_limits.reached?)
    assert_kind_of(Liquid::MemoryError, template.errors.last)
  end

  def test_partial_consumption_never_hits_length_limit
    template = Liquid::Template.parse("{% for i in (1..10) %}0123456789{% endfor %}")
    template.resource_limits.render_length_limit = 25

    enum = template.render_each
    2.times { enum.next }

    refute(template.resource_limits.reached?)
    assert_empty(template.errors)
  end

  def test_exception_is_raised_at_consumption_position
    template = Liquid::Template.parse('A{{ err.boom }}B')
    enum = template.render_each!('err' => RaisingDrop.new)

    assert_equal('A', enum.next)
    error = assert_raises(RuntimeError) { enum.next }
    assert_equal('boom', error.message)
  end

  def test_errors_are_rendered_inline_by_default
    template = Liquid::Template.parse('A{{ err.boom }}B')
    assigns = { 'err' => RaisingDrop.new }

    chunks = template.render_each(assigns).to_a

    assert_equal(template.render(assigns), chunks.join)
    assert_equal(1, template.errors.size)
  end

  def test_options_are_passed_through
    template = Liquid::Template.parse("{{ 'hi' | shout }}")

    assert_equal(['hi!'], template.render_each({ 'unused' => 1 }, filters: [ShoutFilter]).to_a)
  end

  def test_render_each_without_assigns
    template = Liquid::Template.parse('static')

    assert_equal(['static'], template.render_each.to_a)
  end
end
