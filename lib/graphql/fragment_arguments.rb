# frozen_string_literal: true

require "graphql"
require "graphql/language"
require "graphql/query"
require "graphql/execution_error"
require "graphql/language/nodes"
require "graphql/fragment_arguments/parser"
require "graphql/fragment_arguments/expander"

module GraphQL
  # {FragmentArguments} is a project extension to GraphQL-Ruby.
  #
  # It is _not_ part of the current GraphQL specification and the syntax it
  # accepts is rejected by a stock parser. Don't use it for interoperable
  # documents.
  #
  # When enabled on a schema, fragments may declare typed local parameters
  # with default values, and fragment spreads may pass arguments:
  #
  #   fragment Page($first: Int = 10, $after: String) on Connection {
  #     edges(first: $first, after: $after) { node { id } }
  #   }
  #
  #   query($cursor: String) {
  #     friends {
  #       ...Page(first: 5)
  #       ...Page(first: 20, after: $cursor)
  #     }
  #   }
  #
  # Before validation/execution each parameterized spread is converted into a
  # plain inline fragment with its parameters substituted by the provided
  # values (or defaults), so the rest of the execution pipeline is unchanged.
  #
  # Scoping rules:
  # - fragment-local names never capture operation variables (and vice versa);
  # - every spread is expanded independently, so spreading the same fragment
  #   with two argument sets can never share an expansion.
  #
  # Enable it per schema:
  #
  #   class MySchema < GraphQL::Schema
  #     use GraphQL::FragmentArguments
  #   end
  module FragmentArguments
    # Parse a query string using the extension-aware parser.
    # @return [GraphQL::Language::Nodes::Document]
    def self.parse(query_string, filename: nil, trace: GraphQL::Tracing::NullTrace, max_tokens: nil)
      Parser.parse(query_string, filename: filename, trace: trace, max_tokens: max_tokens)
    end

    # Run the desugaring pass on a document (parsed with {Parser}).
    #
    # @param document [GraphQL::Language::Nodes::Document]
    # @param context [GraphQL::Query::Context]
    # @return [Array(GraphQL::Language::Nodes::Document, Array<Expander::Error>)]
    def self.expand(document, context: nil)
      expander = Expander.new(document, context: context)
      transformed = expander.transform
      [transformed, expander.errors]
    end

    # Schema-level configuration and query integration.
    module SchemaExtension
      # Enable fragment parameters on this schema.
      def fragment_arguments(new_value = nil)
        if new_value.nil?
          if defined?(@fragment_arguments)
            @fragment_arguments
          elsif superclass.respond_to?(:fragment_arguments)
            superclass.fragment_arguments
          else
            false
          end
        else
          @fragment_arguments = new_value
        end
      end

      def fragment_arguments?
        !!fragment_arguments
      end
    end

    # Mixed into {GraphQL::Query} once; the per-schema gate keeps the feature
    # inactive for schemas that didn't enable it.
    module QueryIntegration
      module PrependedMethods
        private

        def prepare_ast
          # Use the raw @query_string ivar (not the #query_string method, which
          # can derive a string from a provided document) to avoid re-entering
          # preparation when queries come in as documents.
          if @schema.respond_to?(:fragment_arguments?) && @schema.fragment_arguments? && @query_string && @document.nil?
            prepare_fragment_arguments_ast
          else
            super
          end
        end

        def prepare_fragment_arguments_ast
          @prepared_ast = true
          @warden ||= @schema.warden_class.new(schema: @schema, context: @context)
          parse_error = nil

          raw_document = begin
            GraphQL::FragmentArguments::Parser.parse(
              query_string,
              trace: self.current_trace,
              max_tokens: @schema.max_query_string_tokens
            )
          rescue GraphQL::ParseError => err
            parse_error = err
            @schema.parse_error(err, @context)
            nil
          end

          if raw_document
            if GraphQL::FragmentArguments::Expander.parameterized?(raw_document)
              transformed_document, expansion_errors = GraphQL::FragmentArguments.expand(raw_document, context: @context)
              # Variable names that the expander rejected for type/undefined
              # reasons; the stock "declared but not used" rule would also fire
              # for these, so suppress that duplicate.
              rejected_variable_names = expansion_errors.flat_map do |err|
                (err.respond_to?(:variable_name) && err.variable_name) || []
              end
              expansion_errors.each do |err|
                exec_err = if err.ast_node
                  GraphQL::ExecutionError.new(err.message, ast_node: err.ast_node)
                else
                  GraphQL::ExecutionError.new(err.message)
                end
                @context.add_error(exec_err)
              end
              if rejected_variable_names.any?
                @context.namespace(:fragment_arguments)[:rejected_variable_names] = rejected_variable_names
              end
              @document = transformed_document
            else
              # The extension parser accepted it but no parameter syntax was
              # used; the document is identical in shape to a standard parse.
              @document = raw_document
            end
          end

          @fragments = {}
          @operations = {}
          if @document
            @document.definitions.each do |part|
              case part
              when GraphQL::Language::Nodes::FragmentDefinition
                @fragments[part.name] = part
              when GraphQL::Language::Nodes::OperationDefinition
                @operations[part.name] = part
              end
            end
          end

          @ast_variables = []
          @mutation = false
          @subscription = false
          operation_name_error = nil
          if !@operations.empty?
            @selected_operation = find_operation(@operations, @operation_name)
            if @selected_operation.nil?
              operation_name_error = GraphQL::Query::OperationNameMissingError.new(@operation_name)
            else
              if @operation_name.nil?
                @operation_name = @selected_operation.name
              end
              @ast_variables = @selected_operation.variables
              @mutation = @selected_operation.operation_type == "mutation"
              @query = @selected_operation.operation_type == "query"
              @subscription = @selected_operation.operation_type == "subscription"
            end
          end

          @validation_pipeline = GraphQL::Query::ValidationPipeline.new(
            query: self,
            parse_error: parse_error,
            operation_name_error: operation_name_error,
            max_depth: @max_depth,
            max_complexity: @max_complexity
          )
        end
      end
    end

    module ValidationPipelineIntegration
      def validation_errors
        errors = super
        rejected = @query.context.namespace(:fragment_arguments)[:rejected_variable_names]
        return errors if rejected.nil? || rejected.empty?
        errors.reject do |err|
          err.is_a?(GraphQL::StaticValidation::VariablesAreUsedAndDefinedError) &&
            err.code == "variableNotUsed" &&
            rejected.include?(err.variable_name)
        end
      end
    end

    @query_integration_installed = false

    # `use GraphQL::FragmentArguments` hook. Pass `enabled: false` to install
    # the parser integration while keeping the syntax inactive on this schema.
    def self.use(schema, enabled: true, **_options)
      unless @query_integration_installed
        GraphQL::Query.prepend(QueryIntegration::PrependedMethods)
        GraphQL::Query::ValidationPipeline.prepend(ValidationPipelineIntegration)
        @query_integration_installed = true
      end
      schema.singleton_class.prepend(SchemaExtension) unless schema.singleton_class.ancestors.include?(SchemaExtension)
      schema.fragment_arguments(enabled)
      schema
    end
  end
end
