# frozen_string_literal: true
require "set"

module GraphQL
  module Analysis
    # Symbolic, pre-execution analysis of whether a field at a given response
    # path can appear in a response.
    #
    # Unlike a naive scan for a field name, this understands `@skip` and
    # `@include` directives, fragment type conditions, named and inline
    # fragments, boolean variables and their default values. It answers one of:
    #
    # - {ALWAYS_PRESENT} — every valid variable assignment and every possible
    #   runtime object shape causes the field to be selected
    # - {NEVER_PRESENT} — the field can never be selected (no structurally and
    #   type-consistent route reaches it, or every such route is statically
    #   gated off)
    # - {DEPENDS_ON_VARIABLES} — at least one input makes it appear and at
    #   least one makes it disappear. {Result#scenarios} then lists concrete,
    #   reviewable variable assignments and runtime object types that make it
    #   appear.
    #
    # Only schema metadata and the operation document are used. No resolvers,
    # `resolve_type`, or field methods are invoked. Custom directives are never
    # assumed to behave like `@include`/`@skip`: an unrecognized directive is
    # an opaque gate, so a route containing one can never establish
    # {ALWAYS_PRESENT} and the scenario is flagged for manual review.
    #
    # @example
    #   result = GraphQL::Analysis::FieldPresence.analyze(
    #     MySchema,
    #     document,
    #     %w[order auditTrail],
    #   )
    #   result.verdict              # => :depends_on_variables
    #   result.present?             # => true (reachable for some input)
    #   result.scenarios.first.variables # => { "showAudit" => true }
    class FieldPresence
      # The field is selected for every variable assignment and every runtime
      # type combination reachable through the schema.
      ALWAYS_PRESENT = :always_present

      # No variable assignment / runtime type combination selects the field.
      NEVER_PRESENT = :never_present

      # The field is selected for some inputs but not others (or its presence
      # depends on an unrecognized directive that can't be evaluated here).
      DEPENDS_ON_VARIABLES = :depends_on_variables

      # Guards against exponential work on very large operations: if the
      # boolean-assignment space or the runtime-type combination space is
      # larger than this, the conservative {DEPENDS_ON_VARIABLES} verdict is
      # returned even when a proof of {ALWAYS_PRESENT} might exist.
      MAX_ENUMERATIONS = 10_000

      # One conjunction of conditions under which the target field is
      # selected, carrying a concrete, reviewable witness.
      class Scenario
        # @return [Hash{String => Object}] Variable values to send (plus any
        #   provided values in effect) that make the field appear. Variables
        #   whose declared default already satisfies the gate are omitted.
        attr_reader :variables

        # @return [Array<Array<String>>] At each path depth, the concrete
        #   object type names for which the field is selected along this
        #   route. Unconstrained depths list every possible object type.
        attr_reader :type_candidates

        # @return [Array<Hash>] The AST selections on this route (fields,
        #   fragment spreads, inline type conditions and directives), with
        #   line numbers, so the route can be re-reviewed by hand.
        attr_reader :selection_trail

        # @return [Array<String>] Descriptions of gates that could not be
        #   interpreted symbolically (custom directives, non-boolean
        #   conditions, unknown types). These must be reviewed manually.
        attr_reader :unknown_gates

        def initialize(variables:, type_candidates:, selection_trail:, unknown_gates:)
          @variables = variables
          @type_candidates = type_candidates
          @selection_trail = selection_trail
          @unknown_gates = unknown_gates
        end

        # @return [Boolean] True if an opaque (non-built-in) gate is on this route
        def unknown_gate?
          !@unknown_gates.empty?
        end

        def inspect
          "#<#{self.class.name} variables=#{@variables.inspect} " \
            "types=#{@type_candidates.inspect} unknown_gates=#{@unknown_gates.inspect}>"
        end
      end

      # The analysis result.
      class Result
        # @return [Symbol] One of {FieldPresence::ALWAYS_PRESENT},
        #   {FieldPresence::NEVER_PRESENT}, {FieldPresence::DEPENDS_ON_VARIABLES}
        attr_reader :verdict

        # @return [Array<Scenario>] Concrete routes under which the field is
        #   selected. Empty for {FieldPresence::NEVER_PRESENT}.
        attr_reader :scenarios

        # @return [Array<String>] Variables referenced by relevant
        #   `@skip(if:)` / `@include(if:)` arguments along routes to the field.
        attr_reader :referenced_variables

        def initialize(verdict:, scenarios:, referenced_variables:)
          @verdict = verdict
          @scenarios = scenarios
          @referenced_variables = referenced_variables
        end

        def always_present?
          @verdict == ALWAYS_PRESENT
        end

        def never_present?
          @verdict == NEVER_PRESENT
        end

        def depends_on_variables?
          @verdict == DEPENDS_ON_VARIABLES
        end

        # @return [Boolean] True if the field is selected in at least one scenario
        def present?
          @verdict != NEVER_PRESENT
        end

        def inspect
          "#<#{self.class.name} verdict=#{@verdict.inspect} " \
            "scenarios=#{@scenarios.size} referenced_variables=#{@referenced_variables.inspect}>"
        end
      end

      class << self
        # Analyze a single operation in `document` for the field at `path`.
        #
        # @param schema [Class<GraphQL::Schema>]
        # @param document [GraphQL::Query, GraphQL::Language::Nodes::Document, String]
        #   The operation document (a query string is parsed), or an already
        #   built {GraphQL::Query}. Constructing a {GraphQL::Query} never
        #   executes the operation.
        # @param path [Array<String, Symbol>, String] Response path to the
        #   field from the operation root. A `"."`-separated string is also
        #   accepted; each segment is a response key (aliases honored).
        # @param operation_name [String, nil] Which operation to analyze when
        #   the document contains several.
        # @param variables [Hash{String, Symbol => Object}, nil] A concrete
        #   variable assignment to assume. Variables absent here stay
        #   symbolic (declared default values are still considered).
        # @return [Result]
        def analyze(schema, document, path, operation_name: nil, variables: nil)
          query, doc, provided = coerce_inputs(schema, document, operation_name, variables)
          segments = coerce_path(path)
          operation = find_operation(doc, operation_name, query)
          if operation.nil?
            name_part = " named #{operation_name.inspect}" if operation_name
            raise GraphQL::Error, "No operation#{name_part} found in document"
          end

          new(query, doc, operation, segments, provided).perform
        end

        private

        def coerce_inputs(schema, document, operation_name, variables)
          case document
          when GraphQL::Query
            [document, document.document, stringify(document.provided_variables)]
          when Language::Nodes::Document
            query = GraphQL::Query.new(
              schema, document: document,
              operation_name: operation_name, variables: variables, validate: false,
            )
            [query, document, stringify(variables)]
          when String
            query = GraphQL::Query.new(
              schema, document,
              operation_name: operation_name, variables: variables, validate: false,
            )
            [query, query.document, stringify(variables)]
          else
            raise ArgumentError, "Expected a GraphQL::Query, document, or query String, got #{document.class}"
          end
        end

        def stringify(vars)
          return {} if vars.nil?
          vars.each_with_object({}) { |(k, v), memo| memo[k.to_s] = v }
        end

        def coerce_path(path)
          segments =
            case path
            when Array then path.map(&:to_s)
            when String then path.split(".").reject(&:empty?)
            when Symbol then path.to_s.split(".")
            else raise ArgumentError, "Expected an Array or dot-separated String path, got #{path.class}"
            end
          if segments.empty?
            raise ArgumentError, "Path must contain at least one field name"
          end
          segments
        end

        def find_operation(document, operation_name, query)
          operations = document.definitions.select { |d| d.is_a?(Language::Nodes::OperationDefinition) }
          if operation_name
            operations.find { |op| op.name == operation_name }
          elsif operations.size == 1
            operations.first
          else
            query.selected_operation
          end
        end
      end

      # A single DNF term: a conjunction of boolean constraints over variables,
      # per-depth runtime-type constraints, and opaque-gate markers.
      class Clause
        attr_reader :constraints, :unknown_gates, :type_candidates, :trail, :provided_values
        attr_accessor :impossible

        def initialize(constraints: {}, unknown_gates: [], type_candidates: [], trail: [], provided_values: {})
          @constraints = constraints
          @unknown_gates = unknown_gates
          @type_candidates = type_candidates
          @trail = trail
          @provided_values = provided_values
          @impossible = false
        end

        def dup_with(trail: @trail, candidates: @type_candidates)
          self.class.new(
            constraints: @constraints.dup,
            unknown_gates: @unknown_gates.dup,
            type_candidates: candidates.dup,
            trail: trail,
            provided_values: @provided_values.dup,
          )
        end

        def require_value(variable, value)
          if @constraints.key?(variable) && @constraints[variable] != value
            @impossible = true
          else
            @constraints[variable] = value
          end
        end

        def satisfiable?
          !@impossible
        end

        def opaque?
          !@unknown_gates.empty?
        end
      end

      # @api private
      def initialize(query, document, operation, segments, provided)
        @query = query
        @types = query.types
        @schema = query.schema
        @document = document
        @operation = operation
        @segments = segments
        @provided = provided || {}
        @fragments = {}
        @defaults = {}
        @boolean_vars = {}
        prepare!
      end

      # @api private
      def perform
        root_type = @schema.root_type_for_operation(@operation.operation_type || "query")
        clauses = []
        initial_trail = [
          { kind: :operation, name: @operation.name, type: (@operation.operation_type || "query"), line: @operation.line },
        ] + directive_entries(@operation.directives)
        initial_clause = Clause.new(trail: initial_trail)
        apply_directives!(@operation.directives, initial_clause)

        if initial_clause.satisfiable?
          walk(
            @operation.selections,
            root_type,
            concrete_types(root_type),
            depth: 0,
            clause: initial_clause,
            clauses: clauses,
            visited: Set.new,
          )
        end

        feasible = clauses.select { |c| c.satisfiable? && c.type_candidates.none?(&:empty?) }
        referenced = feasible.flat_map { |c| c.constraints.keys.map(&:to_s) }.uniq.sort

        if feasible.empty?
          return Result.new(verdict: NEVER_PRESENT, scenarios: [], referenced_variables: [])
        end

        transparent = feasible.reject(&:opaque?)
        if !transparent.empty? && universally_covered?(transparent)
          Result.new(
            verdict: ALWAYS_PRESENT,
            scenarios: build_scenarios(transparent, referenced),
            referenced_variables: referenced,
          )
        else
          Result.new(
            verdict: DEPENDS_ON_VARIABLES,
            scenarios: build_scenarios(feasible, referenced),
            referenced_variables: referenced,
          )
        end
      end

      private

      def prepare!
        @document.definitions.each do |defn|
          if defn.is_a?(Language::Nodes::FragmentDefinition)
            @fragments[defn.name] = defn
          elsif defn.equal?(@operation)
            defn.variables.each do |var|
              @defaults[var.name] = default_value_of(var)
              @boolean_vars[var.name] = declared_boolean?(var)
            end
          end
        end
      end

      def default_value_of(var_ast)
        case var_ast.default_value
        when Language::Nodes::NullValue then nil
        when true, false then var_ast.default_value
        when nil then nil
        else :opaque
        end
      end

      def declared_boolean?(var_ast)
        named = var_ast.type
        named = named.of_type while named.is_a?(Language::Nodes::NonNullType)
        named.is_a?(Language::Nodes::TypeName) && named.name == "Boolean"
      end

      def concrete_types(type)
        @types.possible_types(type).to_a
      end

      def walk(selections, scope_type, candidates, depth:, clause:, clauses:, visited:)
        selections.each do |selection|
          case selection
          when Language::Nodes::Field
            visit_field(selection, scope_type, candidates, depth, clause, clauses, visited)
          when Language::Nodes::InlineFragment
            visit_inline_fragment(selection, scope_type, candidates, depth, clause, clauses, visited)
          when Language::Nodes::FragmentSpread
            visit_spread(selection, scope_type, candidates, depth, clause, clauses, visited)
          end
        end
      end

      def visit_field(selection, scope_type, candidates, depth, clause, clauses, visited)
        # Path segments are response keys: an alias, or the field name when unaliased.
        response_key = selection.alias || selection.name
        return unless response_key == @segments[depth]

        trail = clause.trail + [field_entry(selection)] + directive_entries(selection.directives)
        field_clause = clause.dup_with(trail: trail)
        apply_directives!(selection.directives, field_clause)
        return unless field_clause.satisfiable?

        field_clause.type_candidates[depth] = candidates
        if depth == @segments.size - 1
          # The selection is only valid (and hence executable) on concrete
          # types that actually define this field; drop the others.
          narrowed = select_types_with_field(scope_type, candidates, selection.name)
          return if narrowed.empty?
          field_clause.type_candidates[depth] = narrowed
          clauses << field_clause
        else
          owning = select_types_with_field(scope_type, candidates, selection.name)
          return if owning.empty?
          field_clause.type_candidates[depth] = owning
          descend_field_selection(selection, scope_type, owning, depth, field_clause, clauses, visited)
        end
      end

      # Descend into a field that still has path segments beneath it.
      #
      # When the scope is abstract, different concrete types may define a field
      # with the same name but different return types. Each distinct return
      # type gets its own clause so the remaining path is checked against the
      # right schema types; branches whose return type doesn't contain the
      # next segment simply yield no clauses.
      def descend_field_selection(selection, scope_type, candidates, depth, field_clause, clauses, visited)
        groups = field_return_groups(scope_type, candidates, selection.name)
        groups.each do |ret_type, owning_types|
          next unless ret_type.kind.composite?

          branch = field_clause.dup_with
          branch.type_candidates[depth] = owning_types
          walk(
            selection.selections,
            ret_type,
            concrete_types(ret_type),
            depth: depth + 1,
            clause: branch,
            clauses: clauses,
            visited: visited,
          )
        end
      end

      # @return [Hash{GraphQL::Schema::Object => Array}] concrete return types per owning concrete type
      def field_return_groups(scope_type, candidates, field_name)
        direct = @types.field(scope_type, field_name)
        if direct
          { direct.type.unwrap => candidates }
        elsif scope_type.kind.abstract?
          groups = Hash.new { |h, k| h[k] = [] }
          candidates.each do |obj_type|
            f = @types.field(obj_type, field_name)
            groups[f.type.unwrap] << obj_type if f
          end
          groups
        else
          {}
        end
      end

      def visit_inline_fragment(selection, scope_type, candidates, depth, clause, clauses, visited)
        trail = clause.trail
        narrowed = candidates

        if selection.type
          cond_type = @types.type(selection.type.name)
          trail = trail + [{ kind: :inline_fragment, type: selection.type.name, line: selection.line }]
          if cond_type
            narrowed = candidates & concrete_types(cond_type)
            return if narrowed.empty?
          else
            clause = clause.dup_with
            clause.unknown_gates << "unknown type condition: #{selection.type.name}"
          end
        end

        if selection.directives.any?
          trail = trail + directive_entries(selection.directives)
        end

        frag_clause = clause.dup_with(trail: trail)
        apply_directives!(selection.directives, frag_clause)
        return unless frag_clause.satisfiable?

        walk(
          selection.selections,
          scope_type,
          narrowed,
          depth: depth,
          clause: frag_clause,
          clauses: clauses,
          visited: visited,
        )
      end

      def visit_spread(selection, scope_type, candidates, depth, clause, clauses, visited)
        frag_defn = @fragments[selection.name]
        return if frag_defn.nil? || visited.include?(selection.name)

        trail = clause.trail +
          [{ kind: :fragment_spread, name: selection.name, type: frag_defn.type&.name, line: selection.line }] +
          directive_entries(selection.directives)
        spread_clause = clause.dup_with(trail: trail)
        apply_directives!(selection.directives, spread_clause)
        return unless spread_clause.satisfiable?

        cond_type = frag_defn.type && @types.type(frag_defn.type.name)
        if cond_type.nil?
          spread_clause.unknown_gates << "unknown type condition: #{frag_defn.type&.name}"
          narrowed = candidates
        else
          narrowed = candidates & concrete_types(cond_type)
          # Impossible spread against the types in scope: never executes.
          return if narrowed.empty?
        end

        # Directives on a fragment *definition* are not executable per spec,
        # so they are intentionally not inspected here.
        walk(
          frag_defn.selections,
          scope_type,
          narrowed,
          depth: depth,
          clause: spread_clause,
          clauses: clauses,
          visited: visited + [selection.name],
        )
      end

      # When a fragment type condition narrowed `scope_type`'s possibilities,
      # the selected field must actually exist on each remaining concrete type
      # (otherwise that route is invalid and never executes).
      def select_types_with_field(scope_type, candidates, field_name)
        return candidates if @types.field(scope_type, field_name)
        candidates.select { |obj_type| @types.field(obj_type, field_name) }
      end

      def apply_directives!(directive_nodes, clause)
        directive_nodes.each do |dir_node|
          case dir_node.name
          when "skip"
            gate(directive_name: "skip", positive: false, node: dir_node, clause: clause)
          when "include"
            gate(directive_name: "include", positive: true, node: dir_node, clause: clause)
          else
            # Never assume a custom directive behaves like @include/@skip.
            clause.unknown_gates << "unrecognized directive: @#{dir_node.name}"
          end
        end
      end

      def gate(directive_name:, positive:, node:, clause:)
        arg = node.arguments.find { |a| a.name == "if" }
        value = arg.nil? ? :missing : arg.value

        case value
        when true
          clause.impossible = true unless positive # @skip(if: true)
        when false
          clause.impossible = true if positive      # @include(if: false)
        when nil, :missing, Language::Nodes::NullValue
          # Null or missing `if` is treated as non-gating by execution.
        when Language::Nodes::VariableIdentifier
          gate_on_variable(directive_name, value.name, positive, clause)
        else
          clause.unknown_gates << "@#{directive_name}(if: #{value.inspect})"
        end
      end

      def gate_on_variable(directive_name, var_name, positive, clause)
        if @provided.key?(var_name)
          clause.provided_values[var_name] = @provided[var_name]
          provided_value = @provided[var_name]
          case provided_value
          when true
            clause.impossible = true unless positive
          when false
            clause.impossible = true if positive
          when nil
            # null is non-gating
          else
            clause.unknown_gates << "@#{directive_name}(if: $#{var_name} = #{provided_value.inspect})"
          end
        elsif @boolean_vars[var_name] == true
          clause.require_value(var_name, positive)
        else
          # Undeclared variable or non-boolean declared type: can't evaluate.
          reason = @boolean_vars[var_name].nil? ? "undeclared" : "non-boolean"
          clause.unknown_gates << "@#{directive_name}(if: #{reason} $#{var_name})"
        end
      end

      def field_entry(selection)
        { kind: :field, name: selection.name, response_key: selection.alias || selection.name, line: selection.line }
      end

      def directive_entries(directive_nodes)
        directive_nodes.map { |d| { kind: :directive, name: d.name, line: d.line } }
      end

      # --- Verdict proof -----------------------------------------------------

      # Do the given (fully interpretable) clauses cover the entire boolean
      # assignment space AND every possible runtime type combination?
      def universally_covered?(clauses)
        variables = clauses.flat_map { |c| c.constraints.keys }.uniq

        grid = full_type_grid
        return false if grid.nil?

        assignments = enumerate_assignments(variables)
        return false if assignments.nil?
        return false if assignments.size * grid.size > MAX_ENUMERATIONS

        assignments.all? do |assignment|
          active = clauses.select { |c| assignment_satisfies?(c, assignment) }
          grid.all? { |point| active.any? { |c| covers_point?(c, point) } }
        end
      end

      # The cartesian set of possible concrete types at each path depth.
      # @return [Array<Array>, nil] nil if the space is too large to enumerate
      def full_type_grid
        axes = @segments.size.times.map { |d| full_types_at_depth(d) }
        return nil if axes.any? { |a| a.empty? }
        size = axes.reduce(1) { |n, a| n * a.size }
        return nil if size > MAX_ENUMERATIONS
        cartesian(axes)
      end

      # Concrete object types that can occur at `depth` along any schema-valid
      # instance of the field path. When an abstract scope implements the path
      # field heteromorphically (different concrete types returning different
      # types for the same field name) the possibilities are unioned. Using
      # this (slight) over-approximation keeps the ALWAYS proof conservative:
      # proving coverage over a larger space is never unsound.
      def full_types_at_depth(depth)
        scopes = [@schema.root_type_for_operation(@operation.operation_type || "query")]
        depth.times do |i|
          next_scopes = []
          scopes.uniq.each do |scope|
            owning = scope.kind.object? ? [scope] : concrete_types(scope)
            owning.each do |obj_type|
              field_defn = @types.field(obj_type, @segments[i])
              next if field_defn.nil?
              ret = field_defn.type.unwrap
              next_scopes.concat(ret.kind.object? ? [ret] : concrete_types(ret))
            end
          end
          scopes = next_scopes.uniq
          return [] if scopes.empty?
        end
        scopes
      end

      # @return [Array<Hash>, nil] all assignments over free variables (nil if too many)
      def enumerate_assignments(variables)
        assignments = [{}]
        variables.each do |var|
          # Effective values the `if` argument can take at runtime: explicit
          # true/false/null, or the variable's declared default value. (For
          # non-null variables null is an invalid input, but including it only
          # makes the ALWAYS proof more conservative, never unsound.)
          domain = [true, false, nil, @defaults[var]].uniq
          domain.delete(:opaque)
          next_assignments = []
          assignments.each do |assign|
            domain.each { |v| next_assignments << assign.merge(var => v) }
          end
          assignments = next_assignments
          return nil if assignments.size > MAX_ENUMERATIONS
        end
        assignments
      end

      def assignment_satisfies?(clause, assignment)
        clause.constraints.all? do |var, required|
          assignment.fetch(var, @defaults[var]) == required
        end
      end

      def covers_point?(clause, point)
        point.each_with_index.all? do |object_type, depth|
          clause.type_candidates[depth]&.include?(object_type)
        end
      end

      def cartesian(axes)
        return [[]] if axes.empty?
        axes[0].flat_map do |head|
          cartesian(axes[1..]).map { |tail| [head] + tail }
        end
      end

      def build_scenarios(clauses, referenced)
        seen = {}
        clauses.each do |clause|
          witness = witness_assignment(clause, referenced)
          types = @segments.size.times.map do |d|
            (clause.type_candidates[d] || []).map(&:graphql_name)
          end
          key = [witness, types]
          next if seen.key?(key)
          seen[key] = Scenario.new(
            variables: witness,
            type_candidates: types,
            selection_trail: clause.trail,
            unknown_gates: clause.unknown_gates.uniq,
          )
        end
        seen.values
      end

      def witness_assignment(clause, referenced)
        assignment = clause.provided_values.dup
        clause.constraints.each do |var, required|
          next if assignment.key?(var.to_s)
          next if @defaults[var.to_s] == required # default already makes the gate pass
          assignment[var.to_s] = required
        end
        assignment
      end
    end
  end
end
