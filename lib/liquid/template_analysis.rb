# frozen_string_literal: true

module Liquid
  # Static, syntax-only analysis of a template's input dependencies.
  #
  # The analysis walks the parse tree (no rendering is performed) and
  # classifies every variable lookup as either:
  #
  # * an external read - the template needs this value from its render
  #   context (the "inputs" of the template),
  # * an internal read - the name is bound by the template itself
  #   (`assign`, `capture`, `increment`/`decrement`, `for`/`tablerow`
  #   loop variables, `forloop`/`tablerowloop` drops, `include`/`render`
  #   parameters) before it is read.
  #
  # Reads that only happen inside conditional branches (`if`/`unless`/
  # `case` bodies, `for`/`tablerow` bodies which may run zero times, and
  # `for`-style `include`/`render` partials) are reported as optional.
  #
  # Dynamic subscripts such as `products[handle].title` are never guessed
  # into a fixed field. They produce a constrained unknown path
  # (`products.*.title`) and the subscript expression (`handle`) is
  # reported as a read of its own.
  #
  # `include` shares the caller's context, so an included partial may use
  # names the caller assigned. `render` uses an isolated scope, so a
  # rendered partial only sees the explicitly passed parameters plus the
  # global (external) environment.
  #
  # Example:
  #
  #   result = Liquid::TemplateAnalysis.analyze(Liquid::Template.parse(source))
  #   result.external_variables   # => ["products"]
  #   result.optional_variables   # => ["user.name"]
  #   result.internal_assignments # => { "item" => :unconditional }
  #
  class TemplateAnalysis
    # Sentinel used inside Read#path for a segment that cannot be known
    # statically (dynamic subscript). Rendered as "*" in Read#to_s.
    DYNAMIC = :*

    # A single read of a variable path from the render context.
    class Read
      attr_reader :path

      def initialize(path, optional:)
        @path     = path.freeze
        @optional = optional
      end

      # True when the read only happens on some execution paths
      # (inside a conditional branch or a possibly-empty loop).
      def optional?
        @optional
      end

      # True when the path contains a dynamic subscript segment.
      def dynamic?
        @path.include?(DYNAMIC)
      end

      def to_s
        @path.map { |segment| segment == DYNAMIC ? "*" : segment }.join(".")
      end

      def ==(other)
        self.class == other.class && @path == other.path && @optional == other.optional?
      end
      alias_method :eql?, :==

      def hash
        [@path, @optional].hash
      end
    end

    # A reference to a partial via `include` or `render`.
    class PartialReference
      attr_reader :type, :name, :attributes, :alias_name

      def initialize(type:, name:, attributes:, alias_name:, for_loop:, resolved:)
        @type       = type
        @name       = name
        @attributes = attributes
        @alias_name = alias_name
        @for_loop   = for_loop
        @resolved   = resolved
      end

      def for_loop?
        @for_loop
      end

      # False when the partial source was not provided (or the partial
      # name is a dynamic expression) and its body could not be analyzed.
      def resolved?
        @resolved
      end
    end

    # The outcome of analyzing a template.
    class Result
      # All external reads (Read objects, de-duplicated).
      attr_reader :reads

      # Names bound by the analyzed template itself, mapped to
      # :unconditional or :conditional (bound only inside a branch).
      attr_reader :internal_assignments

      # PartialReference entries for every include/render tag.
      attr_reader :partial_references

      def initialize(reads:, internal_assignments:, partial_references:)
        @reads                = reads
        @internal_assignments = internal_assignments
        @partial_references   = partial_references
      end

      # Path strings the template always needs from its render context.
      def external_variables
        reads.reject(&:optional?).map(&:to_s).uniq
      end

      # Path strings read only inside conditional branches.
      def optional_variables
        reads.select(&:optional?).map(&:to_s).uniq
      end

      # Path strings containing a dynamic subscript segment ("*").
      def dynamic_paths
        reads.select(&:dynamic?).map(&:to_s).uniq
      end
    end

    # Lexical scope chain mirroring Liquid's context scopes. Each frame
    # maps a name to :unconditional or :conditional.
    class Scope
      def initialize(parent = nil)
        @parent = parent
        @defs   = {}
      end

      def define(name, conditional:)
        return if conditional && @defs[name] == :unconditional

        @defs[name] = conditional ? :conditional : :unconditional
      end

      # Returns :unconditional, :conditional, or nil when unbound.
      def lookup(name)
        scope = self
        found = nil
        while scope
          binding_kind = scope.defs[name]
          return :unconditional if binding_kind == :unconditional

          found ||= binding_kind
          scope = scope.parent
        end
        found
      end

      protected

      attr_reader :parent, :defs
    end

    class << self
      # Analyzes a parsed Liquid::Template (or a source string, which is
      # parsed first). `partials` optionally maps partial names to source
      # strings or Liquid::Template objects so include/render targets can
      # be analyzed as well.
      def analyze(template_or_source, partials: {})
        new(partials: partials).analyze(template_or_source)
      end
    end

    def initialize(partials: {})
      @partials           = partials
      @reads              = []
      @seen_reads         = {}
      @assignments        = {}
      @partial_references = []
      @partial_stack      = []
      @conditional_depth  = 0
      @record_assignments = true
    end

    def analyze(template_or_source)
      template = template_or_source.is_a?(Template) ? template_or_source : Template.parse(template_or_source)
      @environment = template.root&.parse_context&.environment
      @scope = Scope.new
      visit(template.root)
      Result.new(
        reads: @reads,
        internal_assignments: @assignments,
        partial_references: @partial_references,
      )
    end

    private

    def visit(node)
      case node
      when nil, String
        # literal text, nothing to do
      when Document
        visit_all(node.nodelist)
      when BlockBody
        visit_all(node.nodelist)
      when Variable
        visit_variable(node)
      when Assign
        visit_variable(node.from)
        define(node.to)
      when Capture
        visit_all(node.nodelist)
        define(node.to)
      when Increment, Decrement
        define(node.variable_name)
      when For
        visit_for(node)
      when TableRow
        visit_table_row(node)
      when If
        visit_conditional(node.blocks)
      when Case
        record_expression(node.left)
        visit_conditional(node.blocks)
      when Cycle
        node.variables.each { |var| record_expression(var) }
      when Echo
        visit_variable(node.variable)
      when Include
        visit_partial_tag(node, shared: true)
      when Render
        visit_partial_tag(node, shared: false)
      else
        # Conservative fallback for other nodes (custom tags, ifchanged,
        # break/continue, raw, ...): walk any child nodes they expose.
        visit_all(node.nodelist) if node.respond_to?(:nodelist) && node.nodelist
      end
    end

    def visit_all(nodes)
      nodes.each { |child| visit(child) }
    end

    def visit_variable(variable)
      record_expression(variable.name)
      variable.filters.each do |_filter_name, args, kwargs|
        args.each { |arg| record_expression(arg) }
        kwargs&.each_value { |value| record_expression(value) }
      end
    end

    def visit_for(node)
      record_expression(node.collection_name)
      record_expression(node.limit) if node.limit
      record_expression(node.from) if node.from && node.from != :continue

      with_scope do
        # Loop variable and the forloop drop are bound for the whole body.
        define_local(node.variable_name)
        define_local("forloop")
        with_conditional { visit_all(node.nodelist) }
      end
    end

    def visit_table_row(node)
      record_expression(node.collection_name)
      node.attributes.each_value { |value| record_expression(value) }

      with_scope do
        define_local(node.variable_name)
        define_local("tablerowloop")
        with_conditional { visit_all(node.nodelist) }
      end
    end

    def visit_conditional(conditions)
      conditions.each do |condition|
        record_condition(condition) unless condition.else?
        with_conditional { visit(condition.attachment) }
      end
    end

    def record_condition(condition)
      current = condition
      while current
        record_expression(current.left)
        record_expression(current.right) if current.operator
        current = current.child_condition
      end
    end

    def visit_partial_tag(node, shared:)
      template_name_expr = node.template_name_expr
      record_expression(template_name_expr) unless template_name_expr.is_a?(String)
      record_expression(node.variable_name_expr) if node.variable_name_expr
      node.attributes.each_value { |value| record_expression(value) }

      name         = template_name_expr.is_a?(String) ? template_name_expr : nil
      alias_name   = node.alias_name
      context_name = alias_name || (name && name.split("/").last)
      for_loop     = node.for_loop?

      # A plain `include 'x'` (no with/for) reads the outer variable
      # named after the partial and passes it along.
      record_read_segments([context_name]) if shared && !node.variable_name_expr && context_name

      partial = name && !@partial_stack.include?(name) ? load_partial(name) : nil
      @partial_references << PartialReference.new(
        type: shared ? :include : :render,
        name: name,
        attributes: node.attributes.keys,
        alias_name: alias_name,
        for_loop: for_loop,
        resolved: !partial.nil?,
      )
      return unless partial

      parent_scope = shared ? @scope : Scope.new
      with_scope(parent: parent_scope) do
        node.attributes.each_key { |key| define_local(key) }
        define_local(context_name) if context_name && (shared || node.variable_name_expr)

        @partial_stack.push(name)
        begin
          without_assignment_recording do
            if for_loop
              with_conditional { visit(partial.root) }
            else
              visit(partial.root)
            end
          end
        ensure
          @partial_stack.pop
        end
      end
    end

    def load_partial(name)
      source = @partials[name]
      return nil unless source

      template = if source.is_a?(Template)
        source
      elsif @environment
        Template.parse(source, environment: @environment)
      else
        Template.parse(source)
      end
      template.root ? template : nil
    end

    def record_expression(expr)
      case expr
      when VariableLookup
        record_lookup(expr)
      when RangeLookup
        record_expression(expr.start_obj)
        record_expression(expr.end_obj)
      end
    end

    def record_lookup(lookup)
      segments = []

      name = lookup.name
      if name.is_a?(String)
        segments << name
      else
        record_expression(name)
        segments << DYNAMIC
      end

      lookup.lookups.each do |part|
        case part
        when String
          segments << part
        when VariableLookup, RangeLookup
          record_expression(part)
          segments << DYNAMIC
        else
          # Literal subscript such as [0] or ["key"]: a fixed key.
          segments << part.to_s
        end
      end

      record_read_segments(segments)
    end

    def record_read_segments(segments)
      root = segments.first

      optional = if root.is_a?(String) && (binding_kind = @scope.lookup(root))
        # A name bound only inside a conditional branch may still fall
        # through to the render context when the branch did not run.
        return if binding_kind == :unconditional

        true
      else
        @conditional_depth > 0
      end

      read = Read.new(segments, optional: optional)
      return if @seen_reads[read]

      @seen_reads[read] = true
      @reads << read
    end

    def define(name)
      kind = @conditional_depth > 0 ? :conditional : :unconditional
      @scope.define(name, conditional: @conditional_depth > 0)
      return unless @record_assignments

      @assignments[name] = kind unless kind == :conditional && @assignments[name] == :unconditional
    end

    # Defines a name that is only visible inside the current (inner) scope
    # and is never reported as a template-level assignment.
    def define_local(name)
      @scope.define(name, conditional: false)
    end

    def with_scope(parent: @scope)
      previous = @scope
      @scope = Scope.new(parent)
      yield
    ensure
      @scope = previous
    end

    def with_conditional
      @conditional_depth += 1
      yield
    ensure
      @conditional_depth -= 1
    end

    def without_assignment_recording
      previous = @record_assignments
      @record_assignments = false
      yield
    ensure
      @record_assignments = previous
    end
  end
end
