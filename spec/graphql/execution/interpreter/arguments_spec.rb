# frozen_string_literal: true
require "spec_helper"

describe "GraphQL::Execution::Interpreter::Arguments" do
  class InterpreterArgsTestSchema < GraphQL::Schema
    class SearchParams < GraphQL::Schema::InputObject
      argument :query, String, required: false
    end

    class Query < GraphQL::Schema::Object
      field :search, [String], null: false do
        argument :params, SearchParams, required: false
        argument :limit, Int
      end
    end

    query(Query)
  end

  def arguments(query_str)
    query = GraphQL::Query.new(InterpreterArgsTestSchema, query_str)
    ast_node = query.document.definitions.first.selections.first
    query_type = query.get_type("Query")
    field = query.get_field(query_type, "search")
    query.arguments_for(ast_node, field)
  end

  it "provides .dig" do
    query_str = <<-GRAPHQL
    {
      search(limit: 10, params: { query: "abcde" } )
    }
    GRAPHQL
    args = arguments(query_str)
    assert_equal 10, args.dig(:limit)
    assert_equal "abcde", args.dig(:params, :query)
    assert_nil args.dig(:nothing)
    assert_nil args.dig(:params, :nothing)
    assert_nil args.dig(:nothing, :nothing, :nothing)
  end

  it "is frozen, and so are its constituent hashes" do
    query_str = <<-GRAPHQL
    {
      search(limit: 10, params: { query: "abcde" } )
    }
    GRAPHQL
    args = arguments(query_str)

    assert args.frozen?
    assert args.argument_values.frozen?
    assert args.keyword_arguments.frozen?
  end

  describe "ArgumentValue source" do
    class ArgumentSourceSchema < GraphQL::Schema
      class Thing < GraphQL::Schema::Resolver
        type String, null: false
        extras [:argument_details]
        argument :status, String, required: false, default_value: "active"

        def resolve(argument_details:, status:)
          value = argument_details.argument_values[:status]
          [
            status.inspect,
            value.source.to_s,
            value.default_used?.to_s,
            value.null?.to_s,
            value.provided?.to_s,
          ].join("|")
        end
      end

      class NullDefaultThing < GraphQL::Schema::Resolver
        type String, null: false
        extras [:argument_details]
        graphql_name "NullDefaultThing"
        argument :status, String, required: false, default_value: nil

        def resolve(argument_details:, status:)
          argument_details.argument_values[:status].source.to_s
        end
      end

      class ReplaceNullThing < GraphQL::Schema::Resolver
        type String, null: false
        extras [:argument_details]
        graphql_name "ReplaceNullThing"
        argument :status, String, required: false, default_value: "active", replace_null_with_default: true

        def resolve(argument_details:, status:)
          argument_details.argument_values[:status].source.to_s
        end
      end

      class Query < GraphQL::Schema::Object
        field :thing, resolver: Thing
        field :null_default_thing, resolver: NullDefaultThing
        field :replace_null_thing, resolver: ReplaceNullThing
      end

      query(Query)
    end

    def source_for(query_str, field_name: "thing", variables: {})
      query = GraphQL::Query.new(ArgumentSourceSchema, query_str, variables: variables)
      ast_node = query.document.definitions.first.selections.first
      field = ArgumentSourceSchema.get_field("Query", field_name)
      query.arguments_for(ast_node, field).argument_values[:status]
    end

    it "distinguishes an omitted argument, an explicit null, and an explicit default" do
      omitted = source_for("{ thing }")
      explicit_null = source_for("{ thing(status: null) }")
      explicit_default = source_for('{ thing(status: "active") }')

      # The omitted argument falls back to the schema default
      assert_equal "active", omitted.value
      assert_equal GraphQL::Execution::Interpreter::ArgumentValue::DEFAULT_VALUE_SOURCE, omitted.source
      assert omitted.default_used?
      refute omitted.null?
      refute omitted.provided?

      # The explicit null is the client's decision, even though the value is nil
      assert_nil explicit_null.value
      assert_equal GraphQL::Execution::Interpreter::ArgumentValue::NULL_VALUE_SOURCE, explicit_null.source
      refute explicit_null.default_used?
      assert explicit_null.null?
      refute explicit_null.provided?

      # The explicitly-provided default keeps the same value but a different source
      assert_equal "active", explicit_default.value
      assert_equal GraphQL::Execution::Interpreter::ArgumentValue::PROVIDED_VALUE_SOURCE, explicit_default.source
      refute explicit_default.default_used?
      refute explicit_default.null?
      assert explicit_default.provided?
    end

    it "keeps the three sources distinct through a resolver even when the final values match" do
      result = ArgumentSourceSchema.execute(<<~GRAPHQL)
      {
        omitted: thing
        explicitNull: thing(status: null)
        explicitDefault: thing(status: "active")
      }
      GRAPHQL
      data = result.to_h["data"]

      assert_equal '"active"|default_value|true|false|false', data["omitted"]
      assert_equal 'nil|null|false|true|false', data["explicitNull"]
      assert_equal '"active"|provided|false|false|true', data["explicitDefault"]
    end

    it "distinguishes an omitted nil default from an explicit null" do
      omitted = source_for("{ nullDefaultThing }", field_name: "nullDefaultThing")
      explicit_null = source_for("{ nullDefaultThing(status: null) }", field_name: "nullDefaultThing")

      assert_nil omitted.value
      assert_nil explicit_null.value
      assert_equal :default_value, omitted.source
      assert_equal :null, explicit_null.source
    end

    it "treats a null replaced by the default as a default-sourced value" do
      replaced = source_for("{ replaceNullThing(status: null) }", field_name: "replaceNullThing")
      assert_equal "active", replaced.value
      assert_equal :default_value, replaced.source
      assert replaced.default_used?
    end

    it "reports variable-provided values with the same three-way source information" do
      query_str = <<~GRAPHQL
        query($status: String) { thing(status: $status) }
      GRAPHQL

      omitted = source_for(query_str, variables: {})
      explicit_null = source_for(query_str, variables: { "status" => nil })
      explicit_default = source_for(query_str, variables: { "status" => "active" })

      assert_equal :default_value, omitted.source
      assert_equal :null, explicit_null.source
      assert_equal :provided, explicit_default.source
    end

    describe "across a multiplex sharing one AST document" do
      let(:shared_document) do
        GraphQL.parse(<<~GRAPHQL)
          query($status: String) {
            thing(status: $status)
          }
        GRAPHQL
      end

      it "keeps the prepared state for omitted, null, and explicit default inputs distinct" do
        results = ArgumentSourceSchema.multiplex([
          { document: shared_document, variables: {} },
          { document: shared_document, variables: { "status" => nil } },
          { document: shared_document, variables: { "status" => "active" } },
        ])

        sources = results.map do |result|
          query = result.query
          ast_node = shared_document.definitions.first.selections.first
          field = ArgumentSourceSchema.get_field("Query", "thing")
          query.arguments_for(ast_node, field).argument_values[:status].source
        end

        assert_equal [:default_value, :null, :provided], sources

        # The omitted and explicitly-provided default end up with the same Ruby value,
        # while the explicit null resolves to nil.
        values = results.map { |r| r.to_h["data"]["thing"].split("|").first }
        assert_equal ['"active"', "nil", '"active"'], values
      end

      it "gives each query its own arguments cache so the sources don't leak between queries" do
        results = ArgumentSourceSchema.multiplex([
          { document: shared_document, variables: {} },
          { document: shared_document, variables: { "status" => nil } },
          { document: shared_document, variables: { "status" => "active" } },
        ])

        caches = results.map { |r| r.query.arguments_cache }
        refute_same caches[0], caches[1]
        refute_same caches[1], caches[2]
      end
    end

    describe "argument caching" do
      it "reuses the prepared arguments when the same AST node is fetched more than once" do
        query = GraphQL::Query.new(
          ArgumentSourceSchema,
          '{ thing(status: "active") }',
        )
        ast_node = query.document.definitions.first.selections.first
        field = ArgumentSourceSchema.get_field("Query", "thing")

        first = query.arguments_for(ast_node, field)
        second = query.arguments_for(ast_node, field)

        assert_same first, second
        assert_equal :provided, first.argument_values[:status].source
      end

      it "does not merge cache entries whose values match but whose sources differ" do
        query = GraphQL::Query.new(ArgumentSourceSchema, <<~GRAPHQL)
        {
          omitted: thing
          explicitDefault: thing(status: "active")
        }
        GRAPHQL
        field = ArgumentSourceSchema.get_field("Query", "thing")
        omitted_node, explicit_node = query.document.definitions.first.selections

        omitted = query.arguments_for(omitted_node, field)
        explicit = query.arguments_for(explicit_node, field)

        # Both end up with the same Ruby value...
        assert_equal "active", omitted[:status]
        assert_equal "active", explicit[:status]
        # ...but the prepared arguments are distinct and keep their own source
        refute_same omitted, explicit
        assert_equal :default_value, omitted.argument_values[:status].source
        assert_equal :provided, explicit.argument_values[:status].source
      end
    end
  end
end
