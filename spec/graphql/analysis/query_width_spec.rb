# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Analysis::QueryWidth do
  let(:result) { GraphQL::Analysis.analyze_query(query, [GraphQL::Analysis::QueryWidth]).first }
  let(:query) { GraphQL::Query.new(schema, query_string, variables: variables) }
  let(:schema) {
    schema = Class.new(Dummy::Schema)
    schema.analysis_engine = GraphQL::Analysis::AST
    schema
  }
  let(:variables) { {} }

  describe "counting distinct response keys" do
    let(:query_string) {%|
      {
        cheese(id: 1) {
          id
          flavor
          origin
        }
      }
    |}

    it "counts distinct keys in a selection set" do
      assert_equal 3, result
    end
  end

  describe "aliases" do
    let(:query_string) {
      aliases = (1..10).map { |i| "a#{i}: id" }.join("\n")
      "{ cheese(id: 1) { #{aliases} } }"
    }

    it "counts each aliased key separately" do
      assert_equal 10, result
    end
  end

  describe "repeated selections and fragments of the same key" do
    let(:query_string) {
      spreads = (1..10).map { |i| "...f#{i}" }.join("\n")
      fragments = (1..10).map { |i| "fragment f#{i} on Cheese { id }" }.join("\n")
      "{ cheese(id: 1) { #{spreads} } } #{fragments}"
    }

    it "merges selections with the same response key" do
      assert_equal 1, result
    end
  end

  describe "fragments selecting different keys" do
    let(:query_string) {%|
      {
        cheese(id: 1) {
          ...f1
          ...f2
        }
      }
      fragment f1 on Cheese { id }
      fragment f2 on Cheese { flavor }
    |}

    it "unions the keys because the type conditions are compatible" do
      assert_equal 2, result
    end
  end

  describe "abstract type conditions" do
    let(:query_string) {%|
      {
        favoriteEdible {
          __typename
          ... on Cheese { id flavor }
          ... on Milk { id source origin }
        }
      }
    |}

    it "uses the widest reachable branch instead of summing mutually exclusive ones" do
      # Cheese:  __typename + id + flavor = 3
      # Milk:    __typename + id + source + origin = 4
      assert_equal 4, result
    end

    describe "unconditional selections shared by branches" do
      let(:query_string) {%|
        {
          favoriteEdible {
            fatContent
            ... on Cheese { flavor }
            ... on Milk { source origin }
          }
        }
      |}

      it "counts unconditional fields in every branch" do
        assert_equal 3, result
      end
    end

    describe "nested abstract selections" do
      let(:query_string) {%|
        {
          favoriteEdible {
            ... on Cheese {
              id
              selfAsEdible {
                __typename
                ... on Cheese { flavor fatContent }
                ... on Milk { origin source fatContent }
              }
            }
            ... on Milk { id origin }
          }
        }
      |}

      it "does not leak the outer fragment condition into the nested object" do
        # Inner Edible: Milk branch -> __typename + origin + source + fatContent = 4
        assert_equal 4, result
      end
    end
  end

  describe "directives" do
    let(:query_string) {%|
      query($skip: Boolean!) {
        cheese(id: 1) {
          id
          flavor @skip(if: $skip)
          origin @include(if: $skip)
        }
      }
    |}

    describe "when a variable skips a field" do
      let(:variables) { { "skip" => true } }

      it "does not count the skipped field but counts the included field" do
        assert_equal 2, result
      end
    end

    describe "when a variable includes a field" do
      let(:variables) { { "skip" => false } }

      it "does not count the excluded field but counts the others" do
        assert_equal 2, result
      end
    end

    describe "when a fragment spread is skipped" do
      let(:query_string) {%|
        query($skip: Boolean!) {
          cheese(id: 1) {
            id
            ...f @skip(if: $skip)
          }
        }
        fragment f on Cheese { flavor origin }
      |}

      let(:variables) { { "skip" => true } }

      it "does not count keys inside the skipped fragment" do
        assert_equal 1, result
      end
    end
  end

  describe "nested selection sets" do
    let(:query_string) {%|
      {
        cheese(id: 1) {
          id
          similarCheese(source: SHEEP) {
            id
            flavor
            origin
            source
          }
        }
      }
    |}

    it "returns the widest object anywhere in the response" do
      assert_equal 4, result
    end
  end

  describe "introspection fields" do
    let(:query_string) {%|
      {
        __type(name: "Cheese") {
          name
          kind
          description
        }
      }
    |}

    it "counts introspection fields by default" do
      assert_equal 3, result
    end

    describe "when count_introspection_fields is disabled" do
      let(:schema) {
        schema = Class.new(Dummy::Schema)
        schema.analysis_engine = GraphQL::Analysis::AST
        schema.max_depth(100, count_introspection_fields: false)
        schema
      }

      it "does not count introspection fields" do
        assert_equal 0, result
      end
    end
  end
end
