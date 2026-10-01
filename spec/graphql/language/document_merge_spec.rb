# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Language::DocumentMerge do
  def merge(documents)
    GraphQL::Language::DocumentMerge.merge(documents)
  end

  describe "merging documents with mutual forward references" do
    let(:document_a) {
      GraphQL.parse(<<~GRAPHQL)
        schema {
          query: Query
        }

        type Query {
          # Refers to `B`, which is defined in the other document
          b: B
          greeting(name: String = "world"): String
        }
      GRAPHQL
    }

    let(:document_b) {
      GraphQL.parse(<<~GRAPHQL)
        # Refers back to `Query` and `A`, defined in the other document
        type B {
          a: A
          query: Query
        }

        type A {
          b: B
        }
      GRAPHQL
    }

    it "produces a single document with all definitions" do
      merged = merge("a.graphql" => document_a, "b.graphql" => document_b)
      assert_instance_of GraphQL::Language::Nodes::Document, merged
      type_names = merged.definitions.select { |d| d.respond_to?(:name) }.map(&:name)
      assert_equal ["A", "B", "Query"], type_names.sort
      schema_defns = merged.definitions.grep(GraphQL::Language::Nodes::SchemaDefinition)
      assert_equal 1, schema_defns.size
      assert_equal "Query", schema_defns.first.query
    end

    it "doesn't modify the original ASTs" do
      before_a = document_a.to_query_string
      before_b = document_b.to_query_string
      merge("a.graphql" => document_a, "b.graphql" => document_b)
      assert_equal before_a, document_a.to_query_string
      assert_equal before_b, document_b.to_query_string
    end

    it "can be consumed by the existing schema build entry point" do
      merged = merge("a.graphql" => document_a, "b.graphql" => document_b)
      schema = GraphQL::Schema.from_definition(merged.to_query_string)
      assert_includes schema.types.keys, "A"
      assert_includes schema.types.keys, "B"
      assert_includes schema.types.keys, "Query"
      assert_equal schema.types["B"], schema.types["Query"].fields["b"].type.unwrap
      assert_equal schema.types["A"], schema.types["B"].fields["a"].type.unwrap
      assert_equal "world", schema.types["Query"].fields["greeting"].arguments["name"].default_value
    end
  end

  describe "merging extensions" do
    let(:document_a) {
      GraphQL.parse(<<~GRAPHQL)
        directive @tag(name: String!) repeatable on OBJECT | FIELD_DEFINITION | SCALAR

        type Query implements Node {
          id: ID!
        }

        interface Node {
          id: ID!
        }

        enum Color {
          RED
        }

        union SearchResult = Query

        input Filter {
          term: String
        }

        scalar DateTime
      GRAPHQL
    }

    let(:document_b) {
      GraphQL.parse(<<~GRAPHQL)
        directive @tag(name: String!) repeatable on OBJECT | FIELD_DEFINITION | SCALAR

        extend type Query implements Named @tag(name: "root") {
          name: String
          id: ID!
        }

        interface Named {
          name: String
        }

        type Extra implements Named {
          name: String
        }

        extend enum Color {
          BLUE
        }

        extend union SearchResult = Extra

        extend input Filter {
          limit: Int = 10
        }

        extend scalar DateTime @tag(name: "scalar")
      GRAPHQL
    }

    it "folds extensions into their definitions" do
      merged = merge("a.graphql" => document_a, "b.graphql" => document_b)
      assert merged.definitions.none? { |d| d.class.name.include?("Extension") }

      query = merged.definitions.find { |d| d.respond_to?(:name) && d.name == "Query" }
      assert_equal ["id", "name"], query.fields.map(&:name)
      assert_equal ["Node", "Named"], query.interfaces.map(&:name)
      assert_equal ["tag"], query.directives.map(&:name)

      color = merged.definitions.find { |d| d.respond_to?(:name) && d.name == "Color" }
      assert_equal ["RED", "BLUE"], color.values.map(&:name)

      search_result = merged.definitions.find { |d| d.respond_to?(:name) && d.name == "SearchResult" }
      assert_equal ["Query", "Extra"], search_result.types.map(&:name)

      filter = merged.definitions.find { |d| d.respond_to?(:name) && d.name == "Filter" }
      assert_equal ["term", "limit"], filter.fields.map(&:name)
      assert_equal 10, filter.fields.last.default_value

      date_time = merged.definitions.find { |d| d.respond_to?(:name) && d.name == "DateTime" }
      assert_equal ["tag"], date_time.directives.map(&:name)

      # The identical directive definition appears only once:
      assert_equal 1, merged.definitions.grep(GraphQL::Language::Nodes::DirectiveDefinition).size
    end

    it "builds a working schema" do
      merged = merge("a.graphql" => document_a, "b.graphql" => document_b)
      schema = GraphQL::Schema.from_definition(merged.to_query_string)
      assert_equal ["id", "name"], schema.types["Query"].fields.keys
      ["Extra", "Filter", "Named", "Node", "Query", "SearchResult"].each do |type_name|
        assert_includes schema.types.keys, type_name
      end
      assert_equal 10, schema.types["Filter"].arguments["limit"].default_value
    end

    it "keeps the input documents unchanged" do
      before_a = document_a.to_query_string
      before_b = document_b.to_query_string
      merge("a.graphql" => document_a, "b.graphql" => document_b)
      assert_equal before_a, document_a.to_query_string
      assert_equal before_b, document_b.to_query_string
    end
  end

  describe "an extension that only conflicts after merging" do
    let(:document_a) {
      GraphQL.parse(<<~GRAPHQL)
        type Query {
          a: String
        }
      GRAPHQL
    }

    # Valid on its own -- the conflict only appears once the extension
    # is merged with the definition from the other document.
    let(:document_b) {
      GraphQL.parse(<<~GRAPHQL)
        extend type Query {
          a: Int
        }
      GRAPHQL
    }

    it "raises an error naming both documents and positions" do
      err = assert_raises(GraphQL::Language::DocumentMerge::MergeError) do
        merge("a.graphql" => document_a, "b.graphql" => document_b)
      end
      assert_includes err.message, 'Conflicting types for field "Query.a"'
      assert_includes err.message, '"String" (a.graphql:2:3)'
      assert_includes err.message, '"Int" (b.graphql:2:3)'
    end

    it "leaves the original ASTs unchanged" do
      before_a = document_a.to_query_string
      before_b = document_b.to_query_string
      assert_raises(GraphQL::Language::DocumentMerge::MergeError) do
        merge("a.graphql" => document_a, "b.graphql" => document_b)
      end
      assert_equal before_a, document_a.to_query_string
      assert_equal before_b, document_b.to_query_string
    end
  end

  describe "conflicting argument default values" do
    it "raises an error naming both declarations" do
      document_a = GraphQL.parse(<<~GRAPHQL)
        type Query {
          f(limit: Int = 10): Int
        }
      GRAPHQL
      document_b = GraphQL.parse(<<~GRAPHQL)
        extend type Query {
          f(limit: Int = 20): Int
        }
      GRAPHQL
      err = assert_raises(GraphQL::Language::DocumentMerge::MergeError) do
        merge("a.graphql" => document_a, "b.graphql" => document_b)
      end
      assert_includes err.message, 'Conflicting default values for argument "Query.f(limit:)"'
      assert_includes err.message, "10 (a.graphql:2:5)"
      assert_includes err.message, "20 (b.graphql:2:5)"
    end

    it "accepts matching defaults and one-sided defaults" do
      document_a = GraphQL.parse(<<~GRAPHQL)
        type Query {
          f(limit: Int = 10, term: String): Int
        }
      GRAPHQL
      document_b = GraphQL.parse(<<~GRAPHQL)
        extend type Query {
          f(limit: Int = 10, offset: Int = 0): Int
        }
      GRAPHQL
      merged = merge("a.graphql" => document_a, "b.graphql" => document_b)
      query = merged.definitions.find { |d| d.respond_to?(:name) && d.name == "Query" }
      field = query.fields.first
      assert_equal ["limit", "term", "offset"], field.arguments.map(&:name)
      assert_equal [10, nil, 0], field.arguments.map(&:default_value)
    end
  end

  describe "conflicting root operation types" do
    it "raises an error naming both schema blocks" do
      document_a = GraphQL.parse("schema { query: QueryA } type QueryA { a: String }")
      document_b = GraphQL.parse("extend schema { query: QueryB } type QueryB { b: String }")
      err = assert_raises(GraphQL::Language::DocumentMerge::MergeError) do
        merge("a.graphql" => document_a, "b.graphql" => document_b)
      end
      assert_includes err.message, 'Conflicting query root types: "QueryA" (a.graphql:1:1) and "QueryB" (b.graphql:1:1)'
    end

    it "merges complementary root operation types into one schema definition" do
      document_a = GraphQL.parse("schema { query: Query } type Query { a: String }")
      document_b = GraphQL.parse("extend schema { mutation: Mutation } type Mutation { b: String }")
      merged = merge("a.graphql" => document_a, "b.graphql" => document_b)
      schema_defns = merged.definitions.grep(GraphQL::Language::Nodes::SchemaDefinition)
      assert_equal 1, schema_defns.size
      assert_equal "Query", schema_defns.first.query
      assert_equal "Mutation", schema_defns.first.mutation
      schema = GraphQL::Schema.from_definition(merged.to_query_string)
      assert_equal schema.types["Mutation"], schema.mutation
    end
  end

  describe "directive rules" do
    it "rejects directive definitions with different locations" do
      document_a = GraphQL.parse("directive @tag on OBJECT\ntype Query { a: String }")
      document_b = GraphQL.parse("directive @tag on FIELD_DEFINITION\ntype B { b: String }")
      err = assert_raises(GraphQL::Language::DocumentMerge::MergeError) do
        merge("a.graphql" => document_a, "b.graphql" => document_b)
      end
      assert_includes err.message, 'Conflicting locations for directive "@tag"'
      assert_includes err.message, "a.graphql:1:1"
      assert_includes err.message, "b.graphql:1:1"
    end

    it "rejects directive definitions with different `repeatable`" do
      document_a = GraphQL.parse("directive @tag on OBJECT\ntype Query { a: String }")
      document_b = GraphQL.parse("directive @tag repeatable on OBJECT\ntype B { b: String }")
      err = assert_raises(GraphQL::Language::DocumentMerge::MergeError) do
        merge("a.graphql" => document_a, "b.graphql" => document_b)
      end
      assert_includes err.message, 'Conflicting `repeatable` for directive "@tag"'
    end

    it "rejects conflicting applications of a non-repeatable directive" do
      document_a = GraphQL.parse("directive @tag(name: String) on OBJECT\ntype Query @tag(name: \"a\") { a: String }")
      document_b = GraphQL.parse("extend type Query @tag(name: \"b\")")
      err = assert_raises(GraphQL::Language::DocumentMerge::MergeError) do
        merge("a.graphql" => document_a, "b.graphql" => document_b)
      end
      assert_includes err.message, "Conflicting @tag directives"
      assert_includes err.message, "a.graphql:2:51"
      assert_includes err.message, "b.graphql:1:19"
    end

    it "deduplicates identical applications and allows differing repeatable ones" do
      document_a = GraphQL.parse("directive @tag(name: String) repeatable on OBJECT\ntype Query @tag(name: \"a\") { a: String }")
      document_b = GraphQL.parse("directive @tag(name: String) repeatable on OBJECT\nextend type Query @tag(name: \"a\") @tag(name: \"b\")")
      merged = merge("a.graphql" => document_a, "b.graphql" => document_b)
      query = merged.definitions.find { |d| d.respond_to?(:name) && d.name == "Query" }
      assert_equal ["a", "b"], query.directives.map { |d| d.arguments.first.value }
    end
  end

  describe "duplicate type definitions" do
    it "raises an error naming both definitions" do
      document_a = GraphQL.parse("type Query {\n  a: String\n}")
      document_b = GraphQL.parse("type Query {\n  b: String\n}")
      err = assert_raises(GraphQL::Language::DocumentMerge::MergeError) do
        merge("a.graphql" => document_a, "b.graphql" => document_b)
      end
      assert_includes err.message, 'Type "Query" is defined more than once'
      assert_includes err.message, "a.graphql:1:1"
      assert_includes err.message, "b.graphql:1:1"
    end
  end

  describe "extensions without a matching definition" do
    it "raises an error" do
      document_a = GraphQL.parse("type Query { a: String }")
      document_b = GraphQL.parse("extend type Missing {\n  b: String\n}")
      err = assert_raises(GraphQL::Language::DocumentMerge::MergeError) do
        merge("a.graphql" => document_a, "b.graphql" => document_b)
      end
      assert_includes err.message, 'Cannot extend type "Missing" (b.graphql:1:1)'
    end

    it "raises an error when the extension kind doesn't match the definition" do
      document_a = GraphQL.parse("interface Thing {\n  a: String\n}")
      document_b = GraphQL.parse("extend type Thing {\n  b: String\n}")
      err = assert_raises(GraphQL::Language::DocumentMerge::MergeError) do
        merge("a.graphql" => document_a, "b.graphql" => document_b)
      end
      assert_includes err.message, 'Cannot extend interface type definition "Thing" (a.graphql:1:1) with a object type extension (b.graphql:1:1)'
    end
  end

  describe "unsupported definitions" do
    it "raises for executable definitions" do
      document_a = GraphQL.parse("type Query { a: String }")
      document_b = GraphQL.parse("{ a }")
      err = assert_raises(GraphQL::Language::DocumentMerge::MergeError) do
        merge("a.graphql" => document_a, "b.graphql" => document_b)
      end
      assert_includes err.message, "Can't merge OperationDefinition"
    end

    it "raises ArgumentError for non-document inputs" do
      err = assert_raises(ArgumentError) do
        merge("a.graphql" => "type Query { a: String }")
      end
      assert_includes err.message, "expected \"a.graphql\" to be a GraphQL::Language::Nodes::Document"
    end
  end
end
