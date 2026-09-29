# frozen_string_literal: true

require 'test_helper'

class DependencyAnalyzerTest < Minitest::Test
  include Liquid

  def test_static_references_are_expanded_with_positions
    report = analyze(
      "{% render 'header' %}\nbody\n{% include 'footer' %}\n",
      { 'header' => "header\n", 'footer' => "footer\n" },
    )

    assert_equal(['(root)', 'header', 'footer'], report.templates)
    assert_equal(
      [
        { from: '(root)', to: 'header', tag: :render, line_number: 1 },
        { from: '(root)', to: 'footer', tag: :include, line_number: 3 },
      ],
      report.references.map(&:to_h),
    )
    assert_empty(report.unresolved_references)
    assert_empty(report.cycles)
    refute(report.cyclic?)
  end

  def test_nested_partials_are_expanded_transitively
    report = analyze(
      "{% render 'a' %}\n",
      { 'a' => "{% render 'b' %}\n", 'b' => "leaf\n" },
    )

    assert_equal(['(root)', 'a', 'b'], report.templates)
    assert_equal(['a'], report.dependencies_of('(root)'))
    assert_equal(['b'], report.dependencies_of('a'))
    assert_equal(['a'], report.dependents_of('b'))
  end

  def test_shared_partial_is_parsed_once_but_keeps_all_reference_positions
    file_system = StubFileSystem.new('shared' => "shared\n")
    report = analyze(
      "{% render 'shared' %}\n{% include 'shared' %}\n{% render 'shared' %}\n",
      {},
      file_system: file_system,
    )

    assert_equal(1, file_system.file_read_count)
    assert_equal(['(root)', 'shared'], report.templates)
    assert_equal([1, 2, 3], report.references.map(&:line_number))
    assert_equal([:render, :include, :render], report.references.map(&:tag))
  end

  def test_dynamic_names_are_reported_as_unresolved
    report = analyze(
      "{% include dynamic_name %}\n{% render 'static' %}\n",
      { 'static' => "static\n" },
    )

    assert_equal(1, report.unresolved_references.size)
    unresolved = report.unresolved_references.first
    assert_equal('(root)', unresolved.from)
    assert_equal(:include, unresolved.tag)
    assert_equal(1, unresolved.line_number)
    assert_equal('include dynamic_name', unresolved.markup)
    assert_equal(['static'], report.dependencies_of('(root)'))
  end

  def test_cycles_are_detected_and_locatable
    report = analyze(
      "{% render 'a' %}\n",
      { 'a' => "one\n{% render 'b' %}\n", 'b' => "two\n{% render 'a' %}\n" },
    )

    assert(report.cyclic?)
    assert_equal(1, report.cycles.size)

    cycle = report.cycles.first
    assert_equal(['a', 'b', 'a'], cycle.path)
    assert_equal(
      [
        { from: 'a', to: 'b', tag: :render, line_number: 2 },
        { from: 'b', to: 'a', tag: :render, line_number: 2 },
      ],
      cycle.references.map(&:to_h),
    )
  end

  def test_self_referencing_template_reports_cycle
    report = analyze("{% render 'loop' %}\n", { 'loop' => "{% include 'loop' %}\n" })

    assert(report.cyclic?)
    assert_equal(['loop', 'loop'], report.cycles.first.path)
  end

  def test_analysis_does_not_render_or_evaluate_anything
    environment = Environment.build
    environment.register_filter(Module.new do
      def boom(_input)
        raise 'filter must not run during analysis'
      end
    end)

    report = DependencyAnalyzer.analyze(
      "{{ 'x' | boom }}\n{% render 'partial', with: product %}\n",
      file_system: StubFileSystem.new('partial' => "{{ 'y' | boom }}\n"),
      environment: environment,
    )

    assert_equal(['partial'], report.dependencies_of('(root)'))
  end

  def test_mixed_shared_cyclic_and_dynamic_template
    file_system = StubFileSystem.new(
      'shared' => "shared body\n",
      'cycle_a' => "{% render 'cycle_b' %}\n",
      'cycle_b' => "{% include 'cycle_a' %}\n{% render 'shared' %}\n",
    )

    report = DependencyAnalyzer.analyze(<<~LIQUID, file_system: file_system)
      {% render 'shared' %}
      {% include 'shared' %}
      {% render 'cycle_a' %}
      {% include section_partial %}
    LIQUID

    assert_equal(['(root)', 'shared', 'cycle_a', 'cycle_b'], report.templates)
    assert_equal(3, file_system.file_read_count) # each partial parsed exactly once

    assert_equal(
      [
        { from: '(root)', to: 'shared', tag: :render, line_number: 1 },
        { from: '(root)', to: 'shared', tag: :include, line_number: 2 },
        { from: '(root)', to: 'cycle_a', tag: :render, line_number: 3 },
        { from: 'cycle_a', to: 'cycle_b', tag: :render, line_number: 1 },
        { from: 'cycle_b', to: 'cycle_a', tag: :include, line_number: 1 },
        { from: 'cycle_b', to: 'shared', tag: :render, line_number: 2 },
      ],
      report.references.map(&:to_h),
    )

    assert_equal(['cycle_a', 'cycle_b', 'cycle_a'], report.cycles.first.path)

    assert_equal(1, report.unresolved_references.size)
    assert_equal(4, report.unresolved_references.first.line_number)
    assert_equal('include section_partial', report.unresolved_references.first.markup)
  end

  private

  def analyze(source, partials, file_system: nil)
    DependencyAnalyzer.analyze(
      source,
      file_system: file_system || StubFileSystem.new(partials),
    )
  end
end
