# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Analysis::MaxInputValues do
  class InputValueBudgetSchema < GraphQL::Schema
    class << self
      def coercions
        @coercions ||= []
      end

      def resolutions
        @resolutions ||= []
      end

      def reset!
        @coercions = []
        @resolutions = []
      end
    end

    class CountingScalar < GraphQL::Schema::Scalar
      graphql_name "Counting"

      def self.coerce_input(value, _ctx)
        InputValueBudgetSchema.coercions << value
        value
      end

      def self.coerce_result(value, _ctx)
        value
      end
    end

    class FilterInput < GraphQL::Schema::InputObject
      argument :term, String, required: false
      argument :labels, [String], required: false
      argument :sub_filter, -> { InputValueBudgetSchema::FilterInput }, required: false
    end

    class Query < GraphQL::Schema::Object
      field :search, String, null: true do
        argument :filter, InputValueBudgetSchema::FilterInput, required: false
        argument :terms, [String], required: false
        argument :token, InputValueBudgetSchema::CountingScalar, required: false
        argument :with_default, [String], required: false, default_value: ["a", "b", "c"]
      end

      field :with_deep_default, String, null: true do
        argument :input, InputValueBudgetSchema::FilterInput, required: false, default_value: { sub_filter: { sub_filter: { term: "x" } } }
      end

      def search(**_args)
        InputValueBudgetSchema.resolutions << :search
        "result"
      end

      def with_deep_default(**_args)
        InputValueBudgetSchema.resolutions << :with_deep_default
        "result"
      end
    end

    query(Query)
  end

  let(:schema) { Class.new(InputValueBudgetSchema) }

  before do
    InputValueBudgetSchema.reset!
  end

  def error_messages(result)
    result["errors"]&.map { |e| e["message"] }
  end

  describe "max_input_value_count" do
    it "counts a huge variable even when the query string is tiny" do
      schema.max_input_value_count = 10
      query_str = "query($terms: [String!]) { search(terms: $terms) }"
      result = schema.execute(query_str, variables: { "terms" => 20.times.map { |i| "term-#{i}" } })
      assert_equal ["Argument `Query.search.terms` brings the query to 21 input value elements, which exceeds max input value count of 10"], error_messages(result)
      # The error is located at the argument that used the variable:
      assert_equal [{ "line" => 1, "column" => query_str.index("terms: $terms") + 1 }], result["errors"].first["locations"]
      # The resolver never ran:
      assert_equal [], InputValueBudgetSchema.resolutions
      assert_nil result["data"]
    end

    it "counts an equivalent inline value the same way" do
      schema.max_input_value_count = 10
      terms = 20.times.map { |i| "\"term-#{i}\"" }.join(", ")
      query_str = "{ search(terms: [#{terms}]) }"
      result = schema.execute(query_str)
      assert_equal ["Argument `Query.search.terms` brings the query to 21 input value elements, which exceeds max input value count of 10"], error_messages(result)
      assert_equal [{ "line" => 1, "column" => query_str.index("terms:") + 1 }], result["errors"].first["locations"]
      assert_equal [], InputValueBudgetSchema.resolutions
    end

    it "counts the same variable once per argument position" do
      schema.max_input_value_count = 10
      query_str = "query($terms: [String!]) { a: search(terms: $terms) b: search(terms: $terms) }"
      variables = { "terms" => ["a", "b", "c", "d", "e"] } # 6 elements per use
      result = schema.execute(query_str, variables: variables)
      # Each `search` also contributes its `withDefault` default (4 elements),
      # so the second use of `$terms` brings the total to 6 + 4 + 6 = 16:
      assert_equal ["Argument `Query.search.terms` brings the query to 16 input value elements, which exceeds max input value count of 10"], error_messages(result)
      # The error is attached to the second use, which exceeded the budget:
      assert_equal [{ "line" => 1, "column" => query_str.rindex("terms: $terms") + 1 }], result["errors"].first["locations"]
    end

    it "counts argument default values" do
      schema.max_input_value_count = 3
      result = schema.execute("{ search }")
      assert_equal ["Argument `Query.search.withDefault` brings the query to 4 input value elements, which exceeds max input value count of 3"], error_messages(result)
      # There's no argument in the query string, so the field itself is flagged:
      assert_equal [{ "line" => 1, "column" => 3 }], result["errors"].first["locations"]
      assert_equal [], InputValueBudgetSchema.resolutions
    end

    it "counts variable defaults from the query string" do
      schema.max_input_value_count = 3
      query_str = "query($terms: [String!] = [\"a\", \"b\", \"c\"]) { search(terms: $terms) }"
      result = schema.execute(query_str)
      assert_equal ["Argument `Query.search.terms` brings the query to 4 input value elements, which exceeds max input value count of 3"], error_messages(result)
      assert_equal [{ "line" => 1, "column" => query_str.index("terms: $terms") + 1 }], result["errors"].first["locations"]
    end

    it "counts defaults nested inside provided input objects" do
      schema.max_input_value_count = 100
      nested_schema = Class.new(InputValueBudgetSchema) do
        class DeepFilterInput < GraphQL::Schema::InputObject
          argument :name, String, required: false
          argument :tags, [String], required: false, default_value: ["x"] * 50
        end

        class DeepQuery < GraphQL::Schema::Object
          field :search, String, null: true do
            argument :filter, DeepFilterInput, required: false
          end

          def search(**_args)
            "result"
          end
        end

        query(DeepQuery)
      end
      nested_schema.max_input_value_count = 10
      # The provided value is tiny, but the `tags` default adds 51 elements:
      result = nested_schema.execute('{ search(filter: { name: "a" }) }')
      assert_equal ["Argument `DeepQuery.search.filter` brings the query to 55 input value elements, which exceeds max input value count of 10"], error_messages(result)
    end

    it "doesn't count operations that aren't executed" do
      schema.max_input_value_count = 10
      big_list = (["\"big\""] * 50).join(", ")
      query_str = "query Small { search(terms: [\"a\"]) }\nquery Big { search(terms: [#{big_list}]) }"

      result = schema.execute(query_str, operation_name: "Small")
      assert_nil result["errors"]
      assert_equal "result", result["data"]["search"]

      result = schema.execute(query_str, operation_name: "Big")
      assert_equal ["Argument `Query.search.terms` brings the query to 51 input value elements, which exceeds max input value count of 10"], error_messages(result)
    end

    it "doesn't count fields skipped by directives" do
      schema.max_input_value_count = 3
      query_str = "query($skipIt: Boolean!) { search(terms: [\"a\", \"b\", \"c\", \"d\"]) @skip(if: $skipIt) }"
      result = schema.execute(query_str, variables: { "skipIt" => true })
      assert_nil result["errors"]

      result = schema.execute(query_str, variables: { "skipIt" => false })
      assert_equal ["Argument `Query.search.terms` brings the query to 5 input value elements, which exceeds max input value count of 3"], error_messages(result)
    end
  end

  describe "max_input_value_string_bytes" do
    it "adds up string bytes over all argument values, per variable use" do
      schema.max_input_value_string_bytes = 10
      query_str = "query($terms: [String!]) { a: search(terms: $terms) b: search(terms: $terms) }"
      result = schema.execute(query_str, variables: { "terms" => ["aaaaaa"] })
      # Each use of `$terms` adds 6 bytes and each `search`'s `withDefault`
      # default adds 3, so the second use of `$terms` totals 6 + 3 + 6 = 15:
      assert_equal ["Argument `Query.search.terms` brings the query to 15 input value string bytes, which exceeds max input value string bytes of 10"], error_messages(result)
      assert_equal [], InputValueBudgetSchema.resolutions
    end

    it "counts hash keys from variables" do
      schema.max_input_value_string_bytes = 5
      result = schema.execute("query($filter: FilterInput) { search(filter: $filter) }", variables: { "filter" => { "term" => "ab" } })
      # "term" (4) + "ab" (2) = 6 bytes
      assert_equal ["Argument `Query.search.filter` brings the query to 6 input value string bytes, which exceeds max input value string bytes of 5"], error_messages(result)
    end
  end

  describe "max_input_value_depth" do
    it "checks the depth of each value, wherever it comes from" do
      schema.max_input_value_depth = 2
      query_str = "query($filter: FilterInput) { search(filter: $filter) }"
      variables = { "filter" => { "subFilter" => { "subFilter" => { "term" => "x" } } } }
      result = schema.execute(query_str, variables: variables)
      assert_equal ["Argument `Query.search.filter` has input depth of 3, which exceeds max input value depth of 2"], error_messages(result)
      assert_equal [{ "line" => 1, "column" => query_str.index("filter: $filter") + 1 }], result["errors"].first["locations"]
      assert_equal [], InputValueBudgetSchema.resolutions
    end

    it "checks inline values the same way" do
      schema.max_input_value_depth = 2
      query_str = "{ search(filter: { subFilter: { subFilter: { term: \"x\" } } }) }"
      result = schema.execute(query_str)
      assert_equal ["Argument `Query.search.filter` has input depth of 3, which exceeds max input value depth of 2"], error_messages(result)
      assert_equal [{ "line" => 1, "column" => query_str.index("filter:") + 1 }], result["errors"].first["locations"]
    end

    it "checks deep default values" do
      schema.max_input_value_depth = 2
      result = schema.execute("{ withDeepDefault }")
      assert_equal ["Argument `Query.withDeepDefault.input` has input depth of 3, which exceeds max input value depth of 2"], error_messages(result)
      assert_equal [], InputValueBudgetSchema.resolutions
    end
  end

  describe "custom scalars" do
    it "doesn't run coercion an extra time to measure values" do
      query_str = '{ search(token: "abc") }'

      baseline_schema = Class.new(InputValueBudgetSchema)
      baseline_schema.execute(query_str)
      baseline_count = InputValueBudgetSchema.coercions.size

      InputValueBudgetSchema.reset!
      schema.max_input_value_count = 100
      result = schema.execute(query_str)
      assert_equal "result", result["data"]["search"]
      assert_equal baseline_count, InputValueBudgetSchema.coercions.size
    end

    it "doesn't coerce anything or start resolvers when the budget is exceeded" do
      schema.max_input_value_count = 3
      result = schema.execute('{ search(token: "abc", terms: ["a", "b", "c", "d"]) }')
      assert_equal ["Argument `Query.search.terms` brings the query to 5 input value elements, which exceeds max input value count of 3"], error_messages(result)
      assert_equal [], InputValueBudgetSchema.resolutions
      # The only coercion is from static validation of the literal;
      # the runtime coercion never happened and measuring added none:
      assert_equal ["abc"], InputValueBudgetSchema.coercions
    end
  end

  describe "without configuration" do
    it "runs queries with big input values" do
      result = schema.execute("query($terms: [String!]) { search(terms: $terms) }", variables: { "terms" => ["a"] * 1000 })
      assert_nil result["errors"]
      assert_equal "result", result["data"]["search"]
    end
  end
end
