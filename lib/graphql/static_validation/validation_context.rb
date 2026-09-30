# frozen_string_literal: true
module GraphQL
  module StaticValidation
    # The validation context gets passed to each validator.
    #
    # It exposes a {GraphQL::Language::Visitor} where validators may add hooks. ({Language::Visitor#visit} is called in {Validator#validate})
    #
    # It provides access to the schema & fragments which validators may read from.
    #
    # It holds a list of errors which each validator may add to.
    class ValidationContext
      extend Forwardable

      attr_reader :query, :errors, :visitor,
        :on_dependency_resolve_handlers,
        :max_errors, :types, :schema


      def_delegators :@query, :document, :fragments, :operations

      # @param session [GraphQL::StaticValidation::ValidationSession, nil] When this context
      #   belongs to a {ValidationSession}, the session which charges work units and
      #   enforces cancellation. `nil` for ordinary, non-pausable validation.
      def initialize(query, visitor_class, max_errors, session: nil)
        @query = query
        @types = query.types # TODO update migrated callers to use this accessor
        @schema = query.schema
        @literal_validator = LiteralValidator.new(context: query.context)
        @errors = []
        @max_errors = max_errors || Float::INFINITY
        @on_dependency_resolve_handlers = []
        @session = session
        @visitor = visitor_class.new(document, self)
      end

      # @return [GraphQL::StaticValidation::ValidationSession, nil]
      attr_reader :session

      # TODO stop using def_delegators because of Array allocations
      def_delegators :@visitor,
        :path, :type_definition, :field_definition, :argument_definition,
        :parent_type_definition, :directive_definition, :dependencies

      def on_dependency_resolve(&handler)
        @on_dependency_resolve_handlers << handler
      end

      def validate_literal(ast_value, type)
        @literal_validator.validate(ast_value, type)
      end

      # Charge `work_units` units of validation work to the running
      # {ValidationSession}, suspending the fiber driving the visit when the
      # caller's budget for the current {ValidationSession#advance} call is spent
      # (or when the session was cancelled).
      #
      # Every discrete piece of rule work (one AST node's hooks, one fragment
      # dependency resolution step, one field-merge comparison, ...) calls this,
      # so pausing is driven by the actual amount of checking done -- not by
      # wall-clock time measured at the end of the document.
      #
      # No-op (aside from honoring cancellation) for ordinary, non-session validation.
      # @return [void]
      def consume_work(work_units = 1)
        if @session
          @session.consume_work(work_units)
        end
      end

      # @return [Boolean] `true` when the owning {ValidationSession} was cancelled
      def cancelled?
        @session && @session.cancelled?
      end

      def too_many_errors?
        @errors.length >= @max_errors
      end

      def schema_directives
        @schema_directives ||= schema.directives
      end

      def did_you_mean_suggestion(name, options)
        if did_you_mean = schema.did_you_mean
          suggestions = did_you_mean::SpellChecker.new(dictionary: options).correct(name)
          case suggestions.size
          when 0
            ""
          when 1
            " (Did you mean `#{suggestions.first}`?)"
          else
            last_sugg = suggestions.pop
            " (Did you mean #{suggestions.map {|s| "`#{s}`"}.join(", ")} or `#{last_sugg}`?)"
          end
        end
      end
    end
  end
end
