#!/usr/bin/env ruby
# frozen_string_literal: true

# Demonstrates Liquid::DependencyAnalyzer: static include/render dependency
# analysis over an entry template, without rendering anything.
#
# Run with: ruby example/dependency_analysis.rb

$LOAD_PATH.unshift(File.join(__dir__, "..", "lib"))
require "liquid"

# An in-memory file system mixing a shared partial, a reference cycle and a
# dynamic (unresolvable) template name.
PARTIALS = {
  # shared partial, referenced by both the entry template and "product_card"
  "price"        => "{{ product.price | money }}",
  "product_card" => <<~LIQUID,
    <h2>{{ product.title }}</h2>
    {% render 'price' %}
    {% render 'related' %}
  LIQUID
  # "related" and "product_card" reference each other: a cycle
  "related"      => <<~LIQUID,
    <aside>
      {% render 'product_card' %}
    </aside>
  LIQUID
}.freeze

class InMemoryFileSystem
  def initialize(templates)
    @templates = templates
  end

  def read_template_file(template_path)
    @templates.fetch(template_path) do
      raise Liquid::FileSystemError, "No such template '#{template_path}'"
    end
  end
end

ENTRY = <<~LIQUID
  <h1>{{ collection.title }}</h1>
  {% for product in collection.products %}
    {% render 'product_card' %}
    {% render 'price' %}
  {% endfor %}
  {% include section.settings.summary %}
LIQUID

graph = Liquid::DependencyAnalyzer
  .new(file_system: InMemoryFileSystem.new(PARTIALS))
  .analyze(ENTRY, name: "collection")

puts "Templates parsed (each partial parsed once):"
graph.templates.each { |name| puts "  #{name}" }

puts "\nReference graph (every edge with its source location):"
graph.references.each { |reference| puts "  #{reference}" }

puts "\nUnresolved references (dynamic template names):"
graph.unresolved_references.each do |reference|
  expression = reference.expression
  description = if expression.is_a?(Liquid::VariableLookup)
    ([expression.name] + expression.lookups).join(".")
  else
    expression.inspect
  end
  puts "  #{reference.from}:#{reference.line_number}: #{reference.tag} #{description}"
end

puts "\nCycles:"
if graph.cycle?
  graph.cycles.each do |cycle|
    puts "  #{cycle}"
    cycle.references.each { |reference| puts "    #{reference}" }
  end
else
  puts "  (none)"
end
