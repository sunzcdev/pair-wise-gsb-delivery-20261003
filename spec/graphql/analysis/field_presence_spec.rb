# frozen_string_literal: true
require "spec_helper"

describe GraphQL::Analysis::FieldPresence do
  module FieldPresenceTest
    class BaseObject < GraphQL::Schema::Object; end

    module Node
      include GraphQL::Schema::Interface
      field :id, ID, null: false
    end

    class AuditTrail < BaseObject
      field :id, ID, null: false
      field :entries, [String], null: false
    end

    class Invoice < BaseObject
      implements Node
      field :number, String, null: false
      field :audit_trail, AuditTrail, null: true
    end

    class Order < BaseObject
      implements Node
      field :audit_trail, AuditTrail, null: true
    end

    class BillingResult < GraphQL::Schema::Union
      possible_types Order, Invoice
    end

    class QueryRoot < BaseObject
      field :order, Order, null: true
      field :invoices, [Invoice], null: false
      field :billing, [BillingResult], null: false
      field :audit_trail, AuditTrail, null: true

      # If analysis ever (incorrectly) invokes resolvers, these blow up.
      def order
        raise "resolver must not be called"
      end

      def billing
        raise "resolver must not be called"
      end
    end

    class Schema < GraphQL::Schema
      query QueryRoot
      orphan_types Order, Invoice
    end
  end

  def analyze(query_string, path, **options)
    GraphQL::Analysis::FieldPresence.analyze(
      FieldPresenceTest::Schema,
      query_string,
      path,
      **options,
    )
  end

  describe "#verdict" do
    it "is always_present for an unconditional field" do
      result = analyze("{ order { auditTrail { id } } }", %w[order auditTrail id])
      assert_equal :always_present, result.verdict
      assert result.always_present?
      assert result.present?
    end

    it "is never_present when the field is absent at the path" do
      result = analyze("{ order { id } }", %w[order auditTrail])
      assert_equal :never_present, result.verdict
      assert result.never_present?
      refute result.present?
      assert_equal [], result.scenarios
    end

    it "is never_present when @skip(if: true) gates the only route" do
      result = analyze("{ order { auditTrail @skip(if: true) } }", %w[order auditTrail])
      assert_equal :never_present, result.verdict
    end

    it "is never_present when @include(if: false) gates the only route" do
      result = analyze("{ order { auditTrail @include(if: false) } }", %w[order auditTrail])
      assert_equal :never_present, result.verdict
    end

    it "is always_present with @skip(if: false)" do
      result = analyze("{ order { auditTrail @skip(if: false) } }", %w[order auditTrail])
      assert_equal :always_present, result.verdict
    end

    it "is always_present with @include(if: true)" do
      result = analyze("{ order { auditTrail @include(if: true) } }", %w[order auditTrail])
      assert_equal :always_present, result.verdict
    end

    it "treats @include(if: null) as non-gating" do
      query_string = "query($v: Boolean) { order { auditTrail @include(if: $v) } }"
      result = analyze(query_string, %w[order auditTrail], variables: { "v" => nil })
      assert_equal :always_present, result.verdict
    end

    it "depends on a boolean variable used in @include" do
      query_string = "query($show: Boolean!) { order { auditTrail @include(if: $show) } }"
      result = analyze(query_string, %w[order auditTrail])
      assert_equal :depends_on_variables, result.verdict
      assert result.depends_on_variables?
      assert_equal ["show"], result.referenced_variables
    end

    it "depends on a boolean variable used in @skip" do
      query_string = "query($hide: Boolean!) { order { auditTrail @skip(if: $hide) } }"
      result = analyze(query_string, %w[order auditTrail])
      assert_equal :depends_on_variables, result.verdict
      scenario = result.scenarios.first
      assert_equal({ "hide" => false }, scenario.variables)
    end

    it "combines nested @skip / @include with AND semantics" do
      query_string = <<~GRAPHQL
        query($a: Boolean!, $b: Boolean!) {
          order { auditTrail @include(if: $a) @skip(if: $b) }
        }
      GRAPHQL
      result = analyze(query_string, %w[order auditTrail])
      assert_equal :depends_on_variables, result.verdict
      assert_equal 1, result.scenarios.size
      assert_equal({ "a" => true, "b" => false }, result.scenarios.first.variables)
    end

    it "is never_present for contradictory conditions on one route" do
      query_string = <<~GRAPHQL
        query($a: Boolean!) {
          order { auditTrail @include(if: $a) @skip(if: $a) }
        }
      GRAPHQL
      result = analyze(query_string, %w[order auditTrail])
      assert_equal :never_present, result.verdict
    end

    it "ANDs directives on the parent field with those nested below" do
      query_string = <<~GRAPHQL
        query($a: Boolean!, $b: Boolean!) {
          order @include(if: $a) {
            auditTrail @include(if: $b)
          }
        }
      GRAPHQL
      result = analyze(query_string, %w[order auditTrail])
      assert_equal :depends_on_variables, result.verdict
      assert_equal({ "a" => true, "b" => true }, result.scenarios.first.variables)
    end

    it "follows a named fragment defined on an interface implemented by the scope" do
      query_string = <<~GRAPHQL
        query($show: Boolean!) {
          billing { ...NodeAudit @include(if: $show) }
        }
        fragment NodeAudit on Node { id }
      GRAPHQL
      result = analyze(query_string, %w[billing id])
      assert_equal :depends_on_variables, result.verdict
      assert_equal %w[Invoice Order], result.scenarios.first.type_candidates.last.sort
    end

    it "ORs routes entering the same field from different fragments" do
      query_string = <<~GRAPHQL
        query($a: Boolean!, $b: Boolean!) {
          billing {
            ... on Order { auditTrail @include(if: $a) }
            ... on Invoice { auditTrail @include(if: $b) }
          }
        }
      GRAPHQL
      result = analyze(query_string, %w[billing auditTrail])
      assert_equal :depends_on_variables, result.verdict
      witnesses = result.scenarios.map { |s| [s.variables, s.type_candidates.last] }
      assert_includes witnesses, [{ "a" => true }, %w[Order]]
      assert_includes witnesses, [{ "b" => true }, %w[Invoice]]
    end

    it "is always_present when every possible object type is covered unconditionally" do
      query_string = <<~GRAPHQL
        {
          billing {
            ... on Order { auditTrail }
            ... on Invoice { auditTrail }
          }
        }
      GRAPHQL
      result = analyze(query_string, %w[billing auditTrail])
      assert_equal :always_present, result.verdict
    end

    it "excludes type conditions that cannot hold simultaneously in the schema" do
      query_string = <<~GRAPHQL
        {
          billing {
            ... on Order {
              ... on Invoice { auditTrail }
            }
          }
        }
      GRAPHQL
      result = analyze(query_string, %w[billing auditTrail])
      assert_equal :never_present, result.verdict
    end

    it "excludes fields that do not exist on the narrowed concrete type" do
      query_string = "{ billing { ... on Order { number } } }"
      result = analyze(query_string, %w[billing number])
      assert_equal :never_present, result.verdict
    end

    it "follows named fragments and their spread directives" do
      query_string = <<~GRAPHQL
        query($show: Boolean!) {
          order { ...Audit @include(if: $show) }
        }
        fragment Audit on Order { auditTrail }
      GRAPHQL
      result = analyze(query_string, %w[order auditTrail])
      assert_equal :depends_on_variables, result.verdict
      assert_equal({ "show" => true }, result.scenarios.first.variables)
    end

    it "handles untyped inline fragments" do
      result = analyze("{ billing { ... { ... on Order { auditTrail } } } }", %w[billing auditTrail])
      assert_equal :depends_on_variables, result.verdict
      assert_equal %w[Order], result.scenarios.first.type_candidates.last
    end

    it "tolerates fragment spread cycles" do
      query_string = <<~GRAPHQL
        query($show: Boolean!) {
          billing { ...A }
        }
        fragment A on BillingResult { ...B ... on Order { auditTrail @include(if: $show) } }
        fragment B on BillingResult { ...A }
      GRAPHQL
      result = Timeout.timeout(5) do
        analyze(query_string, %w[billing auditTrail])
      end
      assert_equal :depends_on_variables, result.verdict
    end

    it "treats custom directives as opaque gates rather than as @include" do
      query_string = "{ order { auditTrail @featureFlag(name: \"audit\") } }"
      result = analyze(query_string, %w[order auditTrail])
      assert_equal :depends_on_variables, result.verdict
      assert result.scenarios.first.unknown_gate?
      assert_includes result.scenarios.first.unknown_gates, "unrecognized directive: @featureFlag"
    end

    it "treats a non-boolean variable in @include as an opaque gate" do
      query_string = "query($v: String) { order { auditTrail @include(if: $v) } }"
      result = analyze(query_string, %w[order auditTrail])
      assert_equal :depends_on_variables, result.verdict
      assert result.scenarios.first.unknown_gate?
    end
  end

  describe "variable default values" do
    it "does not treat a default-true variable as proof of always_present (it is overridable)" do
      query_string = "query($show: Boolean = true) { order { auditTrail @include(if: $show) } }"
      result = analyze(query_string, %w[order auditTrail])
      assert_equal :depends_on_variables, result.verdict
      # The default value already satisfies the gate, so no request value is needed.
      assert_equal({}, result.scenarios.first.variables)
    end

    it "considers a default-false variable when proving never_present" do
      query_string = "query($hide: Boolean = false) { order { auditTrail @skip(if: $hide) } }"
      result = analyze(query_string, %w[order auditTrail])
      # The variable is still overridable, so it can't prove always_present...
      assert_equal :depends_on_variables, result.verdict
    end

    it "uses a provided value that makes the gate pass to conclude always_present" do
      query_string = "query($show: Boolean = false) { order { auditTrail @include(if: $show) } }"
      result = analyze(query_string, %w[order auditTrail], variables: { "show" => true })
      assert_equal :always_present, result.verdict
      assert_equal({ "show" => true }, result.scenarios.first.variables)
    end

    it "uses a provided value that closes the gate to conclude never_present" do
      query_string = "query($show: Boolean = true) { order { auditTrail @include(if: $show) } }"
      result = analyze(query_string, %w[order auditTrail], variables: { "show" => false })
      assert_equal :never_present, result.verdict
    end
  end

  describe "path handling" do
    it "matches aliases via response keys" do
      query_string = "{ order { trail: auditTrail } }"
      assert_equal :always_present, analyze(query_string, %w[order trail]).verdict
      assert_equal :never_present, analyze(query_string, %w[order auditTrail]).verdict
    end

    it "accepts a dot-separated string path" do
      result = analyze("{ order { auditTrail } }", "order.auditTrail")
      assert_equal :always_present, result.verdict
    end

    it "accepts symbol segments" do
      result = analyze("{ order { auditTrail } }", [:order, :auditTrail])
      assert_equal :always_present, result.verdict
    end

    it "handles list fields in the middle of the path" do
      result = analyze("{ invoices { auditTrail { id } } }", %w[invoices auditTrail id])
      assert_equal :always_present, result.verdict
    end

    it "raises for an empty path" do
      assert_raises(ArgumentError) do
        analyze("{ order { auditTrail } }", [])
      end
    end
  end

  describe "witness scenarios" do
    it "exposes reviewable variable values, concrete types and the selection trail" do
      query_string = <<~GRAPHQL
        query($show: Boolean!) {
          billing { ... on Order { auditTrail @include(if: $show) } }
        }
      GRAPHQL
      result = analyze(query_string, %w[billing auditTrail])
      scenario = result.scenarios.first
      assert_equal({ "show" => true }, scenario.variables)
      assert_equal [%w[QueryRoot], %w[Order]], scenario.type_candidates
      kinds = scenario.selection_trail.map { |entry| entry[:kind] }
      assert_includes kinds, :operation
      assert_includes kinds, :field
      assert_includes kinds, :inline_fragment
      assert_includes kinds, :directive
    end
  end

  describe "operation selection" do
    let(:document) do
      <<~GRAPHQL
        query One { order { auditTrail @skip(if: true) } }
        query Two { order { auditTrail } }
      GRAPHQL
    end

    it "analyzes the named operation" do
      assert_equal :never_present, analyze(document, %w[order auditTrail], operation_name: "One").verdict
      assert_equal :always_present, analyze(document, %w[order auditTrail], operation_name: "Two").verdict
    end

    it "raises when the named operation does not exist" do
      assert_raises(GraphQL::Error) do
        analyze(document, %w[order auditTrail], operation_name: "Three")
      end
    end
  end

  describe "documents and queries" do
    it "accepts a parsed document" do
      doc = GraphQL.parse("{ order { auditTrail } }")
      result = GraphQL::Analysis::FieldPresence.analyze(FieldPresenceTest::Schema, doc, %w[order auditTrail])
      assert_equal :always_present, result.verdict
    end

    it "accepts a GraphQL::Query without executing it" do
      query = GraphQL::Query.new(FieldPresenceTest::Schema, "{ order { auditTrail } }", validate: false)
      result = GraphQL::Analysis::FieldPresence.analyze(FieldPresenceTest::Schema, query, %w[order auditTrail])
      assert_equal :always_present, result.verdict
    end
  end
end
