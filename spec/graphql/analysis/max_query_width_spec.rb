# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Analysis::MaxQueryWidth do
  class MaxWidthCheese < GraphQL::Schema::Object
    graphql_name "MaxWidthCheese"
    field :id, Integer, null: false
    field :flavor, String, null: true
    field :source, String, null: true
  end

  class MaxWidthMilk < GraphQL::Schema::Object
    graphql_name "MaxWidthMilk"
    field :id, Integer, null: false
    field :dairy, String, null: true
    field :brand, String, null: true
  end

  class MaxWidthProduct < GraphQL::Schema::Union
    graphql_name "MaxWidthProduct"
    possible_types MaxWidthCheese, MaxWidthMilk
  end

  class MaxWidthQuery < GraphQL::Schema::Object
    graphql_name "MaxWidthQuery"

    field :cheese, MaxWidthCheese, null: true do
      argument :id, Integer, required: true
    end
    field :product, MaxWidthProduct, null: true
    field :int_field, Integer, null: true

    def cheese(id:)
      self.class.resolved!
      { id: id }
    end

    def int_field
      self.class.resolved!
      1
    end

    def self.resolved!
      @resolver_calls = (@resolver_calls || 0) + 1
    end

    def self.resolver_calls
      @resolver_calls || 0
    end

    def self.reset!
      @resolver_calls = 0
    end
  end

  def build_schema(max_width: nil)
    mod = Module.new
    # Build the schema class dynamically so each spec gets its own config.
    Class.new(GraphQL::Schema) do
      query MaxWidthQuery

      if max_width
        max_width(max_width)
      end
    end
  end

  let(:schema) { build_schema }

  def analyze(schema, query_string, variables: {}, max_width: nil)
    query = GraphQL::Query.new(schema, query_string, variables: variables, **(max_width ? { max_width: max_width } : {}))
    GraphQL::Analysis.analyze_query(query, [GraphQL::Analysis::MaxQueryWidth]).first
  end

  it "returns nil when no max width is configured" do
    assert_nil analyze(schema, "{ #{(1..100).map { |i| "a#{i}: intField" }.join(" ")} }")
  end

  describe "repeated fragments vs. many distinct aliases" do
    let(:max_width) { 50 }
    let(:n) { 200 }

    # Thousands of AST field nodes spread across many fragment definitions, but
    # only a handful of distinct response keys: this stays narrow.
    let(:repeated_fragment_query) {
      spreads = (0...n).map { |i| "...F#{i}" }.join(" ")
      fragments = (0...n).map { |i| "fragment F#{i} on MaxWidthCheese { id flavor }" }.join("\n")
      "{ cheese(id: 1) { #{spreads} } }\n#{fragments}"
    }

    # Many genuinely distinct aliases: this really is wide.
    let(:many_alias_query) {
      "{ #{(1..n).map { |i| "a#{i}: intField" }.join(" ")} }"
    }

    it "gives the two queries different results" do
      repeated_result = analyze(schema, repeated_fragment_query, max_width: max_width)
      alias_result = analyze(schema, many_alias_query, max_width: max_width)

      # The repeated-fragment query only ever outputs 2 keys on the cheese
      # object, so it is allowed.
      assert_nil repeated_result

      # The aliased query really outputs 200 keys on the root object.
      assert_kind_of GraphQL::AnalysisError, alias_result
      assert_equal "Query has response width of #{n} on the MaxWidthQuery root object, which exceeds max width of #{max_width}", alias_result.message
    end

    it "the repeated-fragment query executes, while the aliased one is rejected" do
      schema_with_limit = build_schema(max_width: max_width)

      MaxWidthQuery.reset!
      ok_result = schema_with_limit.execute(repeated_fragment_query)
      refute ok_result.key?("errors")
      assert ok_result["data"].key?("cheese")
      assert_operator MaxWidthQuery.resolver_calls, :>, 0

      MaxWidthQuery.reset!
      rejected_result = schema_with_limit.execute(many_alias_query)
      assert rejected_result.key?("errors")
      assert_nil rejected_result["data"]
      # No resolver ran for the rejected query.
      assert_equal 0, MaxWidthQuery.resolver_calls
    end
  end

  describe "the reported error" do
    it "includes the response path, width and concrete type of the widest reachable branch" do
      query_string = <<-GRAPHQL
        {
          product {
            ... on MaxWidthCheese { id flavor }
            ... on MaxWidthMilk { id dairy brand extra: __typename }
          }
        }
      GRAPHQL
      # Cheese is 2 keys wide, Milk is 4; the widest reachable branch is reported.
      err = analyze(schema, query_string, max_width: 3)
      assert_kind_of GraphQL::AnalysisError, err
      assert_equal ["product"], err.path
      assert_equal "Query has response width of 4 on MaxWidthMilk at `product`, which exceeds max width of 3", err.message
      assert_equal({
        code: :max_width_exceeded,
        max_width: 3,
        response_width: 4,
        type: "MaxWidthMilk",
      }, err.extensions)
      assert_equal :max_width_exceeded, err.to_h["extensions"]["code"]
    end

    it "reports the root object with an empty path" do
      err = analyze(schema, "{ a: intField b: intField }", max_width: 1)
      assert_equal [], err.path
      assert_includes err.message, "on the MaxWidthQuery root object"
    end

    it "reports a violating nested object by its response path" do
      err = analyze(schema, "{ cheese(id: 1) { id flavor source } }", max_width: 2)
      assert_equal ["cheese"], err.path
      assert_includes err.message, "response width of 3 on MaxWidthCheese at `cheese`"
    end
  end

  describe "configuration" do
    it "uses the schema-level max_width" do
      limited_schema = build_schema(max_width: 1)
      assert_kind_of GraphQL::AnalysisError, analyze(limited_schema, "{ a: intField b: intField }")
      assert_nil analyze(limited_schema, "{ intField }")
    end

    it "allows a query-level override" do
      limited_schema = build_schema(max_width: 100)
      assert_kind_of GraphQL::AnalysisError, analyze(limited_schema, "{ a: intField b: intField }", max_width: 1)
      assert_nil analyze(limited_schema, "{ a: intField b: intField }")
    end

    it "counts fields skipped by known variables as absent" do
      query_string = <<-GRAPHQL
        query($skip: Boolean!) {
          a: intField
          b: intField @skip(if: $skip)
        }
      GRAPHQL
      limited_schema = build_schema(max_width: 1)
      assert_nil analyze(limited_schema, query_string, variables: { "skip" => true }, max_width: 1)
      assert_kind_of GraphQL::AnalysisError, analyze(limited_schema, query_string, variables: { "skip" => false }, max_width: 1)
    end
  end

  describe "across a multiplex" do
    it "uses the schema-level limit" do
      limited_schema = build_schema(max_width: 1)
      queries = [
        { query: "{ intField }" },
        { query: "{ a: intField b: intField }" },
      ]
      results = limited_schema.multiplex(queries)
      assert_nil results[0]["errors"]
      assert_includes results[1]["errors"].first["message"], "response width of 2"
    end
  end
end
