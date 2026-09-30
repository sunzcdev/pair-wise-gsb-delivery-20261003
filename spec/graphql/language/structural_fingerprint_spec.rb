# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Language::StructuralFingerprint do
  def fingerprint(query)
    GraphQL::Language::StructuralFingerprint.generate(query).fingerprint
  end

  def canonical(query)
    GraphQL::Language::StructuralFingerprint.generate(query).canonical_query_string
  end

  # Pairs which differ only in a meaningless way and must therefore collapse
  # to the same fingerprint.
  equivalent_pairs = [
    {
      description: "renamed variables",
      left:  "query Q($id: ID!, $first: Int = 10) { user(id: $id, first: $first) { name email } }",
      right: "query Q($who: ID!, $limit: Int = 10) { user(id: $who, first: $limit) { name email } }",
    },
    {
      description: "reordered field arguments",
      left:  "{ user(id: \"1\", active: true) { name } }",
      right: "{ user(active: true, id: \"1\") { name } }",
    },
    {
      description: "reordered input object fields (nested)",
      left:  "{ node(filter: {limit: 10, order: {dir: DESC, field: NAME}}) { id } }",
      right: "{ node(filter: {order: {field: NAME, dir: DESC}, limit: 10}) { id } }",
    },
    {
      description: "renamed fragment definition and its spreads",
      left:  "query Q($id: ID!) { node(id: $id) { ...UserFields } } fragment UserFields on User { name email }",
      right: "query Q($pid: ID!) { node(id: $pid) { ...Renamed } } fragment Renamed on User { name email }",
    },
    {
      description: "renamed variables used inside a fragment",
      left:  "query Q($id: ID!) { node(id: $id) { ...F } } fragment F on Node { child(id: $id) { name } }",
      right: "query Q($x: ID!) { node(id: $x) { ...G } } fragment G on Node { child(id: $x) { name } }",
    },
    {
      description: "reordered directive arguments",
      left:  "query Q { user @skip(if: true, label: \"x\") { name } }",
      right: "query Q { user @skip(label: \"x\", if: true) { name } }",
    },
    {
      description: "reordered input object in a variable default value",
      left:  "query Q($f: Filter = {b: 2, a: 1}) { user(filter: $f) { name } }",
      right: "query Q($g: Filter = {a: 1, b: 2}) { user(filter: $g) { name } }",
    },
  ]

  # Pairs which look similar but differ in a way that may change semantics, so
  # they must keep distinct fingerprints.
  distinct_pairs = [
    {
      description: "response alias changes the result key",
      left:  "{ user { name } }",
      right: "{ user { n: name } }",
    },
    {
      description: "selection field order is preserved",
      left:  "{ user { name email } }",
      right: "{ user { email name } }",
    },
    {
      description: "list element order is preserved",
      left:  '{ user(ids: ["1", "2"]) { name } }',
      right: '{ user(ids: ["2", "1"]) { name } }',
    },
    {
      description: "directive order is preserved",
      left:  "query Q { user @a @b { name } }",
      right: "query Q { user @b @a { name } }",
    },
    {
      description: "custom scalar literals are compared syntactically, never coerced",
      left:  '{ user(created: "2020-01-01") { name } }',
      right: '{ user(created: "2020-01-01T00:00:00Z") { name } }',
    },
    {
      description: "variable declaration order anchors the canonical rename",
      left:  "query Q($a: ID!, $b: Int) { user(id: $a, n: $b) { name } }",
      right: "query Q($b: Int, $a: ID!) { user(id: $a, n: $b) { name } }",
    },
    {
      description: "variables swapped in use are a different query",
      left:  "query Q($a: ID!, $b: ID!) { user(id: $a) { child(id: $b) } }",
      right: "query Q($a: ID!, $b: ID!) { user(id: $b) { child(id: $a) } }",
    },
    {
      description: "variable type changes the operation signature",
      left:  "query Q($id: ID!) { user(id: $id) { name } }",
      right: "query Q($id: ID) { user(id: $id) { name } }",
    },
    {
      description: "a referenced variable is not declared",
      left:  "query Q($id: ID!) { user(id: $id) { name } }",
      right: "query Q { user(id: \"1\") { name } }",
    },
    {
      description: "different enum literal",
      left:  "{ user(status: ACTIVE) { name } }",
      right: "{ user(status: INACTIVE) { name } }",
    },
    {
      description: "fragment type condition differs",
      left:  "query Q { node { ...F } } fragment F on User { name }",
      right: "query Q { node { ...F } } fragment F on Admin { name }",
    },
    {
      description: "operation name is part of the identity",
      left:  "query A { user { name } }",
      right: "query B { user { name } }",
    },
    {
      description: "top-level fragment definition order is preserved",
      left:  "query Q { node { ...A ...B } } fragment A on Node { a } fragment B on Node { b }",
      right: "query Q { node { ...A ...B } } fragment B on Node { b } fragment A on Node { a }",
    },
  ]

  equivalent_pairs.each do |pair|
    it "treats #{pair[:description]} as equivalent" do
      assert_equal fingerprint(pair[:left]), fingerprint(pair[:right])
    end
  end

  distinct_pairs.each do |pair|
    it "keeps #{pair[:description]} distinct" do
      refute_equal fingerprint(pair[:left]), fingerprint(pair[:right])
    end
  end

  it "is stable across calls and independent of whitespace" do
    query = "{ user(id: \"1\") { name email } }"
    pretty = "{\n  user(id: \"1\") {\n    name\n    email\n  }\n}\n"
    result_a = GraphQL::Language::StructuralFingerprint.generate(query)
    assert_equal result_a.fingerprint, GraphQL::Language::StructuralFingerprint.generate(query).fingerprint
    assert_equal result_a.fingerprint, GraphQL::Language::StructuralFingerprint.generate(pretty).fingerprint
  end

  it "accepts a parsed document without re-parsing" do
    document = GraphQL.parse("{ user(id: \"1\") { name } }")
    parsed_fingerprint = GraphQL::Language::StructuralFingerprint.generate(document).fingerprint
    assert_equal parsed_fingerprint, fingerprint("{ user(id: \"1\") { name } }")
  end

  it "rejects unsupported inputs" do
    err = assert_raises(ArgumentError) do
      GraphQL::Language::StructuralFingerprint.generate(123)
    end
    assert_includes err.message, "String or GraphQL::Language::Nodes::Document"
  end

  it "does not modify the input AST" do
    document = GraphQL.parse(
      "query Q($id: ID!, $n: Int) { user(n: $n, id: $id) { ...F } } fragment F on User { x(a: 1, b: 2) }",
    )
    original_query_string = document.to_query_string
    original_definition = document.definitions.first
    original_variables = original_definition.variables
    original_arguments = original_definition.selections.first.arguments
    original_fragment = document.definitions.last

    GraphQL::Language::StructuralFingerprint.generate(document)

    assert_equal original_query_string, document.to_query_string
    assert_same original_definition, document.definitions.first
    assert_same original_variables, document.definitions.first.variables
    assert_same original_arguments, document.definitions.first.selections.first.arguments
    assert_same original_fragment, document.definitions.last
  end

  it "returns a fresh, mutable canonical document" do
    result = GraphQL::Language::StructuralFingerprint.generate("query Q($id: ID!) { user(id: $id) { name } }")
    refute_same GraphQL.parse("query Q($id: ID!) { user(id: $id) { name } }"), result.canonical_document
    assert result.canonical_document.respond_to?(:merge)
  end

  describe "canonical document" do
    it "renames variables by declaration order" do
      assert_equal(
        "query Q($v0: ID!, $v1: Int = 10) { user(first: $v1, id: $v0) { name } }",
        canonical(
          "query Q($who: ID!, $limit: Int = 10) { user(id: $who, first: $limit) { name } }",
        ).gsub(/\s+/, " ").strip,
      )
    end

    it "renames fragments by definition order, including spreads" do
      query = <<~GRAPHQL
        query Q($id: ID!) { node(id: $id) { ...Second } }
        fragment Second on User { ...First }
        fragment First on User { name }
      GRAPHQL
      assert_equal(
        "query Q($v0: ID!) { node(id: $v0) { ...f0 } } fragment f0 on User { ...f1 } fragment f1 on User { name }",
        canonical(query).gsub(/\s+/, " ").strip,
      )
    end

    it "sorts arguments and input object fields but not lists" do
      query = '{ user(zzz: {d: 4, c: [3, 2]}, aaa: 1) { name } }'
      assert_equal(
        'query { user(aaa: 1, zzz: {c: [3, 2], d: 4}) { name } }',
        canonical(query).gsub(/\s+/, " ").strip,
      )
    end

    it "preserves aliases, field order and directive order" do
      query = "query Q { user @a @b { z aaa: zzz email name } }"
      assert_equal(
        "query Q { user @a @b { z aaa: zzz email name } }",
        canonical(query).gsub(/\s+/, " ").strip,
      )
    end

    it "sorts input object fields inside variable default values" do
      query = "query Q($f: F = {b: 2, a: 1}) { user(filter: $f) { name } }"
      assert_equal(
        "query Q($v0: F = {a: 1, b: 2}) { user(filter: $v0) { name } }",
        canonical(query).gsub(/\s+/, " ").strip,
      )
    end
  end

  describe "fragments shared by several operations" do
    it "renames the fragment's variables when every operation agrees" do
      left = <<~GRAPHQL
        query A($x: ID!) { node(id: $x) { ...F } }
        query B($x: ID!) { other(id: $x) { ...F } }
        fragment F on Node { f(id: $x) }
      GRAPHQL
      right = <<~GRAPHQL
        query A($z: ID!) { node(id: $z) { ...G } }
        query B($z: ID!) { other(id: $z) { ...G } }
        fragment G on Node { f(id: $z) }
      GRAPHQL
      assert_equal fingerprint(left), fingerprint(right)
    end

    it "does not rename a fragment variable when operations disagree (conservative)" do
      # In A, $x is the first declared variable; in B it is the second one.
      # A canonical rename inside the shared fragment would be ambiguous, so
      # the fragment keeps its original variable name.
      query = <<~GRAPHQL
        query A($x: ID!) { node(id: $x) { ...F } }
        query B($y: ID!, $x: ID!) { other(id: $y, nid: $x) { ...F } }
        fragment F on Node { f(id: $x) }
      GRAPHQL
      assert_includes canonical(query), "f(id: $x)"
      refute_includes canonical(query), "f(id: $v"
    end
  end
end

