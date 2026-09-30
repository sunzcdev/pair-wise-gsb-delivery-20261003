# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Language::StructuralFingerprint do
  def fingerprint(query)
    GraphQL::Language::StructuralFingerprint.generate(query)
  end

  def normalized(query)
    GraphQL::Language::StructuralFingerprint.normalize(query)
  end

  def assert_equivalent(left_string, right_string)
    left = GraphQL.parse(left_string)
    right = GraphQL.parse(right_string)
    left_before = left.to_query_string
    right_before = right.to_query_string
    assert_equal(fingerprint(left), fingerprint(right),
      "expected equivalent queries to share a fingerprint")
    # Normalization must never touch the input AST.
    assert_equal(left_before, left.to_query_string)
    assert_equal(right_before, right.to_query_string)
  end

  def refute_equivalent(left_string, right_string)
    left = GraphQL.parse(left_string)
    right = GraphQL.parse(right_string)
    left_before = left.to_query_string
    right_before = right.to_query_string
    refute_equal(fingerprint(left), fingerprint(right),
      "expected distinct queries to have different fingerprints")
    assert_equal(left_before, left.to_query_string)
    assert_equal(right_before, right.to_query_string)
  end

  describe "the result object" do
    let(:query_string) { "query Q($id: ID!) { node(id: $id) { name } }" }

    it "exposes an opaque fingerprint, a normalized document and its string" do
      result = GraphQL::Language::StructuralFingerprint.analyze(GraphQL.parse(query_string))
      assert_kind_of(String, result.fingerprint)
      assert(result.fingerprint.start_with?("structural/v1/"))
      assert_kind_of(GraphQL::Language::Nodes::Document, result.document)
      assert_equal(result.document.to_query_string, result.normalized_query_string)
    end

    it "accepts a query string as well as a document" do
      assert_equal(fingerprint(query_string), fingerprint(GraphQL.parse(query_string)))
    end

    it "is deterministic" do
      assert_equal(fingerprint(query_string), fingerprint(query_string))
    end

    it "raises for unsupported input" do
      assert_raises(ArgumentError) { GraphQL::Language::StructuralFingerprint.analyze(123) }
    end
  end

  describe "pairs that must be equivalent" do
    it "ignores incidental whitespace and formatting" do
      assert_equivalent(
        "{ node(id: 42) { name } }",
        "{\n  node(id: 42) {\n    name\n  }\n}\n",
      )
    end

    it "treats shorthand and explicit `query` the same" do
      assert_equivalent(
        "{ node { name } }",
        "query { node { name } }",
      )
    end

    it "renames variables consistently, including inside fragments" do
      assert_equivalent(
        "query Q($id: ID!) { node(id: $id) { ...F } } fragment F on T { x(arg: $id) }",
        "query Q($other: ID!) { node(id: $other) { ...G } } fragment G on T { x(arg: $other) }",
      )
    end

    it "assigns canonical names by declaration order" do
      assert_equivalent(
        "query Q($a: ID, $b: ID) { x(a: $a, b: $b) }",
        "query Q($x: ID, $y: ID) { x(a: $x, b: $y) }",
      )
    end

    it "renames fragments consistently across spreads and keeps cycles intact" do
      assert_equivalent(
        "{ a { ...One ...Two } } fragment One on T { x ...Two } fragment Two on T { y ...One }",
        "{ a { ...P ...Q } } fragment P on T { x ...Q } fragment Q on T { y ...P }",
      )
    end

    it "does not depend on the order fragment definitions appear" do
      assert_equivalent(
        "{ a { ...A ...B } } fragment A on T { x } fragment B on T { y }",
        "{ a { ...P ...Q } } fragment Q on T { y } fragment P on T { x }",
      )
    end

    it "normalizes the order of field arguments" do
      assert_equivalent(
        "{ node(id: 1, active: true) { name } }",
        "{ node(active: true, id: 1) { name } }",
      )
    end

    it "normalizes the order of directive arguments" do
      assert_equivalent(
        "{ node @skip(a: 1, b: 2) { name } }",
        "{ node @skip(b: 2, a: 1) { name } }",
      )
    end

    it "normalizes input object field order, recursively" do
      assert_equivalent(
        "{ node(input: { b: 2, a: { z: 1, y: 2 } }) { name } }",
        "{ node(input: { a: { y: 2, z: 1 }, b: 2 }) { name } }",
      )
    end

    it "canonicalizes variables in reordered arguments and input objects together" do
      assert_equivalent(
        "query Q($id: ID!) { node(id: $id, input: { b: 2, a: 1 }) { name } }",
        "query Q($who: ID!) { node(input: { a: 1, b: 2 }, id: $who) { name } }",
      )
    end

    it "treats list element formatting (not order) as insignificant" do
      assert_equivalent(
        "{ node(ids: [1, 2, 3]) { name } }",
        "{ node(ids: [1,2,3]) { name } }",
      )
    end
  end

  describe "pairs that must stay distinct" do
    it "keeps response aliases significant" do
      refute_equivalent(
        "{ node { name } }",
        "{ alias: node { name } }",
      )
    end

    it "distinguishes a changed alias even with the same field" do
      refute_equivalent(
        "{ first: node { name } }",
        "{ second: node { name } }",
      )
    end

    it "keeps selection field order significant" do
      refute_equivalent(
        "{ node { name title } }",
        "{ node { title name } }",
      )
    end

    it "keeps top-level selection order significant" do
      refute_equivalent(
        "{ a b }",
        "{ b a }",
      )
    end

    it "keeps list element order significant" do
      refute_equivalent(
        "{ node(ids: [1, 2]) }",
        "{ node(ids: [2, 1]) }",
      )
    end

    it "keeps directive order significant (it may be meaningful)" do
      refute_equivalent(
        "{ node @a @b { name } }",
        "{ node @b @a { name } }",
      )
    end

    it "does not treat an added directive as equivalent" do
      refute_equivalent(
        "{ node { name } }",
        "{ node @skip(if: true) { name } }",
      )
    end

    it "never executes custom scalar coercion: 1 vs 1.0 differ" do
      refute_equivalent(
        "{ node(weight: 1) }",
        "{ node(weight: 1.0) }",
      )
    end

    it "compares scalar literals syntactically, not by value guessing" do
      refute_equivalent(
        '{ node(when: "2024-01-01") }',
        "{ node(when: 2024) }",
      )
    end

    it "distinguishes an enum from a string" do
      refute_equivalent(
        "{ node(status: ACTIVE) }",
        '{ node(status: "ACTIVE") }',
      )
    end

    it "distinguishes null from an empty string" do
      refute_equivalent(
        "{ node(note: null) }",
        '{ node(note: "") }',
      )
    end

    it "keeps operation names significant" do
      refute_equivalent(
        "query One { node { name } }",
        "query Two { node { name } }",
      )
    end

    it "keeps operation types significant" do
      refute_equivalent(
        "query { node { name } }",
        "mutation { node { name } }",
      )
    end

    it "keeps fragment type conditions significant" do
      refute_equivalent(
        "{ ...F } fragment F on T { x }",
        "{ ...F } fragment F on U { x }",
      )
    end

    it "keeps variable types significant" do
      refute_equivalent(
        "query Q($id: ID!) { node(id: $id) }",
        "query Q($id: String!) { node(id: $id) }",
      )
    end

    it "keeps variable default values significant" do
      refute_equivalent(
        "query Q($first: Int = 1) { node(first: $first) }",
        "query Q($first: Int = 2) { node(first: $first) }",
      )
    end

    it "does not reorder variable definitions (only arguments/input fields are normalized)" do
      refute_equivalent(
        "query Q($a: ID, $b: ID) { x(a: $a, b: $b) }",
        "query Q($b: ID, $a: ID) { x(a: $a, b: $b) }",
      )
    end

    it "distinguishes a changed argument name even with the same value" do
      refute_equivalent(
        "{ node(id: 1) }",
        "{ node(idx: 1) }",
      )
    end

    it "distinguishes a changed input object key" do
      refute_equivalent(
        "{ node(input: { a: 1 }) }",
        "{ node(input: { b: 1 }) }",
      )
    end

    it "distinguishes changed selected fields inside a fragment" do
      refute_equivalent(
        "{ node { ...F } } fragment F on T { x }",
        "{ node { ...F } } fragment F on T { y }",
      )
    end
  end

  describe "the normalized document" do
    it "renames variables and fragments to canonical names" do
      doc = normalized("query Q($id: ID!) { node(id: $id) { ...F } } fragment F on T { x }")
      printed = doc.to_query_string
      assert_includes(printed, "$v0")
      refute_includes(printed, "$id")
      assert_includes(printed, "...F0")
      assert_includes(printed, "fragment F0 on T")
    end

    it "sorts arguments and input object fields" do
      doc = normalized("{ node(b: 1, a: 2, nested: { z: 1, y: 2 }) }")
      printed = doc.to_query_string
      assert(printed.index("a: 2") < printed.index("b: 1"))
      assert(printed.index("y: 2") < printed.index("z: 1"))
    end

    it "leaves selection and directive order in place" do
      doc = normalized("{ node @a @b { zeta alpha } }")
      printed = doc.to_query_string
      assert_includes(printed, "@a @b")
      assert(printed.index("zeta") < printed.index("alpha"))
    end

    it "does not mutate or return the input document" do
      original = GraphQL.parse("query Q($id: ID!) { node(b: 2, a: 1, id: $id) { ...F } } fragment F on T { x }")
      before = original.to_query_string
      normalized_doc = normalized(original)
      refute(original.equal?(normalized_doc))
      assert_equal(before, original.to_query_string)
      refute_includes(original.to_query_string, "$v0")
    end
  end
end
