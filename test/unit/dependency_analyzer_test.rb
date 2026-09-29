# frozen_string_literal: true

require 'test_helper'

class DependencyAnalyzerTest < Minitest::Test
  class InMemoryFileSystem
    attr_reader :reads

    def initialize(templates)
      @templates = templates
      @reads     = Hash.new(0)
    end

    def read_template_file(template_path)
      @reads[template_path] += 1
      @templates.fetch(template_path) do
        raise Liquid::FileSystemError, "No such template '#{template_path}'"
      end
    end
  end

  def test_builds_graph_with_source_locations
    file_system = InMemoryFileSystem.new(
      "card" => "{{ product.title }}",
    )

    graph = analyze(<<~LIQUID, file_system: file_system)
      <h1>{{ product.title }}</h1>
      {% render 'card' %}
      {% if product.available %}
        {% include 'card' %}
      {% endif %}
    LIQUID

    assert_equal(["(entry)", "card"], graph.templates)
    assert_equal(["card"], graph.dependencies_of("(entry)"))

    render_ref, include_ref = graph.references
    assert_equal("(entry)", render_ref.from)
    assert_equal("card", render_ref.to)
    assert_equal("render", render_ref.tag)
    assert_equal(2, render_ref.line_number)

    assert_equal("include", include_ref.tag)
    assert_equal(4, include_ref.line_number)

    # the shared partial was parsed only once, but both locations are kept
    assert_equal(1, file_system.reads["card"])
    assert_equal(2, graph.references_to("card").size)
  end

  def test_dynamic_names_are_reported_as_unresolved
    graph = analyze(<<~LIQUID)
      {% include snippet_name %}
      {% render 'static' %}
    LIQUID

    assert_equal(1, graph.unresolved_references.size)
    unresolved = graph.unresolved_references.first
    assert_equal("(entry)", unresolved.from)
    assert_equal("include", unresolved.tag)
    assert_equal(1, unresolved.line_number)
    assert_kind_of(Liquid::VariableLookup, unresolved.expression)

    assert_equal(["static"], graph.dependencies_of("(entry)"))
  end

  def test_cycles_are_locatable
    file_system = InMemoryFileSystem.new(
      "alpha" => "{% render 'beta' %}",
      "beta"  => "one\n{% render 'alpha' %}\n{% render 'beta' %}",
    )

    graph = analyze("{% render 'alpha' %}", file_system: file_system)

    assert(graph.cycle?)

    cycles = graph.cycles.sort_by { |cycle| cycle.templates.length }
    assert_equal(["beta", "beta"], cycles[0].templates)
    assert_equal(["alpha", "beta", "alpha"], cycles[1].templates)

    alpha_to_beta = cycles[1].references.first
    assert_equal("alpha", alpha_to_beta.from)
    assert_equal("beta", alpha_to_beta.to)
    assert_equal(1, alpha_to_beta.line_number)

    beta_to_alpha = cycles[1].references.last
    assert_equal("beta", beta_to_alpha.from)
    assert_equal("alpha", beta_to_alpha.to)
    assert_equal(2, beta_to_alpha.line_number)
  end

  def test_missing_partials_are_recorded
    graph = analyze("{% render 'ghost' %}", file_system: InMemoryFileSystem.new({}))

    assert_equal(["ghost"], graph.missing_templates)
    assert_equal(["ghost"], graph.dependencies_of("(entry)"))
  end

  def test_entry_template_can_come_from_the_file_system
    file_system = InMemoryFileSystem.new(
      "product" => "{% render 'price' %}",
      "price"   => "{{ product.price }}",
    )

    graph = analyzer(file_system: file_system).analyze(name: "product")

    assert_equal("product", graph.entry)
    assert_equal(["product", "price"], graph.templates)
  end

  def test_analysis_does_not_render
    environment = Liquid::Environment.build(error_mode: :strict) do |env|
      env.register_filter(Module.new do
        def self.explode(_input)
          raise "filter executed during analysis"
        end
      end)
    end

    graph = analyzer(file_system: InMemoryFileSystem.new({}), environment: environment)
      .analyze("{{ 'x' | explode }} {% render 'ghost' %}")

    assert_equal(["ghost"], graph.missing_templates)
  end

  private

  def analyzer(file_system: nil, environment: Liquid::Environment.default)
    Liquid::DependencyAnalyzer.new(
      file_system: file_system || InMemoryFileSystem.new({}),
      environment: environment,
    )
  end

  def analyze(source, file_system: InMemoryFileSystem.new({}))
    analyzer(file_system: file_system).analyze(source)
  end
end
