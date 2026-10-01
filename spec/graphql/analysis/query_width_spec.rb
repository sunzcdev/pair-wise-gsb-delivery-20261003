# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Analysis::QueryWidth do
  # A self-contained schema with a union and an interface so that mutually
  # exclusive abstract branches can be exercised.
  class WidthCheese < GraphQL::Schema::Object
    graphql_name "WidthCheese"
    field :id, Integer, null: false
    field :flavor, String, null: true
    field :source, String, null: true
  end

  class WidthMilk < GraphQL::Schema::Object
    graphql_name "WidthMilk"
    field :id, Integer, null: false
    field :dairy, String, null: true
    field :brand, String, null: true
    field :fat_content, Float, null: true
  end

  module WidthEdible
    include GraphQL::Schema::Interface
    graphql_name "WidthEdible"
    field :id, Integer, null: false
    orphan_types WidthCheese, WidthMilk
  end

  class WidthProduct < GraphQL::Schema::Union
    graphql_name "WidthProduct"
    possible_types WidthCheese, WidthMilk
  end

  class WidthQuery < GraphQL::Schema::Object
    graphql_name "WidthQuery"
    field :cheese, WidthCheese, null: true do
      argument :id, Integer, required: true
    end
    field :product, WidthProduct, null: true
    field :edible, WidthEdible, null: true
    field :int_field, Integer, null: true

    def cheese(id:)
      { id: id }
    end
  end

  class WidthSchema < GraphQL::Schema
    query WidthQuery
    orphan_types WidthCheese, WidthMilk
  end

  def width_for(query_string, variables = {})
    query = GraphQL::Query.new(WidthSchema, query_string, variables: variables)
    GraphQL::Analysis.analyze_query(query, [GraphQL::Analysis::QueryWidth]).first
  end

  it "counts distinct aliases at the same level" do
    query_string = "{ #{(1..10).map { |i| "c#{i}: intField" }.join(" ")} intField }"
    assert_equal 11, width_for(query_string)
  end

  it "merges repeated selections of the same response key" do
    query_string = <<-GRAPHQL
      {
        a: intField
        a: intField
        intField
        intField
      }
    GRAPHQL
    assert_equal 2, width_for(query_string)
  end

  it "merges fields selected through many repeated fragment spreads instead of counting each spread" do
    n = 300
    spreads = (0...n).map { |i| "...F#{i}" }.join(" ")
    fragments = (0...n).map { |i| "fragment F#{i} on WidthCheese { id flavor }" }.join("\n")
    query_string = "{ cheese(id: 1) { #{spreads} } }\n#{fragments}"
    assert_equal 2, width_for(query_string)
  end

  it "counts one spread per unique fragment but merges the shared response keys" do
    # Two distinct fragments each selecting `id`, plus `flavor` directly: id is
    # merged across both fragments, so the object is 2 keys wide, not 3.
    query_string = <<-GRAPHQL
      {
        cheese(id: 1) {
          ...A
          ...B
          flavor
        }
      }
      fragment A on WidthCheese { id }
      fragment B on WidthCheese { id source }
    GRAPHQL
    assert_equal 3, width_for(query_string)
  end

  it "takes the widest reachable branch for mutually exclusive union members, not their sum" do
    query_string = <<-GRAPHQL
      {
        product {
          __typename
          ... on WidthCheese { id flavor }
          ... on WidthMilk { id dairy brand fatContent }
        }
      }
    GRAPHQL
    # Cheese: 3 keys, Milk: 5 keys; never both at once -> 5, not 6.
    assert_equal 5, width_for(query_string)
  end

  it "takes the widest reachable branch for interface implementations" do
    query_string = <<-GRAPHQL
      {
        edible {
          ... on WidthCheese { id flavor source }
          ... on WidthMilk { id dairy brand fatContent }
        }
      }
    GRAPHQL
    assert_equal 4, width_for(query_string)
  end

  it "combines unconditional selections with each abstract branch" do
    query_string = <<-GRAPHQL
      {
        product {
          id
          ... on WidthCheese { flavor }
        }
      }
    GRAPHQL
    # Milk gets only `id` (1), Cheese gets `id` + `flavor` (2) -> 2
    assert_equal 2, width_for(query_string)
  end

  it "only counts an abstract-scope field on object types that actually define it" do
    query_string = <<-GRAPHQL
      {
        product {
          ... on WidthCheese { id flavor source }
          ... on WidthMilk { id dairy brand }
        }
      }
    GRAPHQL
    # Cheese: 3, Milk: 3 -> 3 (never the union of both, which would be 5)
    assert_equal 3, width_for(query_string)
  end

  it "ignores fields removed by @skip with known variables" do
    query_string = <<-GRAPHQL
      query($skip: Boolean!) {
        cheese(id: 1) {
          id
          flavor @skip(if: $skip)
          source @include(if: $skip)
        }
      }
    GRAPHQL
    # skip=true  -> id + source
    # skip=false -> id + flavor
    assert_equal 2, width_for(query_string, { "skip" => true })
    assert_equal 2, width_for(query_string, { "skip" => false })
  end

  it "measures each nested object independently" do
    query_string = "{ cheese(id: 1) { id flavor source } }"
    # Root is 1 key wide, the cheese object is 3 keys wide.
    assert_equal 3, width_for(query_string)
  end

  describe "count_introspection_fields: false" do
    it "does not count introspection fields" do
      schema = Class.new(WidthSchema)
      schema.max_depth(100, count_introspection_fields: false)
      query = GraphQL::Query.new(schema, "{ __typename intField }")
      width = GraphQL::Analysis.analyze_query(query, [GraphQL::Analysis::QueryWidth]).first
      assert_equal 1, width
    end
  end
end
