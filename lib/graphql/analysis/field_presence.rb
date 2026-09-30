# frozen_string_literal: true
module GraphQL
  module Analysis
    # Symbolic, static analysis of whether a response path (field) can appear
    # in the response of an operation, *before* the operation is executed.
    #
    # Unlike a naive field-name scan, this follows the GraphQL execution rules:
    #
    # - `@skip(if:)` / `@include(if:)` directives are evaluated symbolically,
    #   including when their `if` argument references a Boolean variable.
    # - Operation-provided variable values and variable definition default
    #   values are applied (they never depend on the response data).
    # - Nested `@skip`/`@include` conditions are conjuncted along a path.
    # - Fragment spreads and inline fragments are inlined together with
    #   their type conditions. The same field reached through several
    #   fragment paths is handled (the paths combine with OR).
    # - Type conditions which cannot simultaneously hold according to the
    #   schema (two disjoint object types, an object outside an interface,
    #   etc.) prune the path.
    # - Unknown custom directives are never assumed to pass: they are
    #   recorded as opaque gates and force a conditional result.
    #
    # No resolver is ever called; only the schema's type structure is read.
    #
    # @example
    #   result = GraphQL::Analysis::FieldPresence.analyze(query, ["node", "auditTrail"])
    #   result.status       # => :always, :never, or :variable
    #   result.always?      # => true / false
    #   result.witnesses    # => [Witness] reviewable assignments proving reachability
    module FieldPresence
      # At least one path reaches the field unconditionally.
      ALWAYS = :always
      # No path can reach the field.
      NEVER = :never
      # Reachable, but depending on variables, object types, or unknown gates.
      VARIABLE = :variable

      class << self
        # Analyze whether `path` can appear in the response of `query`'s
        # selected operation (or the operation named `operation_name`).
        #
        # @param query [GraphQL::Query]
        # @param path [Array<String, Symbol>, String] Response path, e.g.
        #   `["node", "auditTrail"]` or `"node.auditTrail"`. Field aliases
        #   are matched as well.
        # @param operation_name [String, nil] analyze this operation instead
        #   of the query's selected operation
        # @return [FieldPresence::Result]
        def analyze(query, path, operation_name: nil)
          path_segments = normalize_path(path)
          if path_segments.empty?
            raise ArgumentError, "path must contain at least one field name"
          end

          operation = resolve_operation(query, operation_name)
          root_type = query.root_type_for_operation(operation.operation_type || "query")
          analyzer = Analyzer.new(query, path_segments)
          analyzer.collect(operation, root_type)
          analyzer.result
        end

        private

        def normalize_path(path)
          case path
          when String
            path.split(".").map(&:to_s)
          when Array
            path.map { |seg| seg.to_s }
          else
            raise ArgumentError, "path must be an Array of field names or a dot-separated String"
          end
        end

        def resolve_operation(query, operation_name)
          if operation_name
            op = query.operations[operation_name]
            raise ArgumentError, "no operation named #{operation_name.inspect} in the query" if op.nil?
            op
          else
            query.selected_operation || raise(ArgumentError, "the query contains no operation")
          end
        end
      end

      # A concrete, human-reviewable assignment proving one path is reachable.
      class Witness
        # @return [Hash{String => Boolean}] variable values to use
        attr_reader :variables

        # Concrete object type required at each response-path position.
        # `nil` at a position means "no type condition" - any object type the
        # parent can take works there. Aligned with the analyzed path
        # segments.
        # @return [Array<String, nil>]
        attr_reader :types

        # @return [Array<Hash>] opaque gates (unknown/non-Boolean directives)
        attr_reader :unknown_gates

        # @return [Array<String>] fields/fragments taken, for review
        attr_reader :route

        def initialize(variables:, types:, unknown_gates:, route:)
          @variables = variables
          @types = types
          @unknown_gates = unknown_gates
          @route = route
        end
      end

      # Outcome of an analysis.
      class Result
        # @return [Symbol] {ALWAYS}, {NEVER}, or {VARIABLE}
        attr_reader :status

        # @return [Array<String>] variable names reachability depends on
        attr_reader :variables

        # @return [Array<Witness>] assignments proving each reachable path
        attr_reader :witnesses

        # @return [Array<Hash>] unknown directives encountered on paths
        attr_reader :unknown_gates

        def initialize(status, variables, witnesses, unknown_gates)
          @status = status
          @variables = variables
          @witnesses = witnesses
          @unknown_gates = unknown_gates
        end

        def always?
          @status == ALWAYS
        end

        def never?
          @status == NEVER
        end

        # Reachable, but only depending on variables, concrete object types
        # (fragment type conditions), or directives the analyzer doesn't
        # understand.
        def variable?
          @status == VARIABLE
        end

        def possible?
          @status != NEVER
        end
      end

      # @api private
      class Analyzer
        SKIP = "skip"
        INCLUDE = "include"

        CONST_TRUE = :const_true
        CONST_FALSE = :const_false
        IMPOSSIBLE_CLAUSE = { impossible: true }.freeze

        def initialize(query, path_segments)
          @query = query
          @path_segments = path_segments
          @fragments = query.fragments
          # Effective variable values after applying defaults. Variables not
          # present here have unknown (symbolic) values.
          @variable_values = query.variables.to_h
          @reached = []
        end

        def collect(operation, root_type)
          root_gates = gates_for(operation, "operation #{operation.name || "(anonymous)"}")
          state = PathState.new(
            # Static type used to look up fields of the currently visited
            # selection set. Starts at the root type; fragment type
            # conditions narrow it for field lookup.
            current_type: root_type,
            # Original declared type of `current_type` (root type or return
            # type of the enclosing field). Fragment conditions narrow
            # `current_type` but never this. Used to decide whether a type
            # condition actually constrains the runtime object.
            declared_current_type: root_type,
            # Concrete object types `current_type` may be, after fragment
            # type conditions in surrounding fragments.
            current_set: concrete_set(root_type),
            # For every matched path segment: [declared parent type, concrete
            # parent types after narrowing].
            segment_parent_types: [],
            segment_parent_sets: [],
            bool_clauses: root_gates[:bool],
            unknown_gates: root_gates[:unknown],
            route: [operation.name || "(anonymous query)"],
            spreads: Set.new,
          )
          walk_selections(operation.selections, state)
        end

        def result
          surviving = @reached.reject { |r| unsatisfiable_bool?(r[:bool_clauses]) }
          return Result.new(NEVER, [], [], []) if surviving.empty?

          unconstrained = surviving.find do |r|
            r[:bool_clauses].empty? &&
              r[:unknown_gates].empty? &&
              positions_unconstrained?(r)
          end

          witnesses = surviving
            .map { |r| build_witness(r) }
            .uniq { |w| [w.variables, w.types, w.unknown_gates, w.route] }
          variables = surviving
            .flat_map { |r| r[:bool_clauses].reject { |c| c.equal?(IMPOSSIBLE_CLAUSE) }.map { |c| c[:name] } }
            .uniq.sort
          unknown_gates = surviving
            .flat_map { |r| r[:unknown_gates] }
            .uniq { |g| [g[:directive], g[:location], g[:reason]] }

          Result.new(unconstrained ? ALWAYS : VARIABLE, variables, witnesses, unknown_gates)
        end

        private

        def walk_selections(selections, state)
          selections.each do |selection|
            case selection
            when GraphQL::Language::Nodes::Field
              walk_field(selection, state)
            when GraphQL::Language::Nodes::InlineFragment
              walk_inline_fragment(selection, state)
            when GraphQL::Language::Nodes::FragmentSpread
              walk_fragment_spread(selection, state)
            end
          end
        end

        def walk_field(field, state)
          field_index = state.segment_parent_types.length
          target_name = @path_segments[field_index]
          return unless field.name == target_name || field.alias == target_name

          field_defn = @query.types.field(state.current_type, field.name)
          return if field_defn.nil?

          gates = gates_for(field, field.alias || field.name)
          new_state = state.enter_field(
            bool: gates[:bool],
            unknown: gates[:unknown],
            label: field.alias || field.name,
          )
          return if unsatisfiable_bool?(new_state.bool_clauses)

          # This field is read off the current (possibly fragment-narrowed)
          # parent object. Record both the parent's declared type and the
          # concrete object types fragment conditions allow it to be.
          new_state.segment_parent_types << state.declared_current_type
          new_state.segment_parent_sets << state.current_set.dup

          unwrapped = field_defn.type.unwrap
          # Descending into the field resets the object context to the
          # field's own return type; fragments in its selection set narrow
          # it again.
          new_state.current_type = unwrapped
          new_state.declared_current_type = unwrapped
          new_state.current_set = concrete_set(unwrapped)

          if new_state.segment_parent_types.length == @path_segments.length
            @reached << new_state.snapshot
            return
          end

          walk_selections(field.selections, new_state)
        end

        def walk_inline_fragment(fragment, state)
          type_name = type_name_of(fragment.type)
          location = type_name ? "... on #{type_name}" : "..."
          gates = gates_for(fragment, location)
          bool = state.bool_clauses + gates[:bool]
          return if unsatisfiable_bool?(bool)

          new_state = state.branch(
            bool: bool,
            unknown: gates[:unknown],
            label: location,
          )

          constrained_type, allowed_set =
            apply_type_condition(type_name, state.current_type, state.current_set)
          return if constrained_type.nil?
          new_state.current_type = constrained_type
          new_state.current_set = allowed_set

          walk_selections(fragment.selections, new_state)
        end

        def walk_fragment_spread(spread, state)
          definition = @fragments[spread.name]
          return if definition.nil?
          # A fragment already expanded on this inlining chain adds nothing.
          return if state.spreads.include?(spread.name)

          spread_gates = gates_for(spread, "...#{spread.name}")
          defn_gates = gates_for(definition, "fragment #{spread.name}")
          bool = state.bool_clauses + spread_gates[:bool] + defn_gates[:bool]
          return if unsatisfiable_bool?(bool)

          new_state = state.branch(
            bool: bool,
            unknown: spread_gates[:unknown] + defn_gates[:unknown],
            label: "...#{spread.name}",
          )
          new_state.spreads = state.spreads.dup.add(spread.name)

          type_name = type_name_of(definition.type)
          constrained_type, allowed_set =
            apply_type_condition(type_name, state.current_type, state.current_set)
          return if constrained_type.nil?
          new_state.current_type = constrained_type
          new_state.current_set = allowed_set

          walk_selections(definition.selections, new_state)
        end

        # Intersect the currently applicable concrete set with a fragment
        # type condition's concrete types. Returns the condition type and the
        # narrowed set, or nil when they cannot coexist.
        def apply_type_condition(type_name, current_type, current_set)
          return [current_type, current_set] if type_name.nil?

          condition_type = @query.types.type(type_name)
          return [nil, nil] if condition_type.nil?

          intersection = current_set & concrete_set(condition_type)
          return [nil, nil] if intersection.empty?

          [condition_type, intersection]
        end

        # A path segment is unconditional when its parent's concrete set
        # still covers its parent's full declared type.
        def positions_unconstrained?(route_data)
          route_data[:segment_parent_types].each_with_index.all? do |parent_type, idx|
            set_covers_type?(route_data[:segment_parent_sets][idx], parent_type)
          end
        end

        def set_covers_type?(concrete_set, declared_type)
          concrete_set(declared_type).all? { |t| concrete_set.include?(t) }
        end

        # Concrete object types a type can take at runtime. Objects and leaf
        # types resolve to themselves; abstract types to possible types.
        def concrete_set(type)
          kind = type.kind
          if kind.interface? || kind.union?
            Set.new(@query.types.possible_types(type).map(&:graphql_name))
          else
            Set[type.graphql_name]
          end
        end

        # Symbolically evaluate a node's directives.
        # @return [Hash{Symbol => Array}] `:bool` clauses and `:unknown` gates
        def gates_for(node, location)
          bool = []
          unknown = []
          node.directives.each do |directive|
            case directive.name
            when SKIP
              clause = boolean_if_clause(directive)
              if clause.nil?
                unknown << gate(directive, location, "non-Boolean @skip(if:)")
              elsif clause == CONST_TRUE
                bool << IMPOSSIBLE_CLAUSE
              elsif clause == CONST_FALSE
                # skip(if: false): no restriction
              else
                bool << { name: clause[:name], negated: !clause[:negated] }
              end
            when INCLUDE
              clause = boolean_if_clause(directive)
              if clause.nil?
                unknown << gate(directive, location, "non-Boolean @include(if:)")
              elsif clause == CONST_TRUE
                # include(if: true): no restriction
              elsif clause == CONST_FALSE
                bool << IMPOSSIBLE_CLAUSE
              else
                bool << clause
              end
            else
              # Unknown/custom directive: graphql-ruby ignores it when
              # deciding inclusion, but it must not be silently treated as
              # passing - an extension could act on it. Opaque gate.
              unknown << gate(directive, location, "unknown directive @#{directive.name}")
            end
          end
          { bool: bool, unknown: unknown }
        end

        def gate(directive, location, reason)
          { directive: directive.name, location: location, reason: reason }
        end

        # Symbolic value of an `if:` argument.
        def boolean_if_clause(directive)
          if_arg = directive.arguments.find { |a| a.name == "if" }
          return nil if if_arg.nil?

          case if_arg.value
          when true then CONST_TRUE
          when false then CONST_FALSE
          when GraphQL::Language::Nodes::VariableIdentifier
            variable_boolean_clause(if_arg.value.name)
          else
            nil
          end
        end

        def variable_boolean_clause(name)
          if @variable_values.key?(name)
            case @variable_values[name]
            when true then CONST_TRUE
            when false then CONST_FALSE
            else nil
            end
          else
            { name: name, negated: false }
          end
        end

        def unsatisfiable_bool?(clauses)
          positive = Set.new
          negative = Set.new
          clauses.each do |c|
            return true if c.equal?(IMPOSSIBLE_CLAUSE)
            if c[:negated]
              return true if positive.include?(c[:name])
              negative << c[:name]
            else
              return true if negative.include?(c[:name])
              positive << c[:name]
            end
          end
          false
        end

        def build_witness(route_data)
          variable_values = {}
          route_data[:bool_clauses].each do |clause|
            next if clause.equal?(IMPOSSIBLE_CLAUSE)
            variable_values[clause[:name]] = !clause[:negated]
          end

          types = route_data[:segment_parent_types].each_with_index.map do |parent_type, idx|
            set = route_data[:segment_parent_sets][idx]
            set_covers_type?(set, parent_type) ? nil : set.first
          end

          Witness.new(
            variables: variable_values,
            types: types,
            unknown_gates: route_data[:unknown_gates],
            route: route_data[:route],
          )
        end

        # Inline/fragment definitions store type conditions as TypeName AST
        # nodes; normalize to the plain type name.
        def type_name_of(type_ast)
          case type_ast
          when nil then nil
          when String then type_ast
          else type_ast.name
          end
        end

        # Mutable traversal state for one inlining chain; branches receive
        # copies so sibling selections stay isolated.
        class PathState
          attr_accessor :current_type, :declared_current_type, :current_set,
                        :segment_parent_types, :segment_parent_sets,
                        :bool_clauses, :unknown_gates, :route, :spreads

          def initialize(current_type:, declared_current_type:, current_set:,
                         segment_parent_types:, segment_parent_sets:,
                         bool_clauses:, unknown_gates:, route:, spreads:)
            @current_type = current_type
            @declared_current_type = declared_current_type
            @current_set = current_set
            @segment_parent_types = segment_parent_types
            @segment_parent_sets = segment_parent_sets
            @bool_clauses = bool_clauses
            @unknown_gates = unknown_gates
            @route = route
            @spreads = spreads
          end

          def enter_field(bool:, unknown:, label:)
            PathState.new(
              current_type: @current_type,
              declared_current_type: @declared_current_type,
              current_set: @current_set,
              segment_parent_types: @segment_parent_types.dup,
              segment_parent_sets: @segment_parent_sets.dup,
              bool_clauses: @bool_clauses + bool,
              unknown_gates: @unknown_gates + unknown,
              route: @route + [label],
              # Fragment cycles cannot cross a field boundary in a way that
              # re-enters the same chain; reset the spread guard.
              spreads: Set.new,
            )
          end

          def branch(bool:, unknown:, label:)
            PathState.new(
              current_type: @current_type,
              declared_current_type: @declared_current_type,
              current_set: @current_set,
              segment_parent_types: @segment_parent_types.dup,
              segment_parent_sets: @segment_parent_sets.dup,
              bool_clauses: bool,
              unknown_gates: @unknown_gates + unknown,
              route: @route + [label],
              spreads: @spreads,
            )
          end

          def snapshot
            {
              segment_parent_types: @segment_parent_types,
              segment_parent_sets: @segment_parent_sets,
              bool_clauses: @bool_clauses,
              unknown_gates: @unknown_gates,
              route: @route,
            }
          end
        end
      end
    end
  end
end
