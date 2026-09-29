# frozen_string_literal: true

module Liquid
  # Statically analyzes the include/render dependency graph of a template.
  #
  # Starting from an entry template, every `include`/`render` tag with a
  # literal (quoted) name is expanded by reading the referenced partial from
  # the file system. Tags with a dynamic (non-literal) name cannot be
  # resolved statically and are reported as unresolved references.
  #
  # The analysis is parse-only: templates are tokenized and parsed, but never
  # rendered. No filters are executed and no Drops or assigns are read.
  #
  # Each referenced partial is parsed only once, no matter how many times it
  # is referenced, while every reference (with its source line number) is
  # kept. Circular reference chains are detected and reported with the
  # template path and the source positions of the involved tags.
  #
  # Example:
  #
  #   report = Liquid::DependencyAnalyzer.analyze(
  #     source,
  #     file_system: Liquid::LocalFileSystem.new("snippets"),
  #   )
  #   report.references            # => [#<Reference from="(root)" to="header" line_number=3 ...>, ...]
  #   report.unresolved_references # => dynamic includes/renders
  #   report.cycles                # => [#<Cycle path=["a", "b", "a"] ...>]
  #
  class DependencyAnalyzer
    # A static edge in the dependency graph: template `from` references
    # partial `to` via an include/render tag at `line_number` in `from`.
    Reference = Struct.new(:from, :to, :tag, :line_number, keyword_init: true) do
      def to_h
        { from: from, to: to, tag: tag, line_number: line_number }
      end
    end

    # An include/render tag whose partial name is not a string literal and
    # therefore cannot be resolved statically.
    UnresolvedReference = Struct.new(:from, :tag, :line_number, :markup, keyword_init: true) do
      def to_h
        { from: from, tag: tag, line_number: line_number, markup: markup }
      end
    end

    # A circular chain of references. `path` lists the template names in
    # reference order, repeating the first name at the end (e.g.
    # ["a", "b", "a"]). `references` holds the edges forming the cycle,
    # each carrying the source position of its tag.
    Cycle = Struct.new(:path, :references, keyword_init: true) do
      def to_h
        { path: path, references: references.map(&:to_h) }
      end
    end

    # The result of a dependency analysis.
    class Report
      attr_reader :root, :templates, :references, :unresolved_references, :cycles

      def initialize(root:, templates:, references:, unresolved_references:, cycles:)
        @root                  = root
        @templates             = templates
        @references            = references
        @unresolved_references = unresolved_references
        @cycles                = cycles
      end

      # Names of partials directly referenced by +name+ (static edges only).
      def dependencies_of(name)
        @references.select { |reference| reference.from == name }.map(&:to).uniq
      end

      # Names of templates that directly reference +name+.
      def dependents_of(name)
        @references.select { |reference| reference.to == name }.map(&:from).uniq
      end

      def cyclic?
        !@cycles.empty?
      end

      def to_h
        {
          root: @root,
          templates: @templates,
          references: @references.map(&:to_h),
          unresolved_references: @unresolved_references.map(&:to_h),
          cycles: @cycles.map(&:to_h),
        }
      end
    end

    DEFAULT_ROOT_NAME = "(root)"

    def self.analyze(source, file_system: nil, environment: Environment.default, template_name: DEFAULT_ROOT_NAME)
      new(file_system: file_system, environment: environment).analyze(source, template_name: template_name)
    end

    def initialize(file_system: nil, environment: Environment.default)
      @environment = environment
      @file_system = file_system || environment.file_system
    end

    def analyze(source, template_name: DEFAULT_ROOT_NAME)
      @templates             = []
      @references            = []
      @unresolved_references = []
      @cycles                = []
      @parsed                = {}
      @visiting              = []

      expand(template_name, source)

      Report.new(
        root: template_name,
        templates: @templates,
        references: @references,
        unresolved_references: @unresolved_references,
        cycles: @cycles,
      )
    end

    private

    def expand(name, source)
      @parsed[name] = true
      @templates << name
      @visiting.push(name)

      each_tag(parse_template(name, source).root) do |node|
        next unless node.is_a?(Liquid::Include) || node.is_a?(Liquid::Render)

        template_name = node.template_name_expr
        if template_name.is_a?(String)
          record_reference(name, node, template_name)
        else
          @unresolved_references << UnresolvedReference.new(
            from: name,
            tag: node.tag_name.to_sym,
            line_number: node.line_number,
            markup: node.raw.strip,
          )
        end
      end
    ensure
      @visiting.pop
    end

    def record_reference(from, node, target)
      reference = Reference.new(
        from: from,
        to: target,
        tag: node.tag_name.to_sym,
        line_number: node.line_number,
      )
      @references << reference

      if (cycle_start = @visiting.index(target))
        record_cycle(@visiting[cycle_start..] + [target])
      elsif !@parsed.key?(target)
        expand(target, @file_system.read_template_file(target))
      end
    end

    def record_cycle(path)
      return if @cycles.any? { |cycle| cycle.path == path }

      edges = path.each_cons(2).map do |from, to|
        @references.find { |reference| reference.from == from && reference.to == to }
      end
      @cycles << Cycle.new(path: path, references: edges)
    end

    def parse_template(name, source)
      template = Liquid::Template.new(environment: @environment)
      template.name = name
      template.parse(source, line_numbers: true, environment: @environment)
      template
    end

    def each_tag(node, &block)
      return unless node.respond_to?(:nodelist)

      Array(node.nodelist).each do |child|
        yield child
        each_tag(child, &block)
      end
    end
  end
end
