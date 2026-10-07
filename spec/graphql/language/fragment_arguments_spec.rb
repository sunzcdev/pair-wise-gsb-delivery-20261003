# frozen_string_literal: true

require "spec_helper"

describe GraphQL::Language::FragmentArguments do
  # Project-specific extension (not part of the GraphQL specification) for
  # parameterized fragments:
  #
  #   fragment P($first: Int = 10) on Query {
  #     items(first: $first) { name }
  #   }
  #
  #   query { ...P(first: 5) }
  class FragmentArgItem < GraphQL::Schema::Object
    graphql_name "FragmentArgItem"
    field :name, String, null: false
  end

  class FragmentArgQuery < GraphQL::Schema::Object
    graphql_name "Query"
    field :items, [FragmentArgItem], null: false do
      argument :first, Integer, required: false
      argument :after, String, required: false
      argument :q, String, required: false
    end

    def items(first: nil, after: nil, q: nil)
      values = [{ name: "a" }, { name: "b" }, { name: "c" }]
      values = values.select { |v| v[:name] == q } if q
      values = values.drop_while { |v| v[:name] != after }.drop(1) if after
      values.first(first || values.length)
    end
  end

  class FragmentArgSchema < GraphQL::Schema
    fragment_arguments true
    query FragmentArgQuery
  end

  class FragmentArgSchemaOff < GraphQL::Schema
    query FragmentArgQuery
  end

  def expand(schema, query_string)
    doc = GraphQL.parse(query_string, fragment_arguments: true)
    new_doc, errors, _forwarded = GraphQL::Language::FragmentArguments.expand(schema, doc)
    [new_doc, errors]
  end

  describe "schema setting" do
    it "is disabled by default" do
      refute FragmentArgSchemaOff.fragment_arguments?
    end

    it "can be enabled" do
      assert FragmentArgSchema.fragment_arguments?
    end

    it "is inherited by subclasses" do
      subclass = Class.new(FragmentArgSchema)
      assert subclass.fragment_arguments?
    end
  end

  describe "parsing" do
    it "parses fragment parameter definitions and spread arguments when enabled" do
      doc = GraphQL.parse(<<~GQL, fragment_arguments: true)
        fragment P($first: Int = 10) on Query {
          items(first: $first) { name }
        }
        query { ...P(first: 5) }
      GQL

      frag = doc.definitions.first
      assert_equal "P", frag.name
      assert_equal 1, frag.variables.length
      assert_equal "first", frag.variables.first.name
      assert_equal "Int", frag.variables.first.type.name
      assert_equal 10, frag.variables.first.default_value

      op = doc.definitions.last
      spread = op.selections.first
      assert_equal "P", spread.name
      assert_equal "first", spread.arguments.first.name
      assert_equal 5, spread.arguments.first.value
    end

    it "round-trips through the printer" do
      query_string = "fragment P($first: Int = 10) on Query {\n" \
        "  items(first: $first) {\n" \
        "    name\n" \
        "  }\n" \
        "}\n" \
        "\n" \
        "query {\n" \
        "  ...P(first: 5)\n" \
        "}"
      doc = GraphQL.parse(query_string + "\n", fragment_arguments: true)
      assert_equal query_string, doc.to_query_string
    end

    it "rejects parameter definitions in the default parser mode" do
      assert_raises(GraphQL::ParseError) do
        GraphQL.parse("fragment P($n: Int) on Query { items(first: $n) { name } }")
      end
    end

    it "rejects spread arguments in the default parser mode" do
      assert_raises(GraphQL::ParseError) do
        GraphQL.parse("fragment P on Query { items { name } } query { ...P(n: 1) }")
      end
    end

    it "still parses ordinary fragments with the extension enabled" do
      doc = GraphQL.parse("fragment P on Query { items { name } } query { ...P }", fragment_arguments: true)
      frag = doc.definitions.first
      assert_equal [], frag.variables
      assert_equal [], doc.definitions.last.selections.first.arguments
    end
  end

  describe "expansion" do
    it "inlines literal arguments and removes the fragment definition" do
      doc, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($first: Int = 10) on Query {
          items(first: $first) { name }
        }
        query { ...P(first: 2) }
      GQL

      assert_equal [], errors
      refute doc.definitions.any? { |d| d.is_a?(GraphQL::Language::Nodes::FragmentDefinition) }
      spread = doc.definitions.first.selections.first
      assert_instance_of GraphQL::Language::Nodes::InlineFragment, spread
      assert_equal "Query", spread.type.name
      field = spread.selections.first
      assert_equal "items", field.name
      assert_equal 2, field.arguments.first.value
    end

    it "uses default values when an argument is omitted" do
      doc, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($first: Int = 2) on Query {
          items(first: $first) { name }
        }
        query { ...P }
      GQL

      assert_equal [], errors
      field = doc.definitions.first.selections.first.selections.first
      assert_equal 2, field.arguments.first.value
    end

    it "treats an omitted nullable parameter without a default as an absent value (the argument is dropped)" do
      doc, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($after: String) on Query {
          items(after: $after) { name }
        }
        query { ...P }
      GQL

      assert_equal [], errors
      field = doc.definitions.first.selections.first.selections.first
      assert_equal [], field.arguments
    end

    it "passes outer variables through unchanged" do
      doc, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($first: Int!) on Query {
          items(first: $first) { name }
        }
        query Q($n: Int = 2) { ...P(first: $n) }
      GQL

      assert_equal [], errors
      var_id = doc.definitions.first.selections.first.selections.first.arguments.first.value
      assert_instance_of GraphQL::Language::Nodes::VariableIdentifier, var_id
      assert_equal "n", var_id.name
    end

    it "does not let a local parameter capture an operation variable with the same name" do
      result = FragmentArgSchema.execute(<<~GQL, variables: { "first" => 2 })
        fragment P($first: Int = 1) on Query {
          local: items(first: $first) { name }
        }
        query Q($first: Int!) {
          outer: items(first: $first) { name }
          ...P
        }
      GQL

      refute result.to_h.key?("errors"), result.to_h.inspect
      assert_equal 2, result.to_h["data"]["outer"].length
      # The fragment uses its own default, not the operation's $first
      assert_equal 1, result.to_h["data"]["local"].length
    end

    it "expands two spreads of the same fragment with independent results" do
      doc, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($first: Int!) on Query {
          items(first: $first) { name }
        }
        query {
          ...P(first: 1)
          ...P(first: 2)
        }
      GQL

      assert_equal [], errors
      spreads = doc.definitions.first.selections
      first_values = spreads.map { |s| s.selections.first.arguments.first.value }
      assert_equal [1, 2], first_values
    end

    it "expands nested parameterized fragments with correct substitution order" do
      doc, errors = expand(FragmentArgSchema, <<~GQL)
        fragment Inner($first: Int!) on Query {
          items(first: $first) { name }
        }
        fragment Outer($n: Int!) on Query {
          ...Inner(first: $n)
        }
        query { ...Outer(n: 2) }
      GQL

      assert_equal [], errors
      # Outer is inlined, then Inner is inlined with $n already substituted
      inner = doc.definitions.first.selections.first.selections.first
      assert_instance_of GraphQL::Language::Nodes::InlineFragment, inner
      field = inner.selections.first
      assert_equal 2, field.arguments.first.value
    end

    it "keeps lexical scoping for parameters with the same name in nested fragments" do
      result = FragmentArgSchema.execute(<<~GQL)
        fragment B($first: Int!) on Query {
          bItems: items(first: $first) { name }
        }
        fragment A($first: Int!) on Query {
          aItems: items(first: $first) { name }
          ...B(first: 3)
        }
        query { ...A(first: 1) }
      GQL

      refute result.to_h.key?("errors"), result.to_h.inspect
      assert_equal 1, result.to_h["data"]["aItems"].length
      assert_equal 3, result.to_h["data"]["bItems"].length
    end

    it "substitutes variables inside nested input values" do
      doc, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($q: String) on Query {
          items(q: $q) { name }
        }
        query Q($term: String) { ...P(q: $term) }
      GQL

      assert_equal [], errors
      var_id = doc.definitions.first.selections.first.selections.first.arguments.first.value
      assert_equal "term", var_id.name
    end

    it "preserves directives from the fragment definition and the spread" do
      doc, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($first: Int = 2) on Query @skip(if: false) {
          items(first: $first) { name }
        }
        query {
          ...P(first: 2) @include(if: true)
        }
      GQL

      assert_equal [], errors
      inline = doc.definitions.first.selections.first
      assert_equal ["skip", "include"], inline.directives.map(&:name)
    end

    it "does not flag an operation variable as unused when it is only forwarded to a fragment that ignores it" do
      result = FragmentArgSchema.execute(<<~GQL, variables: { "n" => 2 })
        fragment P($n: Int = 1) on Query {
          items { name }
        }
        query Q($n: Int = 2) {
          ...P(n: $n)
        }
      GQL

      refute result.to_h.key?("errors"), result.to_h.inspect
      assert_equal ["a", "b", "c"], result.to_h["data"]["items"].map { |i| i["name"] }
    end

    it "reports a missing required parameter" do
      _, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($n: Int!) on Query {
          items(first: $n) { name }
        }
        query { ...P }
      GQL

      assert_equal 1, errors.length
      assert_match(/Missing required parameter/, errors.first.message)
    end

    it "reports an unknown parameter" do
      _, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($n: Int = 1) on Query {
          items(first: $n) { name }
        }
        query { ...P(x: 2) }
      GQL

      assert_equal 1, errors.length
      assert_match(/doesn't accept parameter/, errors.first.message)
    end

    it "reports duplicate parameter names in a spread" do
      _, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($n: Int = 1) on Query {
          items(first: $n) { name }
        }
        query { ...P(n: 2, n: 3) }
      GQL

      assert_equal 1, errors.length
      assert_match(/only one argument named/, errors.first.message)
    end

    it "reports duplicate parameter declarations on a fragment" do
      _, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($n: Int, $n: Int) on Query {
          items(first: $n) { name }
        }
        query { ...P(n: 2) }
      GQL

      assert_equal 1, errors.length
      assert_match(/can only be declared once/, errors.first.message)
    end

    it "reports an incompatible literal argument type" do
      _, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($n: Int!) on Query {
          items(first: $n) { name }
        }
        query { ...P(n: "abc") }
      GQL

      assert_equal 1, errors.length
      assert_match(/Int/, errors.first.message)
    end

    it "reports an incompatible variable argument type" do
      _, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($n: Int!) on Query {
          items(first: $n) { name }
        }
        query Q($s: String) { ...P(n: $s) }
      GQL

      assert_equal 1, errors.length
      assert_match(/Type mismatch/, errors.first.message)
    end

    it "reports a non-input parameter type" do
      _, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($q: Query) on Query {
          items { name }
        }
        query { ...P }
      GQL

      assert_equal 1, errors.length
      assert_match(/must be an input type/, errors.first.message)
    end

    it "reports an unknown parameter type" do
      _, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($q: TotallyMadeUp) on Query {
          items { name }
        }
        query { ...P }
      GQL

      assert_equal 1, errors.length
      assert_match(/unknown type/, errors.first.message)
    end

    it "reports an invalid default value" do
      _, errors = expand(FragmentArgSchema, <<~GQL)
        fragment P($n: Int = "nope") on Query {
          items(first: $n) { name }
        }
        query { ...P }
      GQL

      assert_equal 1, errors.length
      assert_match(/coerce|Default value/, errors.first.message)
    end

    it "reports arguments given to a non-parameterized fragment" do
      _, errors = expand(FragmentArgSchema, <<~GQL)
        fragment Plain on Query {
          items { name }
        }
        query { ...Plain(first: 1) }
      GQL

      assert_equal 1, errors.length
      assert_match(/doesn't declare any arguments/, errors.first.message)
    end

    it "detects a direct recursive parameterized fragment" do
      _, errors = expand(FragmentArgSchema, <<~GQL)
        fragment A($n: Int = 1) on Query {
          ...A(n: $n)
        }
        query { ...A }
      GQL

      assert_equal 1, errors.length
      assert_match(/within itself/, errors.first.message)
    end

    it "detects a mutually recursive pair of parameterized fragments" do
      _, errors = expand(FragmentArgSchema, <<~GQL)
        fragment A($n: Int = 1) on Query {
          ...B(n: $n)
        }
        fragment B($n: Int = 1) on Query {
          ...A(n: $n)
        }
        query { ...A }
      GQL

      assert_equal 1, errors.length
      assert_match(/A -> B -> A/, errors.first.message)
    end

    it "leaves ordinary fragments and spreads untouched" do
      doc = GraphQL.parse(<<~GQL, fragment_arguments: true)
        fragment Plain on Query {
          items { name }
        }
        query { ...Plain }
      GQL

      new_doc, errors, _forwarded = GraphQL::Language::FragmentArguments.expand(FragmentArgSchema, doc)
      assert_equal [], errors
      assert_equal doc.to_query_string, new_doc.to_query_string
      assert_same doc, new_doc
      assert_instance_of GraphQL::Language::Nodes::FragmentSpread, new_doc.definitions.last.selections.first
      assert new_doc.definitions.any? { |d| d.is_a?(GraphQL::Language::Nodes::FragmentDefinition) }
    end

    it "leaves unrelated nodes untouched even when the document uses the extension" do
      doc = GraphQL.parse(<<~GQL, fragment_arguments: true)
        fragment P($first: Int = 1) on Query {
          items(first: $first) { name }
        }
        query {
          untouched: items { name }
          ...P(first: 2)
        }
      GQL

      new_doc, errors, _forwarded = GraphQL::Language::FragmentArguments.expand(FragmentArgSchema, doc)
      assert_equal [], errors
      operation = new_doc.definitions.first
      untouched_field = operation.selections.first
      assert_equal "untouched", untouched_field.alias
      assert_same doc.definitions.last.selections.first, untouched_field
    end
  end

  describe "execution" do
    it "executes a parameterized fragment with pagination arguments" do
      result = FragmentArgSchema.execute(<<~GQL)
        fragment Page($first: Int = 10) on Query {
          items(first: $first) { name }
        }
        query {
          ...Page(first: 2)
        }
      GQL

      assert_equal ["a", "b"], result.to_h["data"]["items"].map { |i| i["name"] }
    end

    it "supports the same fragment twice with different argument sets on different response keys" do
      result = FragmentArgSchema.execute(<<~GQL)
        fragment Page($first: Int!) on Query {
          paged: items(first: $first) { name }
        }
        query {
          ... on Query { firstPage: items(first: 1) { name } }
          second: items(first: 2) { name }
        }
      GQL

      refute result.to_h.key?("errors"), result.to_h.inspect
      assert_equal ["a"], result.to_h["data"]["firstPage"].map { |i| i["name"] }
      assert_equal ["a", "b"], result.to_h["data"]["second"].map { |i| i["name"] }
    end

    it "returns validation errors and halts on invalid fragment argument usage" do
      result = FragmentArgSchema.execute(<<~GQL)
        fragment P($n: Int!) on Query {
          items(first: $n) { name }
        }
        query { ...P }
      GQL

      assert_nil result.to_h["data"]
      assert_match(/Missing required parameter/, result.to_h["errors"].first["message"])
    end
  end

  describe "with the extension disabled" do
    it "reports a parse error for parameter syntax, preserving the previous behavior" do
      result = FragmentArgSchemaOff.execute(<<~GQL)
        fragment P($n: Int) on Query {
          items(first: $n) { name }
        }
        query { ...P(n: 1) }
      GQL

      assert(result.to_h.key?("errors"))
      assert_match(/Expected ON/, result.to_h["errors"].first["message"])
    end

    it "rejects parameter syntax at the parser level" do
      assert_raises(GraphQL::ParseError) do
        GraphQL.parse("fragment P($n: Int) on Query { items(first: $n) { name } }")
      end
    end

    it "executes ordinary fragments normally" do
      result = FragmentArgSchemaOff.execute(<<~GQL)
        fragment Plain on Query {
          items { name }
        }
        query { ...Plain }
      GQL

      refute result.to_h.key?("errors")
      assert_equal ["a", "b", "c"], result.to_h["data"]["items"].map { |i| i["name"] }
    end
  end
end
