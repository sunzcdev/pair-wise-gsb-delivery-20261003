# frozen_string_literal: true

# Demonstrates Liquid::DependencyAnalyzer with a template that mixes
# shared partials, a circular reference chain and a dynamic include.
#
# Run with: ruby -Ilib example/dependency_analysis.rb

require "liquid"

class InMemoryFileSystem
  def initialize(partials)
    @partials = partials
  end

  def read_template_file(template_path)
    @partials.fetch(template_path)
  end
end

entry_template = <<~LIQUID
  {% render 'shared' %}
  {% include 'shared' %}
  {% render 'cycle_a' %}
  {% include section_partial %}
LIQUID

file_system = InMemoryFileSystem.new(
  "shared"  => "shared body\n",
  "cycle_a" => "{% render 'cycle_b' %}\n",
  "cycle_b" => "{% include 'cycle_a' %}\n{% render 'shared' %}\n",
)

report = Liquid::DependencyAnalyzer.analyze(entry_template, file_system: file_system)

puts "Templates (#{report.templates.size}): #{report.templates.join(', ')}"
puts
puts "References:"
report.references.each do |ref|
  puts "  #{ref.from}:#{ref.line_number} --#{ref.tag}--> #{ref.to}"
end
puts
puts "Unresolved (dynamic) references:"
report.unresolved_references.each do |ref|
  puts "  #{ref.from}:#{ref.line_number} #{ref.markup}"
end
puts
puts "Cycles:"
report.cycles.each do |cycle|
  puts "  #{cycle.path.join(' -> ')}"
  cycle.references.each do |ref|
    puts "    #{ref.from}:#{ref.line_number} --#{ref.tag}--> #{ref.to}"
  end
end
