# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Analysis::FieldPresence do
  module FP
    module Node
      include GraphQL::Schema::Interface
      field :id, ID, null: false
      field :name, String, null: true
      field :child, Node, null: true
    end

    module Audited
      include GraphQL::Schema::Interface
      field :audit_trail, String, null: true
    end

    class ThingA < GraphQL::Schema::Object
      implements Node
      implements Audited
      field :audit_trail, String, null: true
      field :child, Node, null: true
    end

    class ThingB < GraphQL::Schema::Object
      implements Node
      implements Audited
      field :audit_trail, String, null: true
      field :child, Node, null: true
    end

    class Other < GraphQL::Schema::Object
      implements Node
      field :child, Node, null: true
    end

    class Holder < GraphQL::Schema::Union
      possible_types ThingA, ThingB
    end

    class Root < GraphQL::Schema::Object
      field :thing, ThingA, null: true
      field :node, Node, null: true
      field :nodes, [Node], null: false
      field :holder, Holder, null: true
      field :resolved_thing, ThingA, null: true

      def resolved_thing
        raise "resolvers must not be called by field presence analysis"
      end
    end

    class Schema < GraphQL::Schema
      query Root
      orphan_types ThingA, ThingB, Other
    end
  end

  def analyze(query_string, path, variables: {}, operation_name: nil)
    query = GraphQL::Query.new(FP::Schema, query_string, variables: variables)
    GraphQL::Analysis::FieldPresence.analyze(query, path, operation_name: operation_name)
  end

  describe "#status" do
    it "is :always for a plain field" do
      result = analyze("{ thing { auditTrail } }", %w[thing auditTrail])
      assert_equal :always, result.status
      assert result.always?
      refute result.never?
      refute result.variable?
      assert result.possible?
    end

    it "is :never when the field is not selected" do
      result = analyze("{ thing { id } }", %w[thing auditTrail])
      assert_equal :never, result.status
      assert result.never?
      refute result.possible?
      assert_equal [], result.witnesses
    end

    it "is :never for a nonexistent field" do
      result = analyze("{ thing { id } }", %w[thing nope])
      assert_equal :never, result.status
    end

    it "accepts a dot-separated string path" do
      result = analyze("{ thing { auditTrail } }", "thing.auditTrail")
      assert_equal :always, result.status
    end

    it "matches response aliases" do
      result = analyze("{ t: thing { trail: auditTrail } }", %w[t trail])
      assert_equal :always, result.status
    end

    it "analyzes a named operation" do
      doc = "query A { thing { auditTrail } } query B { thing { id } }"
      assert_equal :always, analyze(doc, %w[thing auditTrail], operation_name: "A").status
      assert_equal :never, analyze(doc, %w[thing auditTrail], operation_name: "B").status
    end

    it "raises when the named operation does not exist" do
      query = GraphQL::Query.new(FP::Schema, "query A { thing { id } }")
      err = assert_raises(ArgumentError) do
        GraphQL::Analysis::FieldPresence.analyze(query, %w[thing id], operation_name: "Z")
      end
      assert_match(/no operation named "Z"/, err.message)
    end

    it "raises for an empty path" do
      query = GraphQL::Query.new(FP::Schema, "{ thing { id } }")
      assert_raises(ArgumentError) { GraphQL::Analysis::FieldPresence.analyze(query, []) }
    end
  end

  describe "@skip / @include with literal Booleans" do
    it "treats @include(if: true) as unconditional" do
      result = analyze("{ thing @include(if: true) { auditTrail } }", %w[thing auditTrail])
      assert_equal :always, result.status
    end

    it "treats @include(if: false) as impossible" do
      result = analyze("{ thing @include(if: false) { auditTrail } }", %w[thing auditTrail])
      assert_equal :never, result.status
    end

    it "treats @skip(if: true) as impossible" do
      result = analyze("{ thing @skip(if: true) { auditTrail } }", %w[thing auditTrail])
      assert_equal :never, result.status
    end

    it "treats @skip(if: false) as unconditional" do
      result = analyze("{ thing @skip(if: false) { auditTrail } }", %w[thing auditTrail])
      assert_equal :always, result.status
    end

    it "is :never when @include and @skip take the same variable" do
      doc = "query($v: Boolean!){ thing @include(if: $v) @skip(if: $v) { auditTrail } }"
      assert_equal :never, analyze(doc, %w[thing auditTrail], variables: { "v" => true }).status
      assert_equal :never, analyze(doc, %w[thing auditTrail], variables: { "v" => false }).status
    end
  end

  describe "@skip / @include with variables" do
    it "is :variable for a free Boolean variable" do
      doc = "query($v: Boolean!){ thing @include(if: $v) { auditTrail } }"
      result = analyze(doc, %w[thing auditTrail])
      assert_equal :variable, result.status
      assert_equal ["v"], result.variables
      assert_equal({ "v" => true }, result.witnesses.first.variables)
    end

    it "uses provided variable values" do
      doc = "query($v: Boolean!){ thing @include(if: $v) { auditTrail } }"
      assert_equal :always, analyze(doc, %w[thing auditTrail], variables: { "v" => true }).status
      assert_equal :never, analyze(doc, %w[thing auditTrail], variables: { "v" => false }).status
    end

    it "uses variable definition default values" do
      assert_equal :always, analyze(
        "query($v: Boolean = true){ thing @include(if: $v) { auditTrail } }",
        %w[thing auditTrail]
      ).status
      assert_equal :never, analyze(
        "query($v: Boolean = false){ thing @include(if: $v) { auditTrail } }",
        %w[thing auditTrail]
      ).status
    end

    it "lets a provided value override the default" do
      doc = "query($v: Boolean = false){ thing @include(if: $v) { auditTrail } }"
      assert_equal :always, analyze(doc, %w[thing auditTrail], variables: { "v" => true }).status
    end

    it "conjoins nested conditions" do
      doc = "query($a: Boolean, $b: Boolean){ thing @include(if: $a) { auditTrail @skip(if: $b) } }"
      result = analyze(doc, %w[thing auditTrail])
      assert_equal :variable, result.status
      assert_equal ["a", "b"], result.variables
      assert_equal({ "a" => true, "b" => false }, result.witnesses.first.variables)
    end

    it "detects contradictory nested conditions with a default" do
      doc = "query($v: Boolean = true){ thing @include(if: $v) { auditTrail @skip(if: $v) } }"
      assert_equal :never, analyze(doc, %w[thing auditTrail]).status
    end

    it "provides a witness proving the field can appear with free variables" do
      doc = "query($v: Boolean!){ thing { auditTrail @include(if: $v) } }"
      result = analyze(doc, %w[thing auditTrail])
      assert_equal :variable, result.status
      assert_equal({ "v" => true }, result.witnesses.first.variables)
    end
  end

  describe "fragment type conditions" do
    it "is :variable when a field is behind an object condition on an interface" do
      result = analyze("{ node { ... on ThingA { auditTrail } } }", %w[node auditTrail])
      assert_equal :variable, result.status
      assert_equal [nil, "ThingA"], result.witnesses.first.types
      assert_includes result.witnesses.first.route, "... on ThingA"
    end

    it "is :always when a condition matches the concrete parent type" do
      result = analyze("{ thing { ... on ThingA { auditTrail } } }", %w[thing auditTrail])
      assert_equal :always, result.status
      assert_equal [nil, nil], result.witnesses.first.types
    end

    it "is :always when a condition is the same interface as the parent" do
      result = analyze("{ node { ... on Node { name } } }", %w[node name])
      assert_equal :always, result.status
    end

    it "is :never for an impossible object type on an object parent" do
      result = analyze("{ thing { ... on Other { auditTrail } } }", %w[thing auditTrail])
      assert_equal :never, result.status
    end

    it "is :never for nested contradictory type conditions" do
      doc = "{ node { ... on ThingA { ... on ThingB { auditTrail } } } }"
      assert_equal :never, analyze(doc, %w[node auditTrail]).status
    end

    it "combines the same field reached through several fragment paths (OR)" do
      doc = "{ node { ... on ThingA { auditTrail } ... on ThingB { auditTrail } } }"
      result = analyze(doc, %w[node auditTrail])
      assert_equal :variable, result.status
      assert_equal ["ThingA", "ThingB"], result.witnesses.map { |w| w.types.last }.sort
    end

    it "treats one unconditional and one conditional path as :always" do
      doc = "{ node { name ... on ThingA { name } } }"
      assert_equal :always, analyze(doc, %w[node name]).status
    end

    it "tracks type conditions at every level of a deep path" do
      doc = "{ node { ... on ThingA { child { ... on ThingB { auditTrail } } } } }"
      result = analyze(doc, %w[node child auditTrail])
      assert_equal :variable, result.status
      assert_equal [nil, "ThingA", "ThingB"], result.witnesses.first.types
    end

    it "follows named fragment spreads" do
      doc = "{ node { ...Trail } } fragment Trail on Node { ... on ThingA { auditTrail } }"
      result = analyze(doc, %w[node auditTrail])
      assert_equal :variable, result.status
      assert_equal [nil, "ThingA"], result.witnesses.first.types
      assert_includes result.witnesses.first.route, "...Trail"
    end

    it "applies the fragment definition's type condition" do
      doc = "{ node { ...A } } fragment A on Other { id }"
      assert_equal :variable, analyze(doc, %w[node id]).status

      doc = "{ node { ...A } } fragment A on Other { auditTrail }"
      assert_equal :never, analyze(doc, %w[node auditTrail]).status
    end

    it "prunes a spread whose definition type cannot hold" do
      doc = "{ thing { ...A } } fragment A on Other { auditTrail }"
      assert_equal :never, analyze(doc, %w[thing auditTrail]).status
    end

    it "does not loop on a fragment spread cycle" do
      doc = "{ node { ...A } } fragment A on Node { ...B } fragment B on Node { ...A name }"
      assert_equal :always, analyze(doc, %w[node name]).status
    end

    it "works through list-returning fields" do
      doc = "{ nodes { ... on ThingA { auditTrail } } }"
      result = analyze(doc, %w[nodes auditTrail])
      assert_equal :variable, result.status
      assert_equal [nil, "ThingA"], result.witnesses.first.types
    end

    it "works through union-returning fields" do
      doc = "{ holder { ... on ThingA { auditTrail } } }"
      result = analyze(doc, %w[holder auditTrail])
      assert_equal :variable, result.status
      assert_equal [nil, "ThingA"], result.witnesses.first.types

      assert_equal :never, analyze(
        "{ holder { ... on Other { id } } }", %w[holder id]
      ).status
    end

    it "gives a witness per union member across fragments" do
      doc = "{ holder { ... on ThingA { auditTrail } ... on ThingB { auditTrail } } }"
      result = analyze(doc, %w[holder auditTrail])
      assert_equal :variable, result.status
      assert_equal ["ThingA", "ThingB"], result.witnesses.map { |w| w.types.last }.sort
    end
  end

  describe "directives on fragments" do
    it "honors @include on a fragment definition" do
      doc = "query($v: Boolean!) { node { ...A } } " \
            "fragment A on ThingA @include(if: $v) { auditTrail }"
      assert_equal :variable, analyze(doc, %w[node auditTrail]).status
      assert_equal :variable, analyze(doc, %w[node auditTrail], variables: { "v" => true }).status
      assert_equal :never, analyze(doc, %w[node auditTrail], variables: { "v" => false }).status
      # With the value provided it reduces to a constant; only the runtime
      # object type remains unknown.
      provided = analyze(doc, %w[node auditTrail], variables: { "v" => true })
      assert_equal({}, provided.witnesses.first.variables)
      assert_equal [nil, "ThingA"], provided.witnesses.first.types
      # Without a value the Boolean requirement is exposed as a witness.
      free = analyze(doc, %w[node auditTrail])
      assert_equal({ "v" => true }, free.witnesses.first.variables)
    end

    it "conjoins @include on the spread and on the field" do
      doc = "query($a: Boolean!, $b: Boolean!) { node { ...A } } " \
            "fragment A on ThingA @include(if: $a) { auditTrail @include(if: $b) }"
      result = analyze(doc, %w[node auditTrail])
      assert_equal :variable, result.status
      assert_equal ["a", "b"], result.variables.sort
      witness = result.witnesses.first.variables
      assert_equal true, witness["a"]
      assert_equal true, witness["b"]
    end
  end

  describe "unknown custom directives" do
    it "does not assume an unknown directive passes and is :variable" do
      result = analyze("{ thing @cache { auditTrail } }", %w[thing auditTrail])
      assert_equal :variable, result.status
      assert_equal ["cache"], result.unknown_gates.map { |g| g[:directive] }
      gate = result.witnesses.first.unknown_gates.first
      assert_equal "cache", gate[:directive]
      assert_equal "thing", gate[:location]
    end

    it "records unknown directives on an inline fragment" do
      result = analyze("{ node { ... on ThingA @cache { auditTrail } } }", %w[node auditTrail])
      assert_equal :variable, result.status
      assert_includes result.unknown_gates.map { |g| g[:directive] }, "cache"
    end
  end

  describe "no resolver calls" do
    it "never invokes field resolution" do
      # resolvedThing's resolver raises if called.
      result = analyze("{ resolvedThing { auditTrail } }", %w[resolvedThing auditTrail])
      assert_equal :always, result.status
    end
  end

  describe "Result predicate methods" do
    it "exposes status predicates" do
      always = analyze("{ thing { auditTrail } }", %w[thing auditTrail])
      never = analyze("{ thing { id } }", %w[thing auditTrail])
      vari = analyze(
        "query($v: Boolean!){ thing @include(if: $v) { auditTrail } }",
        %w[thing auditTrail]
      )
      assert always.always?
      assert never.never?
      assert vari.variable?
      assert vari.possible?
      refute never.possible?
    end
  end
end

