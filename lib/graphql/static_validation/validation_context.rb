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
        :max_errors, :types, :schema, :work_gate


      def_delegators :@query, :document, :fragments, :operations

      # @param work_gate [#charge_work(units)] Object accounting for validation
      #   work as the document is checked. It's invoked before each documented
      #   unit of rule work (each AST node hook entry and each fragment
      #   dependency resolution step). A pausable session uses it to suspend
      #   when its work budget is exhausted; ordinary validation uses a no-op
      #   gate.
      def initialize(query, visitor_class, max_errors, work_gate: NullWorkGate)
        @query = query
        @types = query.types # TODO update migrated callers to use this accessor
        @schema = query.schema
        @literal_validator = LiteralValidator.new(context: query.context)
        @errors = []
        @max_errors = max_errors || Float::INFINITY
        @on_dependency_resolve_handlers = []
        @work_gate = work_gate
        @visitor = visitor_class.new(document, self)
      end

      # Account for `units` units of rule work. See {#work_gate}.
      def charge_work(units = 1)
        @work_gate.charge_work(units)
      end

      # The default work gate: validation runs straight through without pausing.
      module NullWorkGate
        def self.charge_work(_units = 1)
          nil
        end
      end

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
