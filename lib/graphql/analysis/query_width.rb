# frozen_string_literal: true
module GraphQL
  module Analysis
    # A query reducer for measuring the *response width* of a given query:
    # the maximum number of distinct response keys that could end up in a single
    # response object.
    #
    # Unlike counting field nodes in the AST, this accounts for:
    #
    # - repeated selections of the same response key (eg. via many fragment spreads),
    #   which are merged into one key in the response;
    # - `... on SomeType` type conditions on abstract types, where mutually
    #   exclusive branches can never coexist (the reachable maximum is used);
    # - `@skip` / `@include` directives whose condition is known from query
    #   variables (skipped selections don't count).
    #
    # @example Logging the width of a query
    #   class LogQueryWidth < GraphQL::Analysis::QueryWidth
    #     def result
    #       log("GraphQL query width: #{max_width}")
    #     end
    #   end
    class QueryWidth < Analyzer
      # @return [Integer] the maximum number of response keys any object in the response may contain
      attr_reader :max_width

      # @return [Array<String>] response path to the object whose width is {#max_width}
      attr_reader :max_width_path

      # @return [GraphQL::BaseType] the object type on which {#max_width} is reached
      attr_reader :max_width_type

      def initialize(query)
        @max_width = 0
        @max_width_path = []
        @max_width_type = nil
        @count_introspection_fields = query.schema.count_introspection_fields
        # Stack of selection sets currently being visited. Each scope owns its
        # own stack of enclosing type conditions, so an outer fragment condition
        # does not leak into a nested field's selection set (that field resolves
        # to a fresh runtime object).
        @scopes = []
        @possible_types_cache = {}.compare_by_identity
        super
      end

      def on_enter_operation_definition(node, parent, visitor)
        @scopes << Scope.new(visitor.object_types.last, visitor, self)
      end

      def on_leave_operation_definition(node, parent, visitor)
        pop_scope(visitor, [])
      end

      def on_enter_field(node, parent, visitor)
        return if visitor.skipping?
        field_defn = visitor.field_definition
        return if field_defn.nil?
        return if @count_introspection_fields == false && field_defn.introspection?

        @scopes.last.add(node.alias || node.name)

        object_type = field_defn.type.unwrap
        if object_type.kind.fields?
          @scopes << Scope.new(object_type, visitor, self)
        end
      end

      def on_leave_field(node, parent, visitor)
        return if visitor.skipping?
        field_defn = visitor.field_definition
        return if field_defn.nil?
        return if @count_introspection_fields == false && field_defn.introspection?

        object_type = field_defn.type.unwrap
        if object_type.kind.fields?
          # The current field's response key is the path to the object whose
          # selection set is now complete.
          pop_scope(visitor, visitor.response_path)
        end
      end

      def on_enter_inline_fragment(node, parent, visitor)
        @scopes.last.push_conditions(visitor.object_types.last, visitor, self)
      end

      def on_leave_inline_fragment(node, parent, visitor)
        @scopes.last.pop_conditions
      end

      def on_enter_fragment_spread(node, parent, visitor)
        @scopes.last.push_conditions(visitor.object_types.last, visitor, self)
      end

      def on_leave_fragment_spread(node, parent, visitor)
        @scopes.last.pop_conditions
      end

      def result
        @max_width
      end

      # @api private
      def possible_types_for(visitor, type)
        @possible_types_cache[type] ||= visitor.query.types.possible_types(type).to_set
      end

      private

      def pop_scope(visitor, path)
        width, widest_type = @scopes.pop.width!
        if width > @max_width
          @max_width = width
          @max_width_path = path
          @max_width_type = widest_type
        end
        width
      end

      # A selection set on a particular object type. Gathers the response keys
      # contributed by each possible branch of fragment type conditions, then
      # reports the maximum key count reachable for a single runtime object.
      class Scope
        def initialize(object_type, visitor, analyzer)
          @object_type = object_type
          @visitor = visitor
          @analyzer = analyzer
          # @entries: Array of [key, condition_set_or_nil]
          @entries = []
          # Stack of type-condition sets surrounding the current node in this
          # selection set. nil means "unconditional".
          @condition_stack = [nil]
        end

        def add(key)
          @entries << [key, @condition_stack.last]
        end

        def push_conditions(condition_type, visitor, analyzer)
          enclosing = @condition_stack.last
          @condition_stack << intersect_conditions(condition_type, enclosing, visitor, analyzer)
        end

        def pop_conditions
          @condition_stack.pop
        end

        # @return [Array(Integer, GraphQL::BaseType)] the width and the concrete
        #   object type that reaches it
        def width!
          candidates = if @object_type.kind.object?
            [@object_type]
          else
            @analyzer.possible_types_for(@visitor, @object_type).to_a
          end

          max_width = 0
          widest_type = @object_type
          candidates.each do |candidate_type|
            candidate_types = @analyzer.possible_types_for(@visitor, candidate_type)
            keys = Set.new
            @entries.each do |key, conditions|
              if conditions.nil? || conditions.intersect?(candidate_types)
                keys << key
              end
            end
            if keys.size > max_width
              max_width = keys.size
              widest_type = candidate_type
            end
          end
          [max_width, widest_type]
        end

        private

        # Restrict the enclosing conditions by a new type condition.
        # @return [Set, nil] nil means "unconditional / applies to every possible type"
        def intersect_conditions(condition_type, enclosing, visitor, analyzer)
          return enclosing if condition_type.nil?
          condition_types = analyzer.possible_types_for(visitor, condition_type)
          if enclosing.nil?
            condition_types.dup
          else
            enclosing & condition_types
          end
        end
      end
    end
  end
end
