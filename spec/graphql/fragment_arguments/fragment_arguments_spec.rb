# frozen_string_literal: true
require "spec_helper"
require "graphql/fragment_arguments"

describe GraphQL::FragmentArguments do
  # A self-contained schema exercising the feature.
  module FragmentArgumentsTestNS
    class Item < GraphQL::Schema::Object
      field :id, ID, null: false
      field :name, String, null: true
      field :rank, Int, null: true do
        argument :value, Int, required: false
      end
      def rank(value: nil)
        value
      end
      field :tag, String, null: true do
        argument :label, String, required: false
        argument :priority, Int, required: false
      end
      def tag(label: nil, priority: nil)
        label ? "#{label}:#{priority}" : "plain"
      end

      field :repeated, String, null: true do
        argument :times, Int, required: false
        argument :s, String, required: false
      end
      def repeated(times: nil, s: nil)
        s.to_s * (times || 0)
      end
    end

    class Query < GraphQL::Schema::Object
      field :items, [Item], null: false
      def items
        [{ id: "1", name: "D", rank: 0 }]
      end
    end

    class Schema < GraphQL::Schema
      use GraphQL::FragmentArguments
      query(Query)
    end

    class PlainSchema < GraphQL::Schema
      query(Query)
    end
  end

  def execute(query_str, variables: {}, schema: FragmentArgumentsTestNS::Schema)
    schema.execute(query_str, variables: variables)
  end

  def messages(result)
    (result.to_h["errors"] || []).map { |e| e["message"] }
  end

  describe "parsing" do
    it "parses parameter declarations and spread arguments with the extension parser" do
      doc = GraphQL::FragmentArguments.parse(<<~GQL)
        fragment P($first: Int = 10, $after: String) on Item {
          id
        }
        query { items { ...P(first: 1) } }
      GQL

      frag = doc.definitions.first
      assert_kind_of GraphQL::Language::Nodes::FragmentDefinition, frag
      assert frag.parameterized?
      assert_equal ["first", "after"], frag.fragment_arguments.map(&:name)
      assert_equal 10, frag.fragment_arguments.first.default_value

      op = doc.definitions.last
      spread = op.selections.first.selections.first
      assert_kind_of GraphQL::Language::Nodes::FragmentSpread, spread
      assert spread.with_arguments?
      assert_equal ["first"], spread.fragment_arguments.map(&:name)
      assert_kind_of GraphQL::Language::Nodes::FragmentArgument, spread.fragment_arguments.first
      assert_kind_of GraphQL::Language::Nodes::FragmentArgumentDefinition, frag.fragment_arguments.first
    end

    it "round-trips a parameterized document through the printer" do
      str = <<~GQL
        fragment P($first: Int = 10) on Item {
          id
        }

        query {
          items {
            ...P(first: 1)
          }
        }
      GQL
      printed = GraphQL::FragmentArguments.parse(str).to_query_string
      assert_includes printed, "fragment P($first: Int = 10) on Item"
      assert_includes printed, "...P(first: 1)"
    end

    it "marshals and unmarshals parameterized documents" do
      doc = GraphQL::FragmentArguments.parse(<<~GQL)
        fragment P($n: Int = 3) on Item { id }
        query { items { ...P(n: 1) } }
      GQL
      restored = Marshal.load(Marshal.dump(doc))
      frag = restored.definitions.first
      assert_equal 3, frag.fragment_arguments.first.default_value
      spread = restored.definitions.last.selections.first.selections.first
      assert_equal 1, spread.fragment_arguments.first.value
    end

    it "is rejected by the stock parser when the extension is off" do
      assert_raises(GraphQL::ParseError) do
        GraphQL.parse("fragment P($n: Int) on Item { id }")
      end
      assert_raises(GraphQL::ParseError) do
        GraphQL.parse("fragment P on Item { id } query { items { ...P(n: 1) } }")
      end
    end

    it "preserves standard parse/execution behavior on a schema that did not enable it" do
      result = execute(
        "fragment P on Item { id } query { items { ...P } }",
        schema: FragmentArgumentsTestNS::PlainSchema
      )
      assert_equal [{"id" => "1"}], result.to_h["data"]["items"]

      bad = execute(
        "fragment P($n: Int) on Item { id } query { items { ...P } }",
        schema: FragmentArgumentsTestNS::PlainSchema
      )
      assert_equal 1, messages(bad).size
      assert_includes messages(bad).first, "Expected ON"
    end
  end

  describe "execution" do
    it "substitutes provided arguments" do
      result = execute(<<~GQL)
        fragment ItemFields($first: Int) on Item {
          id
          rank(value: $first)
        }
        query {
          items {
            ...ItemFields(first: 3)
          }
        }
      GQL

      expected = {"data" => {"items" => [{"id" => "1", "rank" => 3}]}}
      assert_equal expected, result.to_h
    end

    it "uses default values when no argument is passed" do
      result = execute(<<~GQL)
        fragment ItemFields($first: Int = 8) on Item {
          id
          rank(value: $first)
        }
        query { items { ...ItemFields } }
      GQL
      assert_equal 8, result.to_h["data"]["items"].first["rank"]
    end

    it "executes a spread expanded with aliased, differently-argued fields" do
      result = execute(<<~GQL)
        fragment ItemFields($first: Int) on Item {
          id
          r1: rank(value: $first)
        }
        query {
          items {
            ...ItemFields(first: 1)
          }
        }
      GQL
      assert_equal 1, result.to_h["data"]["items"].first["r1"]
    end

    it "gives each spread its own substituted expansion AST" do
      q = <<~GQL
        fragment ItemFields($first: Int) on Item {
          rank(value: $first)
        }
        query {
          items {
            ...ItemFields(first: 1)
            ...ItemFields(first: 2)
          }
        }
      GQL
      # Build the expanded document without running it (same response key with
      # different args is a GraphQL field conflict at execution).
      raw = GraphQL::FragmentArguments.parse(q)
      expanded, _ = GraphQL::FragmentArguments.expand(raw, context: nil)
      op = expanded.definitions.first
      item_selection = op.selections.first.selections
      inline_fragments = item_selection.select { |s| s.is_a?(GraphQL::Language::Nodes::InlineFragment) }
      assert_equal 2, inline_fragments.size

      values = inline_fragments.map do |frag|
        field = frag.selections.first
        field.arguments.first.value
      end
      assert_equal [1, 2], values
      refute_equal values[0].object_id, values[1].object_id
      refute_same(inline_fragments[0], inline_fragments[1])
    end

    it "does not let fragment-local names capture operation variables (or vice versa)" do
      q = <<~GQL
        fragment Shadow($first: Int = 9) on Item {
          id
          localRank: rank(value: $first)
        }
        query($first: Int = 1) {
          items {
            opRank: rank(value: $first)
            ...Shadow(first: 7)
          }
        }
      GQL
      result = execute(q)
      item = result.to_h["data"]["items"].first
      # The operation variable stays in the operation's own field; the local of
      # the same name is bound to the fragment argument.
      assert_equal 1, item["opRank"]
      assert_equal 7, item["localRank"]
    end

    it "forwards an operation variable into a fragment argument" do
      q = <<~GQL
        fragment ItemFields($s: String) on Item {
          id
          repeated(times: 2, s: $s)
        }
        query($c: String) {
          items {
            ...ItemFields(s: $c)
          }
        }
      GQL
      result = execute(q, variables: { "c" => "V" })
      assert_equal "VV", result.to_h["data"]["items"].first["repeated"]
    end

    it "substitutes inside nested field argument lists" do
      q = <<~GQL
        fragment F($label: String!, $priority: Int) on Item {
          id
          tag(label: $label, priority: $priority)
        }
        query {
          items {
            ...F(label: "hot", priority: 7)
          }
        }
      GQL
      result = execute(q)
      assert_equal "hot:7", result.to_h["data"]["items"].first["tag"]
    end

    it "supports nested parameterized fragments with forwarded locals" do
      q = <<~GQL
        fragment Inner($repeat: Int, $s: String) on Item {
          id
          repeated(times: $repeat, s: $s)
        }
        fragment Outer($m: Int) on Item {
          ...Inner(repeat: $m, s: "ok")
        }
        query { items { ...Outer(m: 2) } }
      GQL
      result = execute(q)
      assert_equal "okok", result.to_h["data"]["items"].first["repeated"]
    end

    it "keeps ordinary, non-parameterized fragments working unchanged" do
      result = execute(<<~GQL)
        fragment Plain on Item { id name }
        query { items { ...Plain } }
      GQL
      assert_equal "D", result.to_h["data"]["items"].first["name"]
    end

    it "drops parameterized definitions from the expanded document" do
      q = "fragment P($n: Int) on Item { id } query { items { ...P(n: 1) } }"
      query = GraphQL::Query.new(FragmentArgumentsTestNS::Schema, q)
      query.valid?
      refute_includes query.document.definitions.map(&:class), GraphQL::Language::Nodes::FragmentDefinition
    end

    it "resolves built-in scalars even when the schema never references them" do
      # The test schema never uses a Boolean field.
      result = execute(<<~GQL)
        fragment P($flag: Boolean = true) on Item { id }
        query { items { ...P } }
      GQL
      assert_empty messages(result)
      assert_equal "1", result.to_h["data"]["items"].first["id"]
    end

    it "supports multiple operations and operation_name" do
      q = <<~GQL
        fragment P($n: Int = 1) on Item { id }
        query A { items { ...P } }
        query B { items { ...P(n: 2) } }
      GQL
      result = FragmentArgumentsTestNS::Schema.execute(q, operation_name: "B")
      assert_equal [{"id" => "1"}], result.to_h["data"]["items"]
    end

    it "keeps spread directives on the expanded inline fragment" do
      result = execute(<<~GQL)
        fragment P($n: Int = 1) on Item { id name }
        query { items { ...P @skip(if: false) } }
      GQL
      assert_equal "D", result.to_h["data"]["items"].first["name"]
    end

    it "leaves document-provided queries on the standard path (no extension parse)" do
      doc = GraphQL.parse("fragment P on Item { id } query { items { ...P } }")
      result = FragmentArgumentsTestNS::Schema.execute(document: doc)
      assert_equal [{"id" => "1"}], result.to_h["data"]["items"]
    end
  end

  describe "validation" do
    def errors_for(query_str, variables: {})
      messages(execute(query_str, variables: variables))
    end

    it "flags a missing required argument" do
      errs = errors_for(<<~GQL)
        fragment P($n: Int!) on Item { id }
        query { items { ...P } }
      GQL
      assert_match(/Missing required argument '\$n'/, errs.first)
    end

    it "does not flag a missing nullable argument (binds null)" do
      result = execute(<<~GQL)
        fragment P($n: Int) on Item { id }
        query { items { ...P } }
      GQL
      assert_empty messages(result)
    end

    it "flags unknown spread arguments" do
      errs = errors_for(<<~GQL)
        fragment P($n: Int) on Item { id }
        query { items { ...P(x: 1, n: 2) } }
      GQL
      assert_includes errs.first, "has no argument named 'x'"
    end

    it "flags repeated arguments" do
      errs = errors_for(<<~GQL)
        fragment P($n: Int) on Item { id }
        query { items { ...P(n: 1, n: 2) } }
      GQL
      assert_includes errs.first, "is repeated"
    end

    it "flags incompatible literal values" do
      errs = errors_for(<<~GQL)
        fragment P($n: Int) on Item { id }
        query { items { ...P(n: "nope") } }
      GQL
      assert_includes errs.first, "not compatible with type 'Int'"
    end

    it "flags null passed to a non-null parameter" do
      errs = errors_for(<<~GQL)
        fragment P($n: Int!) on Item { id }
        query { items { ...P(n: null) } }
      GQL
      assert_includes errs.first, "not compatible with type 'Int!'"
    end

    it "flags an incompatible operation variable" do
      errs = errors_for(<<~GQL, variables: { "s" => "x" })
        fragment P($n: Int!) on Item { id }
        query($s: String) { items { ...P(n: $s) } }
      GQL
      assert_includes errs.first, "'String' is not compatible"
    end

    it "allows a non-null variable into a nullable parameter" do
      result = execute(<<~GQL, variables: { "n" => 1 })
        fragment P($n: Int) on Item { rank(value: $n) }
        query($n: Int!) { items { ...P(n: $n) } }
      GQL
      assert_empty messages(result)
      assert_equal 1, result.to_h["data"]["items"].first["rank"]
    end

    it "rejects a nullable variable into a non-null parameter" do
      errs = errors_for(<<~GQL, variables: { "n" => 1 })
        fragment P($n: Int!) on Item { id }
        query($n: Int) { items { ...P(n: $n) } }
      GQL
      assert_includes errs.first, "'Int' is not compatible"
    end

    it "flags an undefined variable used as an argument" do
      errs = errors_for(<<~GQL)
        fragment P($n: Int) on Item { id }
        query { items { ...P(n: $missing) } }
      GQL
      assert_includes errs.first, "is not defined by the operation"
    end

    it "flags recursively parameterized fragments" do
      errs = errors_for(<<~GQL)
        fragment A($n: Int) on Item { ...B(n: $n) }
        fragment B($n: Int) on Item { ...A(n: $n) }
        query { items { ...A(n: 1) } }
      GQL
      assert_includes errs.first, "recursive parameterized fragment reference"
    end

    it "flags direct self-recursion" do
      errs = errors_for(<<~GQL)
        fragment A($n: Int) on Item { id ...A(n: $n) }
        query { items { ...A(n: 1) } }
      GQL
      assert_includes errs.first, "recursive parameterized fragment reference"
    end

    it "flags unknown types in parameter declarations" do
      errs = errors_for(<<~GQL)
        fragment P($n: Bogus) on Item { id }
        query { items { ...P } }
      GQL
      assert_includes errs.first, "Unknown type 'Bogus'"
    end

    it "flags a spread of an undefined fragment" do
      errs = errors_for(<<~GQL)
        query { items { ...Missing(n: 1) } }
      GQL
      assert errs.any? { |m| m.include?("No fragment named 'Missing'") }, errs.inspect
    end
  end

  describe GraphQL::FragmentArguments::Expander do
    it "reports no parameterized syntax for an ordinary document" do
      doc = GraphQL.parse("fragment P on Item { id } query { items { ...P } }")
      refute GraphQL::FragmentArguments::Expander.parameterized?(doc)
    end

    it "detects a parameterized definition or spread" do
      doc = GraphQL::FragmentArguments.parse(<<~GQL)
        fragment P($n: Int) on Item { id }
        query { items { ...P } }
      GQL
      assert GraphQL::FragmentArguments::Expander.parameterized?(doc)
    end
  end
end


