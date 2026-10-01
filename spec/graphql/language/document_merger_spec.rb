# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Language::DocumentMerger do
  def merge(*pairs)
    GraphQL::Language::DocumentMerger.merge(pairs.map { |source, sdl| [source, GraphQL.parse(sdl)] })
  end

  describe "merging documents with mutual forward references" do
    let(:users_sdl) do
      <<~GRAPHQL
        type Query {
          user: User
        }

        type User {
          name: String
          posts: [Post!]
        }
      GRAPHQL
    end

    let(:posts_sdl) do
      <<~GRAPHQL
        type Post {
          title: String
          author: User
          comments(first: Int = 10): [Comment!]
        }

        type Comment {
          body: String
          post: Post
        }
      GRAPHQL
    end

    it "produces a document that the schema builder can consume" do
      merged = merge(["users.graphql", users_sdl], ["posts.graphql", posts_sdl])

      assert_instance_of GraphQL::Language::Nodes::Document, merged
      assert_equal ["Query", "User", "Post", "Comment"], merged.definitions.map(&:name)

      schema = GraphQL::Schema.from_definition(merged.to_query_string)
      assert_equal ["Comment", "Post", "Query", "User"], (schema.types.keys & ["Query", "User", "Post", "Comment"]).sort
      assert_equal "User", schema.types["Post"].fields["author"].type.unwrap.graphql_name
      assert_equal "User", schema.types["Query"].fields["user"].type.unwrap.graphql_name
      assert_equal 10, schema.types["Post"].fields["comments"].arguments["first"].default_value
    end

    it "also works when handed a source => document Hash" do
      merged = GraphQL::Language::DocumentMerger.merge(
        "users.graphql" => GraphQL.parse(users_sdl),
        "posts.graphql" => GraphQL.parse(posts_sdl),
      )
      assert_equal ["Query", "User", "Post", "Comment"], merged.definitions.map(&:name)
    end
  end

  describe "merging extensions" do
    it "folds valid extensions into their definitions" do
      merged = merge(
        ["a.graphql", "type Query { a: String }"],
        ["b.graphql", "extend type Query { b: Int }"],
      )

      query_defn = merged.definitions.find { |d| d.respond_to?(:name) && d.name == "Query" }
      assert_equal ["a", "b"], query_defn.fields.map(&:name)
      assert merged.definitions.none? { |d| d.is_a?(GraphQL::Language::Nodes::ObjectTypeExtension) }

      schema = GraphQL::Schema.from_definition(merged.to_query_string)
      assert_equal ["a", "b"], schema.types["Query"].fields.keys
    end

    it "accepts identical duplicate fields" do
      merged = merge(
        ["a.graphql", "type Query { f(x: Int = 1): String }"],
        ["b.graphql", "extend type Query { f(x: Int = 1): String }"],
      )
      query_defn = merged.definitions.first
      assert_equal ["f"], query_defn.fields.map(&:name)
    end

    it "rejects an extension whose field type conflicts after the merge" do
      doc_a = GraphQL.parse("type Query {\n  version: String\n}")
      doc_b = GraphQL.parse("extend type Query {\n  version: Int\n}")
      before_a = doc_a.to_query_string
      before_b = doc_b.to_query_string

      err = assert_raises(GraphQL::Language::DocumentMerger::MergeError) do
        GraphQL::Language::DocumentMerger.merge([["a.graphql", doc_a], ["b.graphql", doc_b]])
      end

      assert_includes err.message, "Query.version"
      assert_includes err.message, "String"
      assert_includes err.message, "Int"
      assert_includes err.message, "a.graphql (line 2, column 3)"
      assert_includes err.message, "b.graphql (line 2, column 3)"

      # The input ASTs were not modified:
      assert_equal before_a, doc_a.to_query_string
      assert_equal before_b, doc_b.to_query_string
      assert_equal 1, doc_a.definitions.first.fields.size
    end

    it "rejects conflicting argument default values" do
      err = assert_raises(GraphQL::Language::DocumentMerger::MergeError) do
        merge(
          ["a.graphql", "type Query { f(x: Int = 1): String }"],
          ["b.graphql", "extend type Query { f(x: Int = 2): String }"],
        )
      end
      assert_includes err.message, "default values"
      assert_includes err.message, "a.graphql"
      assert_includes err.message, "b.graphql"
    end

    it "rejects extensions of undefined types, even across documents" do
      err = assert_raises(GraphQL::Language::DocumentMerger::MergeError) do
        merge(
          ["a.graphql", "type Query { a: String }"],
          ["b.graphql", "extend type Missing { b: Int }"],
        )
      end
      assert_includes err.message, "Missing"
      assert_includes err.message, "b.graphql"
    end

    it "rejects extensions with the wrong kind" do
      err = assert_raises(GraphQL::Language::DocumentMerger::MergeError) do
        merge(
          ["a.graphql", "interface Node { id: ID! } type Query { node: Node }"],
          ["b.graphql", "extend type Node { extra: String }"],
        )
      end
      assert_includes err.message, "Node"
    end

    it "merges enum values, union members, interfaces, and input fields from extensions" do
      merged = merge(
        ["a.graphql", <<~GRAPHQL],
          interface Node { id: ID! }
          type Query { node: Node }
          type Photo { url: String }
          type Video { url: String }
          enum Color { RED }
          union Media = Photo
          input Filter { term: String }
        GRAPHQL
        ["b.graphql", <<~GRAPHQL],
          extend enum Color { BLUE }
          extend union Media = Video
          extend interface Node { nid: ID }
          extend input Filter { limit: Int }
        GRAPHQL
      )

      schema = GraphQL::Schema.from_definition(merged.to_query_string)
      assert_equal ["RED", "BLUE"], schema.types["Color"].values.keys
      assert_equal ["Photo", "Video"], schema.types["Media"].possible_types.map(&:graphql_name).sort
      assert_equal ["id", "nid"], schema.types["Node"].fields.keys
      assert_equal ["limit", "term"], schema.types["Filter"].arguments.keys.sort
    end
  end

  describe "duplicate definitions" do
    it "rejects types defined in two documents" do
      err = assert_raises(GraphQL::Language::DocumentMerger::MergeError) do
        merge(
          ["a.graphql", "type User { id: ID }"],
          ["b.graphql", "type User { id: ID }"],
        )
      end
      assert_includes err.message, "User"
      assert_includes err.message, "a.graphql"
      assert_includes err.message, "b.graphql"
    end

    it "rejects the same name defined with different kinds" do
      err = assert_raises(GraphQL::Language::DocumentMerger::MergeError) do
        merge(
          ["a.graphql", "type Thing { id: ID }"],
          ["b.graphql", "input Thing { id: ID }"],
        )
      end
      assert_includes err.message, "object type"
      assert_includes err.message, "input object"
    end
  end

  describe "root operation types" do
    it "merges compatible schema definitions and extensions" do
      merged = merge(
        ["a.graphql", "schema { query: Query } type Query { a: String }"],
        ["b.graphql", "extend schema { mutation: Mutation } type Mutation { b: String }"],
      )

      schema_defn = merged.definitions.find { |d| d.is_a?(GraphQL::Language::Nodes::SchemaDefinition) }
      assert_equal "Query", schema_defn.query
      assert_equal "Mutation", schema_defn.mutation

      schema = GraphQL::Schema.from_definition(merged.to_query_string)
      assert_equal "Query", schema.query.graphql_name
      assert_equal "Mutation", schema.mutation.graphql_name
    end

    it "rejects conflicting root operation types" do
      err = assert_raises(GraphQL::Language::DocumentMerger::MergeError) do
        merge(
          ["a.graphql", "schema {\n  query: Query\n}\ntype Query { a: String }"],
          ["b.graphql", "extend schema {\n  query: RootQuery\n}\ntype RootQuery { b: String }"],
        )
      end
      assert_includes err.message, "query"
      assert_includes err.message, "Query"
      assert_includes err.message, "RootQuery"
      assert_includes err.message, "a.graphql (line 1, column 1)"
      assert_includes err.message, "b.graphql (line 1, column 1)"
    end

    it "rejects two schema definitions" do
      err = assert_raises(GraphQL::Language::DocumentMerger::MergeError) do
        merge(
          ["a.graphql", "schema { query: Query } type Query { a: String }"],
          ["b.graphql", "schema { query: Query }"],
        )
      end
      assert_includes err.message, "schema is defined more than once"
    end
  end

  describe "directive definitions" do
    it "deduplicates identical directive definitions" do
      merged = merge(
        ["a.graphql", "directive @tag(name: String = \"x\") on FIELD_DEFINITION | OBJECT\ntype Query { a: String }"],
        ["b.graphql", "directive @tag(name: String = \"x\") on OBJECT | FIELD_DEFINITION"],
      )
      directives = merged.definitions.select { |d| d.is_a?(GraphQL::Language::Nodes::DirectiveDefinition) }
      assert_equal 1, directives.size
    end

    it "rejects directive definitions with different arguments" do
      err = assert_raises(GraphQL::Language::DocumentMerger::MergeError) do
        merge(
          ["a.graphql", "directive @tag(name: String = \"x\") on OBJECT"],
          ["b.graphql", "directive @tag(name: String = \"y\") on OBJECT"],
        )
      end
      assert_includes err.message, "@tag"
      assert_includes err.message, "a.graphql"
      assert_includes err.message, "b.graphql"
    end

    it "rejects directive definitions with different locations" do
      err = assert_raises(GraphQL::Language::DocumentMerger::MergeError) do
        merge(
          ["a.graphql", "directive @tag on OBJECT"],
          ["b.graphql", "directive @tag on FIELD_DEFINITION"],
        )
      end
      assert_includes err.message, "locations"
    end
  end

  describe "input handling" do
    it "raises ArgumentError for non-document inputs" do
      assert_raises(ArgumentError) do
        GraphQL::Language::DocumentMerger.merge([["a.graphql", "type Query { a: String }"]])
      end
    end

    it "raises ArgumentError when no documents are given" do
      assert_raises(ArgumentError) do
        GraphQL::Language::DocumentMerger.merge([])
      end
    end

    it "rejects executable definitions" do
      err = assert_raises(GraphQL::Language::DocumentMerger::MergeError) do
        merge(["a.graphql", "query { __typename }"])
      end
      assert_includes err.message, "not a type system definition"
    end
  end
end
