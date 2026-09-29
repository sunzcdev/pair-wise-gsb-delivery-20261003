# frozen_string_literal: true

module Liquid
  # Statically analyzes a template and, transitively, every partial it pulls in
  # through `include` / `render` tags with a static (string literal) name.
  #
  # The result is a {DependencyGraph} describing:
  #
  # * every resolved reference edge, with the source location (template name
  #   and line number) of the tag that created it,
  # * every unresolved reference (`include` / `render` with a dynamic,
  #   non-literal template name),
  # * every cycle in the reference graph, as a locatable chain of templates.
  #
  # Each partial is parsed only once, no matter how many times it is
  # referenced; every reference location is still recorded.
  #
  # Analysis is parse-only: templates are never rendered, so no filters are
  # executed and no Drops are read.
  #
  # Example:
  #
  #   analyzer = Liquid::DependencyAnalyzer.new(
  #     file_system: Liquid::LocalFileSystem.new("templates"),
  #   )
  #   graph = analyzer.analyze(source, name: "product")
  #   graph.references             # all resolved include/render edges
  #   graph.unresolved_references  # edges with dynamic template names
  #   graph.cycles                 # reference cycles, with locations
  #
  class DependencyAnalyzer
    # A single resolved `include` / `render` edge in the dependency graph.
    Reference = Struct.new(:from, :to, :tag, :line_number, keyword_init: true) do
      def to_s
        "#{from}:#{line_number}: #{tag} '#{to}'"
      end
    end

    # An `include` / `render` tag whose template name is not a string literal
    # and therefore cannot be resolved statically.
    UnresolvedReference = Struct.new(:from, :tag, :line_number, :expression, keyword_init: true) do
      def to_s
        "#{from}:#{line_number}: #{tag} <dynamic>"
      end
    end

    # A cycle in the dependency graph: the chain of template names forming the
    # loop (first name repeated at the end), plus the references — with source
    # locations — along the chain.
    class Cycle
      attr_reader :templates, :references

      def initialize(templates, references)
        @templates  = templates
        @references = references
      end

      def to_s
        @templates.join(" -> ")
      end
    end

    # The dependency graph produced by {DependencyAnalyzer#analyze}.
    class DependencyGraph
      # Name of the entry template the analysis started from.
      attr_reader :entry

      # Names of every template that was parsed, entry first.
      attr_reader :templates

      # All resolved reference edges ({Reference}), in discovery order. A
      # partial referenced more than once appears once per reference.
      attr_reader :references

      # All references with a dynamic template name ({UnresolvedReference}).
      attr_reader :unresolved_references

      # Names of partials that were referenced but could not be read from the
      # file system.
      attr_reader :missing_templates

      def initialize(entry:, templates:, references:, unresolved_references:, missing_templates:)
        @entry                 = entry
        @templates             = templates
        @references            = references
        @unresolved_references = unresolved_references
        @missing_templates     = missing_templates
      end

      # Names of the partials directly referenced by +name+, in first-use
      # order, without duplicates.
      def dependencies_of(name)
        references_from(name).map(&:to).uniq
      end

      # All resolved reference edges originating from +name+.
      def references_from(name)
        @references.select { |reference| reference.from == name }
      end

      # All resolved reference edges pointing at +name+.
      def references_to(name)
        @references.select { |reference| reference.to == name }
      end

      def cycle?
        !cycles.empty?
      end

      # Every cycle in the graph as a {Cycle} whose +templates+ chain locates
      # the loop (e.g. <tt>["a", "b", "a"]</tt>) and whose +references+ carry
      # the source location of each edge along the chain.
      def cycles
        @cycles ||= compute_cycles
      end

      private

      def compute_cycles
        adjacency = {}
        @references.each do |reference|
          targets = (adjacency[reference.from] ||= [])
          targets << reference.to unless targets.include?(reference.to)
        end

        color  = Hash.new(:white)
        stack  = []
        cycles = []
        seen   = {}

        visit = lambda do |node|
          color[node] = :gray
          stack << node
          (adjacency[node] || Const::EMPTY_ARRAY).each do |target|
            case color[target]
            when :white
              visit.call(target)
            when :gray
              chain = stack[stack.index(target)..] + [target]
              body  = chain[0..-2]
              # normalize rotation so each cycle is only reported once
              signature = body.rotate(body.each_index.min_by { |i| body[i] }).join("\0")
              unless seen[signature]
                seen[signature] = true
                cycles << build_cycle(chain)
              end
            end
          end
          stack.pop
          color[node] = :black
        end

        adjacency.each_key { |node| visit.call(node) if color[node] == :white }
        cycles
      end

      def build_cycle(chain)
        references = chain.each_cons(2).map do |from, to|
          @references.find { |reference| reference.from == from && reference.to == to }
        end
        Cycle.new(chain, references)
      end
    end

    def initialize(file_system: nil, environment: Environment.default)
      @environment = environment
      @file_system = file_system || environment.file_system
    end

    # Builds the dependency graph for +source+, following static
    # `include` / `render` references through the file system.
    #
    # When +source+ is nil, the entry template itself is read from the file
    # system using +name+.
    def analyze(source = nil, name: nil)
      name ||= "(entry)"

      templates             = []
      references            = []
      unresolved_references = []
      missing_templates     = []

      sources = { name => source.nil? ? read_partial(name) : source }
      parsed  = {}
      queue   = [name]

      until queue.empty?
        template_name = queue.shift
        next if parsed[template_name]

        parsed[template_name] = true

        template_source = sources[template_name]
        if template_source.nil?
          missing_templates << template_name
          next
        end

        templates << template_name

        each_reference_tag(parse(template_source)) do |node|
          expression = node.template_name_expr

          if expression.is_a?(String)
            references << Reference.new(
              from: template_name,
              to: expression,
              tag: node.tag_name,
              line_number: node.line_number,
            )
            unless sources.key?(expression)
              sources[expression] = read_partial(expression)
              queue << expression
            end
          else
            unresolved_references << UnresolvedReference.new(
              from: template_name,
              tag: node.tag_name,
              line_number: node.line_number,
              expression: expression,
            )
          end
        end
      end

      DependencyGraph.new(
        entry: name,
        templates: templates,
        references: references,
        unresolved_references: unresolved_references,
        missing_templates: missing_templates,
      )
    end

    private

    def parse(source)
      Template.new(environment: @environment).parse(source, line_numbers: true).root
    end

    def read_partial(name)
      @file_system.read_template_file(name)
    rescue FileSystemError
      nil
    end

    def each_reference_tag(node, &block)
      return unless node.respond_to?(:nodelist)

      Array(node.nodelist).each do |child|
        yield child if child.is_a?(Include) || child.is_a?(Render)
        each_reference_tag(child, &block)
      end
    end
  end
end
