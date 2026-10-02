# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Analysis::InputValueBudget do
  class InputValueBudgetSchema < GraphQL::Schema
    class DeepInput < GraphQL::Schema::InputObject
      graphql_name "DeepInput"
      argument :child, DeepInput, required: false
      argument :name, String, required: false, default_value: "default-name-xxxxxxxxxx"
      argument :tags, [String], required: false
    end

    class TrackingScalar < GraphQL::Schema::Scalar
      graphql_name "TrackingScalar"

      def self.coerce_input(value, ctx)
        ctx[:scalar_coercions] = ctx[:scalar_coercions].to_i + 1
        value
      end
    end

    class Query < GraphQL::Schema::Object
      field :echo, String, null: true do
        argument :tags, [String], required: false
        argument :input, DeepInput, required: false
        argument :s, TrackingScalar, required: false
      end

      field :flag, String, null: true do
        argument :on, Boolean, required: false, default_value: true
        argument :tags, [String], required: false, default_value: ["d" * 40, "e" * 40]
      end

      field :side_effect, String, null: true

      def echo(tags: nil, input: nil, s: nil)
        context[:resolver_ran] = true
        "ok"
      end

      def flag(on: nil, tags: nil)
        context[:resolver_ran] = true
        "ok"
      end

      def side_effect
        context[:resolver_ran] = true
        "ran"
      end
    end

    query(Query)
  end

  def run_query(schema, query_str, **kwargs)
    schema.execute(query_str, **kwargs)
  end

  def schema_with(max_input_depth: nil, max_input_elements: nil, max_input_string_bytes: nil)
    Class.new(InputValueBudgetSchema) do
      max_input_depth(max_input_depth) if max_input_depth
      max_input_elements(max_input_elements) if max_input_elements
      max_input_string_bytes(max_input_string_bytes) if max_input_string_bytes
    end
  end

  describe "short query text with huge variable payload" do
    let(:schema) { schema_with(max_input_elements: 5, max_input_string_bytes: 20) }
    let(:query_str) { "query($v: [String!]) { echo(tags: $v) }" }
    let(:variables) { { "v" => Array.new(10) { |i| "value-#{i}-aaaaaaaaaaaaaaaaaaaaaa" } } }

    it "rejects the variable-based query" do
      result = run_query(schema, query_str, variables: variables)
      messages = result.to_h["errors"].map { |e| e["message"] }
      assert_includes messages, "Input value has 10 elements, which exceeds max input elements of 5"
      assert_includes messages, "Input value has 300 bytes of string data, which exceeds max input string bytes of 20"
      assert_nil result.to_h["data"]
    end

    it "rejects the equivalent inline literal query identically" do
      inline_values = Array.new(10) { |i| '"' + "value-#{i}-aaaaaaaaaaaaaaaaaaaaaa" + '"' }.join(", ")
      result = run_query(schema, "{ echo(tags: [#{inline_values}]) }")
      messages = result.to_h["errors"].map { |e| e["message"] }
      assert_includes messages, "Input value has 10 elements, which exceeds max input elements of 5"
      assert_includes messages, "Input value has 300 bytes of string data, which exceeds max input string bytes of 20"
    end

    it "attributes the error to the argument position using the value" do
      result = run_query(schema, query_str, variables: variables)
      error = result.to_h["errors"].first
      assert_equal [{ "line" => 1, "column" => 29 }], error["locations"]
    end
  end

  describe "depth" do
    let(:schema) { schema_with(max_input_depth: 2) }

    it "counts nested input object depth for variables" do
      result = run_query(schema, "query($i: DeepInput){ echo(input: $i) }", variables: { "i" => { "child" => { "child" => { "name" => "x" } } } })
      assert_includes result.to_h["errors"].first["message"], "Input value has depth of 3, which exceeds max input depth of 2"
    end

    it "counts nested input object depth for inline literals" do
      result = run_query(schema, '{ echo(input: {child: {child: {name: "x"}}}) }')
      assert_includes result.to_h["errors"].first["message"], "Input value has depth of 3, which exceeds max input depth of 2"
    end

    it "counts nested input object depth for variable definition default values" do
      result = run_query(schema, 'query($i: DeepInput = {child: {child: {name: "x"}}}){ echo(input: $i) }')
      assert_includes result.to_h["errors"].first["message"], "Input value has depth of 3, which exceeds max input depth of 2"
    end
  end

  describe "argument default values" do
    it "count the default value of an omitted argument against the budget" do
      schema = schema_with(max_input_elements: 1)
      result = run_query(schema, "{ flag }")
      assert_includes result.to_h["errors"].first["message"], "Input value has 2 elements, which exceeds max input elements of 1"
    end

    it "counts the default value of an omitted input object field" do
      schema = schema_with(max_input_string_bytes: 5)
      result = run_query(schema, "{ echo(input: {}) }")
      assert_includes result.to_h["errors"].first["message"], "bytes of string data"
    end

    it "does not count the default when the argument provides a small value" do
      schema = schema_with(max_input_elements: 1)
      result = run_query(schema, '{ flag(tags: ["x"]) }')
      assert_equal "ok", result.to_h.dig("data", "flag")
    end
  end

  describe "variables used in several positions" do
    let(:schema) { schema_with(max_input_string_bytes: 20) }

    it "counts the full value once per argument position" do
      result = run_query(schema, "query($v: String!){ a: echo(s: $v) b: echo(s: $v) }", variables: { "v" => "a" * 25 })
      locations = result.to_h["errors"].map { |e| e["locations"] }
      assert_equal 2, result.to_h["errors"].length
      assert_includes locations, [{ "line" => 1, "column" => 29 }]
      assert_includes locations, [{ "line" => 1, "column" => 44 }]
    end

    it "counts a variable used as a nested input object field" do
      schema = schema_with(max_input_elements: 2)
      result = run_query(
        schema,
        "query($t: [String!]){ echo(input: {tags: $t}) }",
        variables: { "t" => ["a", "b", "c"] }
      )
      # 3 elements from the variable plus 1 from the input object's default `name`
      assert_includes result.to_h["errors"].first["message"], "Input value has 4 elements, which exceeds max input elements of 2"
    end

    it "does not count a variable only referenced by an unexecuted operation" do
      schema = schema_with(max_input_elements: 1)
      result = run_query(
        schema,
        "query Huge($t: [String!]){ echo(tags: $t) } query Small { sideEffect }",
        variables: { "t" => ["a", "b", "c"] },
        operation_name: "Small"
      )
      assert_equal({ "sideEffect" => "ran" }, result.to_h["data"])
    end
  end

  describe "unexecuted operations" do
    it "does not count arguments of operations that won't be executed" do
      schema = schema_with(max_input_elements: 1)
      result = run_query(
        schema,
        'query Huge { echo(tags: ["a","b","c"]) } query Small { sideEffect }',
        operation_name: "Small"
      )
      assert_equal({ "sideEffect" => "ran" }, result.to_h["data"])
    end
  end

  describe "resolution timing" do
    it "does not run custom scalar conversion just to count values" do
      schema = schema_with(max_input_elements: 1)
      context = {}
      # Run only the analyzer: counting alone must never coerce.
      query = GraphQL::Query.new(schema, '{ echo(s: "x") }', context: context)
      GraphQL::Analysis.analyze_query(query, [GraphQL::Analysis::InputValueBudget])
      assert_nil context[:scalar_coercions]
    end

    it "does not add any extra coercion calls compared with a schema without the budget" do
      context = {}
      InputValueBudgetSchema.execute('{ echo(s: "x") }', context: context)
      baseline = context[:scalar_coercions]

      schema = schema_with(max_input_elements: 1)
      limited_context = {}
      run_query(schema, '{ echo(s: "x") }', context: limited_context)
      assert_equal baseline, limited_context[:scalar_coercions]
    end

    it "halts before any resolver starts when the budget is exceeded" do
      schema = schema_with(max_input_elements: 1)
      context = {}
      result = run_query(schema, "query($v: [String!]){ echo(tags: $v) sideEffect }", variables: { "v" => ["a", "b", "c"] }, context: context)
      refute context[:resolver_ran]
      assert_nil result.to_h["data"]
    end
  end

  describe "byte counting" do
    let(:schema) { schema_with(max_input_string_bytes: 4) }

    it "uses byte length rather than character length" do
      result = run_query(schema, '{ echo(tags: ["éé"]) }')
      assert_equal "ok", result.to_h.dig("data", "echo")
      result = run_query(schema, '{ echo(tags: ["ééé"]) }')
      assert_includes result.to_h["errors"].first["message"], "Input value has 6 bytes of string data"
    end
  end

  describe "custom scalar types (such as JSON)" do
    let(:schema) { schema_with(max_input_elements: 2, max_input_string_bytes: 5) }

    it "counts a composite-shaped value as one scalar for literals and variables alike" do
      literal = run_query(schema, '{ echo(s: {a: {b: [1, 2, 3]}, c: "xxxxxxxxxxxxxxxx"}) }')
      variable = run_query(schema, "query($d: TrackingScalar){ echo(s: $d) }", variables: { "d" => { "a" => [1, 2, 3], "c" => "x" * 20 } })
      assert_equal "ok", literal.to_h.dig("data", "echo")
      assert_equal "ok", variable.to_h.dig("data", "echo")
    end
  end

  describe "when no limit is configured" do
    it "does not add the analyzer" do
      refute_includes InputValueBudgetSchema.query_analyzers, GraphQL::Analysis::InputValueBudget
    end
  end

  describe "per-query overrides" do
    it "accepts a larger limit than the schema default" do
      schema = schema_with(max_input_elements: 1)
      query = GraphQL::Query.new(
        schema,
        "query($v: [String!]){ echo(tags: $v) }",
        variables: { "v" => ["a", "b", "c", "d"] },
        max_input_elements: 10
      )
      assert query.valid?
    end
  end
end

