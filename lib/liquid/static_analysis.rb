# frozen_string_literal: true

require 'set'

module Liquid
  # Syntax-based static analysis of a template's input dependencies.
  #
  # StaticAnalysis walks the parsed template (it never renders anything) and
  # reports which variables the template reads from the outer context, so
  # callers can configure template inputs without listing variables that the
  # template binds itself (for example `for` loop variables):
  #
  #   result = Liquid::StaticAnalysis.analyze(source, partials: { 'item' => item_source })
  #   result.required   # paths always read: these must be provided as input
  #   result.optional   # paths read only inside conditional branches
  #   result.internal   # names the template binds itself (assign, capture, for, ...)
  #
  # Scoping rules follow the runtime:
  #
  # * `assign`/`capture` bind in the current scope, `for`/`tablerow` bind their
  #   loop variable (plus `forloop`/`tablerowloop`) only inside the loop body.
  # * `include` shares the outer scope with the partial; `render` isolates the
  #   partial, which only sees the parameters explicitly passed to it.
  # * Reads inside `if`/`unless`/`case` branches and loop bodies are reported
  #   as optional, because they may not execute.
  # * A dynamic subscript such as `items[key]` yields a bounded unknown path
  #   (`items.*`) instead of guessing a fixed field.
  module StaticAnalysis
    # A single variable read, described as a dotted path.
    #
    # When the path passes through a dynamic subscript (for example
    # `items[key]` or `items[key].title`), the concrete key is unknowable
    # statically, so the path is truncated at that point and marked dynamic:
    # `items.*` means "some unknown field of items".
    class Path
      include Comparable

      attr_reader :segments

      def initialize(segments, dynamic: false)
        super()
        @segments = segments.map(&:to_s).freeze
        @dynamic  = dynamic
        freeze
      end

      def dynamic?
        @dynamic
      end

      def root
        @segments.first
      end

      def <=>(other)
        order = segments <=> other.segments
        return order unless order&.zero?

        (dynamic? ? 1 : 0) <=> (other.dynamic? ? 1 : 0)
      end

      def ==(other)
        other.is_a?(Path) && segments == other.segments && dynamic? == other.dynamic?
      end
      alias_method :eql?, :==

      def hash
        [self.class, @segments, @dynamic].hash
      end

      def to_s
        base = @segments.join('.')
        if @dynamic
          base.empty? ? '*' : "#{base}.*"
        else
          base
        end
      end

      def inspect
        "#<Liquid::StaticAnalysis::Path #{self}>"
      end
    end

    # An `include`/`render` whose partial could not be analyzed statically.
    UnresolvedPartial = Struct.new(:name, :tag, :reason, keyword_init: true) do
      def to_h
        {
          'name'   => name,
          'tag'    => tag.to_s,
          'reason' => reason.to_s,
        }
      end
    end

    # The outcome of an analysis run.
    class Result
      # Paths read from the outer context on every render.
      attr_reader :required
      # Paths read from the outer context only inside conditional branches.
      attr_reader :optional
      # Names the template binds itself, mapped to the kinds of binding
      # (`:assign`, `:capture`, `:for`, `:tablerow`, `:increment`,
      # `:decrement`, `:include`, `:render`).
      attr_reader :internal
      # Partials that could not be analyzed (dynamic name, missing source, or
      # recursive inclusion).
      attr_reader :unresolved_partials

      def initialize(required:, optional:, internal:, unresolved_partials:)
        super()
        @required            = required.freeze
        @optional            = optional.freeze
        @internal            = internal.transform_values { |kinds| kinds.sort_by(&:to_s).freeze }.freeze
        @unresolved_partials = unresolved_partials.freeze
      end

      # Every external input path, required and optional.
      def external
        @required | @optional
      end

      def internal?(name)
        @internal.key?(name.to_s)
      end

      def to_h
        {
          'required'            => @required.map(&:to_s),
          'optional'            => @optional.map(&:to_s),
          'internal'            => @internal.transform_values { |kinds| kinds.map(&:to_s) },
          'unresolved_partials' => @unresolved_partials.map(&:to_h),
        }
      end
    end

    # Analyzes a template and returns a Result.
    #
    # `template` may be a source String, a Liquid::Template, a
    # Liquid::Document, or a Liquid::BlockBody.
    #
    # `partials` resolves `include`/`render` template names to sources. It may
    # be a Hash of name => source, a callable, or an object responding to
    # `read_template_file` (such as a Liquid file system). Partials that
    # cannot be resolved are reported in Result#unresolved_partials.
    def self.analyze(template, partials: nil)
      Analyzer.new(partials: partials).analyze(template)
    end

    # @api private
    class Analyzer
      def initialize(partials: nil)
        @partials = partials
      end

      def analyze(template)
        @scopes            = [{}]
        @conditional       = 0
        @required_paths    = Set.new
        @optional_paths    = Set.new
        @internals         = {}
        @unresolved        = []
        @partial_stack     = []
        @partial_documents = {}
        @environment       = nil

        visit_root(template)
        build_result
      end

      private

      def visit_root(template)
        case template
        when String
          visit_document(Liquid::Template.parse(template).root)
        when Liquid::Template
          raise ArgumentError, "template has not been parsed" unless template.root

          visit_document(template.root)
        when Liquid::Document
          visit_document(template)
        when Liquid::BlockBody
          visit_all(template.nodelist)
        else
          raise ArgumentError, "cannot analyze #{template.class}"
        end
      end

      def visit_document(document)
        @environment ||= document.parse_context.environment
        visit_all(document.nodelist)
      end

      def visit_all(nodes)
        nodes.each { |node| visit(node) }
      end

      def visit(node)
        case node
        when nil, String
          # literal output, nothing to read
        when Liquid::Variable
          visit_output(node)
        when Liquid::Echo
          visit_output(node.variable)
        when Liquid::Assign
          visit_output(node.from)
          bind(node.to, :assign)
        when Liquid::Capture
          visit_all(node.nodelist)
          bind(node.to, :capture)
        when Liquid::For
          visit_for(node)
        when Liquid::TableRow
          visit_table_row(node)
        when Liquid::If, Liquid::Case
          # Liquid::If also covers Liquid::Unless; Liquid::Case embeds its
          # left-hand expression in each `when` condition.
          visit_branches(node)
        when Liquid::Cycle
          node.variables.each { |variable| read_expression(variable) }
        when Liquid::Increment
          bind(node.variable_name, :increment)
        when Liquid::Decrement
          bind(node.variable_name, :decrement)
        when Liquid::Include
          visit_include(node)
        when Liquid::Render
          visit_render(node)
        when Liquid::Condition
          read_condition(node)
        when Liquid::VariableLookup
          read_lookup(node)
        when Liquid::RangeLookup
          read_expression(node)
        else
          visit_generic(node)
        end
      end

      # Custom tags and container nodes (Document, BlockBody, Ifchanged, Raw,
      # Comment, Doc, ...) are traversed structurally.
      def visit_generic(node)
        visit_all(Array(node.nodelist)) if node.respond_to?(:nodelist)
      end

      def visit_output(variable)
        read_expression(variable.name)
        variable.filters.each do |(_filter_name, args, kwargs)|
          args.each { |arg| read_expression(arg) }
          kwargs&.each_value { |value| read_expression(value) }
        end
      end

      def visit_for(node)
        read_expression(node.collection_name)
        read_expression(node.limit) if node.limit
        read_expression(node.from) if node.from && node.from != :continue

        for_block, else_block = node.nodelist

        with_scope do
          bind(node.variable_name, :for)
          bind('forloop', :for)
          conditionally { visit(for_block) }
        end
        conditionally { visit(else_block) } if else_block
      end

      def visit_table_row(node)
        read_expression(node.collection_name)
        node.attributes.each_value { |value| read_expression(value) }

        with_scope do
          bind(node.variable_name, :tablerow)
          bind('tablerowloop', :tablerow)
          conditionally { visit_all(node.nodelist) }
        end
      end

      def visit_branches(node)
        read_expression(node.left) if node.is_a?(Liquid::Case)
        node.blocks.each do |condition|
          read_condition(condition)
          conditionally { visit_all(condition.attachment.nodelist) }
        end
      end

      def read_condition(condition)
        while condition
          read_expression(condition.left)
          read_expression(condition.right)
          condition = condition.child_condition
        end
      end

      def visit_include(node)
        name_expr = node.template_name_expr
        read_expression(name_expr) unless name_expr.is_a?(String)
        read_expression(node.variable_name_expr) if node.variable_name_expr
        node.attributes.each_value { |value| read_expression(value) }

        name = name_expr.is_a?(String) ? name_expr : nil

        # {% include 'item' %} without with/for reads the variable named after
        # the template from the shared context.
        record_read([name], dynamic: false) if name && !node.variable_name_expr

        source = name && resolve_partial(name)
        return record_unresolved(name, :include) unless source

        # include shares the outer scopes with the partial; only the alias and
        # the explicit attributes are added on top.
        with_scope do
          bind(node.alias_name || name.split('/').last, :include)
          node.attributes.each_key { |key| bind(key, :include) }
          visit_partial(name, source, :include)
        end
      end

      def visit_render(node)
        name_expr = node.template_name_expr
        read_expression(name_expr) unless name_expr.is_a?(String)
        read_expression(node.variable_name_expr) if node.variable_name_expr
        node.attributes.each_value { |value| read_expression(value) }

        name   = name_expr.is_a?(String) ? name_expr : nil
        source = name && resolve_partial(name)
        return record_unresolved(name, :render) unless source

        # render isolates the partial: it only sees the parameters explicitly
        # passed to it, never the outer template's assignments.
        with_isolated_scope do
          node.attributes.each_key { |key| bind(key, :render) }
          if node.variable_name_expr
            bind(node.alias_name || name.split('/').last, :render)
          end
          bind('forloop', :render) if node.for_loop?
          visit_partial(name, source, :render)
        end
      end

      def visit_partial(name, source, tag)
        if @partial_stack.include?(name)
          record_unresolved(name, tag, :recursive)
          return
        end

        @partial_stack.push(name)
        begin
          visit_document(partial_document(name, source))
        ensure
          @partial_stack.pop
        end
      end

      def partial_document(name, source)
        @partial_documents[name] ||= begin
          options = @environment ? { environment: @environment } : {}
          Liquid::Template.parse(source, **options).root
        end
      end

      def resolve_partial(name)
        return nil unless @partials

        if @partials.respond_to?(:call)
          @partials.call(name)
        elsif @partials.respond_to?(:key?)
          @partials[name] if @partials.key?(name)
        elsif @partials.respond_to?(:read_template_file)
          @partials.read_template_file(name)
        end
      rescue StandardError
        nil
      end

      def record_unresolved(name, tag, reason = nil)
        reason ||= name.nil? ? :dynamic_name : :missing_partial
        @unresolved << UnresolvedPartial.new(name: name, tag: tag, reason: reason)
      end

      def read_expression(expression)
        case expression
        when Liquid::VariableLookup
          read_lookup(expression)
        when Liquid::RangeLookup
          read_expression(expression.start_obj)
          read_expression(expression.end_obj)
        when Liquid::Condition
          read_condition(expression)
        end
        # Anything else (String, Numeric, Range, Condition::MethodLiteral,
        # true/false/nil, ...) is a literal with no variable reads.
      end

      def read_lookup(lookup)
        segments = []
        dynamic  = false

        case (name = lookup.name)
        when String, Integer
          segments << name
        else
          # dynamic root, for example `[section].title`
          read_expression(name)
          dynamic = true
        end

        unless dynamic
          lookup.lookups.each do |part|
            case part
            when String, Integer
              segments << part
            else
              # dynamic subscript, for example `items[key]`: the key is read,
              # and everything past it is a bounded unknown path
              read_expression(part)
              dynamic = true
              break
            end
          end
        end

        record_read(segments, dynamic: dynamic)
      end

      def record_read(segments, dynamic:)
        root = segments.first
        return if root && bound?(root.to_s)

        path = Path.new(segments, dynamic: dynamic)
        if @conditional.positive?
          @optional_paths << path unless @required_paths.include?(path)
        else
          @optional_paths.delete(path)
          @required_paths << path
        end
      end

      def bind(name, kind)
        return unless name

        @scopes.last[name.to_s] = kind
        kinds = (@internals[name.to_s] ||= [])
        kinds << kind unless kinds.include?(kind)
      end

      def bound?(name)
        @scopes.reverse_each.any? { |scope| scope.key?(name) }
      end

      def with_scope
        @scopes.push({})
        yield
      ensure
        @scopes.pop
      end

      def with_isolated_scope
        outer_scopes = @scopes
        @scopes = [{}]
        yield
      ensure
        @scopes = outer_scopes
      end

      def conditionally
        @conditional += 1
        yield
      ensure
        @conditional -= 1
      end

      def build_result
        Result.new(
          required: @required_paths.to_a.sort,
          optional: @optional_paths.to_a.sort,
          internal: @internals,
          unresolved_partials: @unresolved,
        )
      end
    end

    private_constant :Analyzer
  end
end
