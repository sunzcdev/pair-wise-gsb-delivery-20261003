# frozen_string_literal: true
module GraphQL
  module Analysis
    # A query reducer for measuring the *width* of a query, i.e. the number of
    # distinct response keys that may appear in a single response object.
    #
    # Unlike {QueryDepth} (which counts nested levels) or {QueryComplexity}
    # (which scores field cost), this reducer counts how wide a single object
    # in the response may get. Selections which can never be present in the
    # same response object are not added together:
    #
    # - Selections with the same response key (the same field, or aliases that
    #   agree on their response name) are merged and counted once.
    # - Repeated spreads of the same fragment are merged (the same way the
    #   response object would merge them) rather than counted per spread.
    # - Selections guarded by incompatible type conditions (e.g. different
    #   members of a union) are mutually exclusive, so only the widest reachable
    #   branch contributes.
    # - Fields removed by `@skip` / `@include` directives (using the query's
    #   known variables) are not counted.
    #
    # @example Logging the width of a query
    #   class LogQueryWidth < GraphQL::Analysis::QueryWidth
    #     def result
    #       log("GraphQL query width: #{max_width}")
    #     end
    #   end
    class QueryWidth < Analyzer
      # ScopedWidth models a tree of GraphQL types mapped to inner selections,
      # ie: Hash<GraphQL::BaseType, Hash<String, ScopedWidth>>
      class ScopedWidth < Hash
        # A proc for defaulting empty namespace requests as a new scope hash.
        DEFAULT_PROC = ->(h, k) { h[k] = {} }

        # @param parent_type [Class] The type on which this key was selected
        # @param field_definition [GraphQL::Schema::Field] The field backing this response key
        # @param query [GraphQL::Query] Used for resolving possible object types
        # @param response_path [Array<String>] The path to the response key for this scope
        def initialize(parent_type, field_definition, query, response_path)
          super(&DEFAULT_PROC)
          @field_definition = field_definition
          @query = query
          @response_path = response_path
        end

        # @return [Array<String>] The response path at which this key is selected
        attr_reader :response_path

        # @return [GraphQL::Schema::Field, nil] The field backing this response key
        attr_reader :field_definition

        # @return [GraphQL::Query] The query in which this key is selected
        attr_reader :query

        def composite?
          !empty?
        end
      end

      def initialize(subject)
        @skip_introspection_fields = !subject.schema.count_introspection_fields
        # One tree of scopes per query in an analyzed multiplex
        @scopes_by_query = {}
        @intersect_cache = Hash.new { |h, k| h[k] = {}.compare_by_identity }.compare_by_identity
        @possible_types_cache = {}.compare_by_identity
        @max_width = 0
        super
      end

      def on_enter_field(node, parent, visitor)
        # Fragment definitions are visited via their spreads.
        return if visitor.visiting_fragment_definition?
        # Fields removed by @skip / @include (given known variables) don't count.
        return if visitor.skipping?
        field_defn = field_definition(visitor, node)
        return if field_defn.nil?
        return if @skip_introspection_fields && field_defn.introspection?

        parent_type = visitor.parent_type_definition
        field_key = node.alias || node.name

        scopes_stack = @scopes_by_query[visitor.query] ||= [ScopedWidth.new(nil, nil, visitor.query, [])]
        scope = ScopedWidth.new(parent_type, field_defn, visitor.query, visitor.response_path)
        # A field selected directly in an abstract scope applies to every object
        # type that actually defines it; registering it once per applicable
        # parent type keeps mutually exclusive members separate.
        scope_parent_types(visitor, parent_type, node.name).each do |scope_parent_type|
          existing = scopes_stack.last[scope_parent_type][field_key]
          if existing.nil?
            scopes_stack.last[scope_parent_type][field_key] = scope
          else
            scope = existing
          end
        end
        scopes_stack.push(scope)
      end

      def on_leave_field(node, parent, visitor)
        return if visitor.visiting_fragment_definition?
        return if visitor.skipping?
        field_defn = field_definition(visitor, node)
        return if field_defn.nil?
        return if @skip_introspection_fields && field_defn.introspection?

        scopes_stack = @scopes_by_query[visitor.query]
        scopes_stack.pop
      end

      # Override this method to use the width result.
      # @return [Integer] The maximum number of simultaneously-present response keys in one object
      def result
        max_width
      end

      protected

      # @return [Integer] The widest object reachable anywhere in the query
      def max_width
        compute_width
        @max_width
      end

      # @return [Array<Array(Object)>] `[response_path, width, object_type]` triples for every object that exceeds the limit
      def width_violations(limit)
        compute_width
        @violations.select { |v| v[:width] > limit }
      end

      private

      # The AST visitor looks fields up directly on the parent type, which
      # returns nil for a field selected directly on an abstract type (e.g. a
      # field shared by objects in a union scope). Resolve it through the
      # possible object types in that case.
      def field_definition(visitor, node)
        field_defn = visitor.field_definition
        return field_defn unless field_defn.nil?

        parent_type = visitor.parent_type_definition
        return nil if parent_type.nil? || !parent_type.kind.abstract?

        visitor.query.types.possible_types(parent_type).each do |object_type|
          candidate = visitor.query.types.field(object_type, node.name)
          return candidate unless candidate.nil?
        end
        nil
      end

      # @return [Array<GraphQL::BaseType>] The parent types under which this
      #   field's scope should be registered: the parent type itself for object
      #   scopes, or every possible object type that defines the field for an
      #   abstract scope.
      def scope_parent_types(visitor, parent_type, field_name)
        return [parent_type] unless parent_type.kind.abstract?

        visitor.query.types.possible_types(parent_type).select do |object_type|
          !visitor.query.types.field(object_type, field_name).nil?
        end
      end

      def compute_width
        return if @computed
        @computed = true
        @violations = []
        @scopes_by_query.each_value do |scopes_stack|
          merged_width_for_scopes(scopes_stack.first.query, scopes_stack.first, [])
        end
      end

      # @param query [GraphQL::Query] The query whose selections are being measured
      # @param scope [ScopedWidth] A selection-holder keyed by parent type then response key
      # @param response_path [Array<String>] Path to the object described by `scope`
      # @return [Integer] The maximum number of response keys this object may contain
      def merged_width_for_scopes(query, scope, response_path)
        # Expand abstract types into their concrete object types; selections on
        # overlapping abstract types coalesce through their shared object types.
        possible_scope_types = {}
        scope.each_key { |t| possible_scope_types[t] = true }
        possible_scope_types.keys.each do |scope_type|
          if scope_type.kind.abstract?
            query.types.possible_types(scope_type).each { |impl_type| possible_scope_types[impl_type] ||= true }
            possible_scope_types.delete(scope_type)
          end
        end

        width = 0
        possible_scope_types.each_key do |possible_type|
          # Merge every selection branch that can apply to this object type.
          # Selections keyed by the same response key merge into one entry, so
          # repeated fragments / aliases of the same key are counted once.
          keys = {}
          scope.each do |scope_type, inner_selections|
            next unless types_intersect?(query, scope_type, possible_type)
            inner_selections.each do |field_key, child_scope|
              (keys[field_key] ||= []) << child_scope
            end
          end

          this_width = keys.size
          @max_width = this_width if this_width > @max_width
          @violations << { path: response_path, width: this_width, type: possible_type }

          keys.each do |field_key, child_scopes|
            # Only descend into composite selections; a composite key that also
            # appears as a leaf somewhere is rejected by FieldsWillMerge during
            # static validation, so any composite representative is sufficient.
            representative = child_scopes.find(&:composite?)
            next unless representative

            # Selections for the same key merge together, so evaluate their
            # combined selection set (and take the maximum over abstract branches).
            merged_width_for_scopes(query, reduce_scopes(query, child_scopes), response_path + [field_key])
          end

          width = this_width if this_width > width
        end
        width
      end

      # Combine several scopes (each keyed by parent type -> response key) into
      # one virtual scope, as the execution engine would do for selections that
      # share a response key.
      def reduce_scopes(query, scopes)
        merged = ScopedWidth.new(nil, nil, query, scopes.first.response_path)
        scopes.each do |scope|
          scope.each do |parent_type, inner_selections|
            inner_selections.each do |field_key, child_scope|
              merged[parent_type][field_key] = child_scope
            end
          end
        end
        merged
      end

      def types_intersect?(query, a, b)
        return true if a == b

        if a.object_id < b.object_id
          first_cache = @intersect_cache[a]
          second_key = b
        else
          first_cache = @intersect_cache[b]
          second_key = a
        end

        if first_cache.key?(second_key)
          first_cache[second_key]
        else
          a_types = @possible_types_cache[a] ||= query.types.possible_types(a).to_set
          b_types = @possible_types_cache[b] ||= query.types.possible_types(b).to_set
          first_cache[second_key] = a_types.intersect?(b_types)
        end
      end
    end
  end
end
