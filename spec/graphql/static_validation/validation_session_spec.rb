# frozen_string_literal: true
require "spec_helper"

describe GraphQL::StaticValidation::ValidationSession do
  let(:validator) { GraphQL::StaticValidation::Validator.new(schema: Dummy::Schema) }

  def build_session(query_string, **opts)
    query = GraphQL::Query.new(Dummy::Schema, query_string)
    validator.validation_session(query, **opts)
  end

  def drive(session, budget)
    results = []
    while session.pending?
      results << session.advance(budget)
    end
    results
  end

  def full_errors(query_string, **opts)
    GraphQL::StaticValidation::Validator.new(schema: Dummy::Schema, rules: opts.delete(:rules) || GraphQL::StaticValidation::ALL_RULES)
      .validate(GraphQL::Query.new(Dummy::Schema, query_string), **opts)[:errors].map(&:to_h)
  end

  let(:valid_query_string) { "{ cheese(id: 1) { id flavor } }" }

  describe "lifecycle" do
    it "starts pending and completes with no errors for a valid query" do
      session = build_session(valid_query_string)
      assert session.pending?
      results = drive(session, 1)
      assert session.complete?
      assert_equal [], session.errors.map(&:to_h)
      assert results.size > 1, "a budget of 1 should require several advances"
      assert_equal :pending, results.first[:status]
      assert_equal :complete, results.last[:status]
    end

    it "accepts any non-negative integer budget, including 0" do
      session = build_session(valid_query_string)
      # A zero budget runs no work at all, so the session stays pending.
      assert_equal :pending, session.advance(0)[:status]
      assert session.pending?
      # A large budget finishes everything in one call.
      result = session.advance(1_000_000)
      assert_equal :complete, result[:status]
      assert_equal [], result[:errors]
    end

    it "rejects negative and non-integer budgets" do
      session = build_session(valid_query_string)
      err = assert_raises(ArgumentError) { session.advance(-1) }
      assert_match(/work_budget/, err.message)
      assert_raises(ArgumentError) { session.advance(1.5) }
    end

    it "returns the final result when advanced after completion" do
      session = build_session(valid_query_string)
      drive(session, 100)
      result = session.advance(100)
      assert_equal :complete, result[:status]
      assert_equal [], result[:errors]
    end

    it "completes immediately with no errors when validation is disabled" do
      session = build_session("{ cheese(id: 1) { nonexistent } }", validate: false)
      assert session.complete?
      assert_equal [], session.errors
    end
  end

  describe "partial progress is never a valid query" do
    it "exposes partial errors but stays pending until everything ran" do
      query_string = <<~GRAPHQL
        {
          cheese(id: 1) { nonsenseField }
          milk: nonsense2
        }
      GRAPHQL
      session = build_session(query_string)
      saw_pending_with_no_errors = false
      loop do
        result = session.advance(1)
        saw_pending_with_no_errors ||= (result[:status] == :pending && result[:errors].empty?)
        break if result[:status] != :pending
      end
      assert session.complete?
      assert saw_pending_with_no_errors, "a paused session must not report validity before completion"
      assert session.errors.size >= 2
    end
  end

  describe "error parity with uninterrupted validation" do
    def assert_parity(query_string, budgets)
      expected = full_errors(query_string)
      budgets.each do |budget|
        session = build_session(query_string)
        drive(session, budget)
        assert session.complete?, "budget=#{budget} did not finish"
        assert_equal expected, session.errors.map(&:to_h), "budget=#{budget}"
      end
    end

    it "matches when split into one-unit advances" do
      assert_parity(valid_query_string, [1])
    end

    it "matches for undefined fields and arguments" do
      query_string = <<~GRAPHQL
        query getCheese($id: Int!) {
          cheese(id: $undefinedVar, bogusArg: true) {
            nonsenseField
            id(nonsenseArg: 1)
            bogusField(bogusArg: true)
          }
          otherCheese: cheese(id: $id) { flavor }
        }
      GRAPHQL
      assert_parity(query_string, [1, 2, 3, 7, 1000])
    end

    it "matches for cyclic and unmet fragment dependencies" do
      query_string = <<~GRAPHQL
        query getCheese {
          cheese(id: 1) { ... cheeseFields }
        }
        fragment cheeseFields on Cheese {
          ... on Cheese { id, ... cheeseFields }
        }
      GRAPHQL
      assert_parity(query_string, [1, 2, 5, 500])

      missing = "{ cheese(id: 1) { ... nope } }"
      assert_parity(missing, [1, 3, 100])
    end

    it "matches for unused fragments and variables" do
      query_string = <<~GRAPHQL
        query Q($unused: Int) {
          cheese(id: 1) { id }
        }
        fragment Unused on Cheese { flavor }
      GRAPHQL
      assert_parity(query_string, [1, 4, 999])
    end

    it "matches for duplicate operation and fragment names" do
      query_string = <<~GRAPHQL
        { cheese(id: 1) { id } }
        { cheese(id: 2) { id } }
        fragment Dup on Cheese { id }
        fragment Dup on Cheese { flavor }
      GRAPHQL
      assert_parity(query_string, [1, 2, 80])
    end

    it "matches for fields that conflict when merged" do
      query_string = <<~GRAPHQL
        {
          cheese(id: 1) {
            x: id
            x: flavor
          }
        }
      GRAPHQL
      assert_parity(query_string, [1, 5, 200])
    end

    it "matches when the query contains schema definitions" do
      query_string = "type X { f: Int }"
      assert_parity(query_string, [1, 10])
    end

    it "honors max_errors exactly like the uninterrupted run" do
      query_string = "{ a: nope1 b: nope2 c: nope3 }"
      expected = full_errors(query_string, max_errors: 2)
      session = build_session(query_string, max_errors: 2)
      drive(session, 1)
      assert session.complete?
      assert_equal expected, session.errors.map(&:to_h)
      assert_equal 2, session.errors.size
    end
  end

  describe "intermediate state across advances" do
    it "resumes fragment-dependency bookkeeping for spreads defined later in the document" do
      query_string = <<~GRAPHQL
        { cheese(id: 1) { ... cheeseFields ... on Cheese { ... deeper } } }
        fragment cheeseFields on Cheese { flavor ... deeper }
        fragment deeper on Cheese { id }
      GRAPHQL
      [1, 2, 3].each do |budget|
        session = build_session(query_string)
        drive(session, budget)
        assert session.complete?
        assert_equal [], session.errors.map(&:to_h), "budget=#{budget}"
      end
    end

    it "uses a deterministic number of work units" do
      counts = 2.times.map do
        session = build_session(valid_query_string)
        advances = 0
        while session.pending?
          session.advance(1)
          advances += 1
        end
        advances
      end
      assert_equal counts.first, counts.last
    end
  end

  describe "interleaved sessions" do
    it "keeps independent context when advanced in lockstep" do
      ok_string = "{ cheese(id: 1) { id } }"
      bad_string = "{ cheese(id: 1) { totallyMissing } }"
      expected_bad = full_errors(bad_string)

      sessions = Array.new(3) do |i|
        build_session(i.even? ? ok_string : bad_string)
      end

      while sessions.any?(&:pending?)
        sessions.each { |s| s.advance(1) if s.pending? }
      end

      assert sessions.all?(&:complete?)
      assert_equal [], sessions[0].errors.map(&:to_h)
      assert_equal expected_bad, sessions[1].errors.map(&:to_h)
      assert_equal [], sessions[2].errors.map(&:to_h)
    end
  end

  describe "cancellation" do
    it "stops permanently without running further rule callbacks" do
      calls = []
      rule = Module.new do
        define_method(:on_field) do |node, parent|
          calls << node.name
          super(node, parent)
        end
      end
      custom_validator = GraphQL::StaticValidation::Validator.new(schema: Dummy::Schema, rules: [rule])
      query = GraphQL::Query.new(Dummy::Schema, "{ cheese(id: 1) { id flavor source } }")
      session = custom_validator.validation_session(query)

      session.advance(1)
      refute session.complete?
      snapshot = calls.dup
      assert_equal :cancelled, session.cancel
      assert session.cancelled?
      refute session.complete?

      assert_equal snapshot, calls
      result = session.advance(1_000)
      assert_equal :cancelled, result[:status]
      assert_equal snapshot, calls
    end

    it "is a no-op once the session completed" do
      session = build_session(valid_query_string)
      drive(session, 100)
      assert_equal :complete, session.cancel
      assert session.complete?
    end
  end

  describe "rules raising GraphQL::ExecutionError" do
    it "surfaces the error as the final result, split into small advances" do
      boom = Module.new do
        define_method(:on_field) do |node, parent|
          raise GraphQL::ExecutionError, "boom" if node.name == "cheese"
          super(node, parent)
        end
      end
      custom_validator = GraphQL::StaticValidation::Validator.new(schema: Dummy::Schema, rules: [boom])
      query = GraphQL::Query.new(Dummy::Schema, valid_query_string)
      session = custom_validator.validation_session(query)
      results = drive(session, 1)
      assert session.complete?
      assert_equal ["boom"], session.errors.map(&:message)
      assert_equal :complete, results.last[:status]
    end
  end

  describe "tracing" do
    it "opens and closes the validate trace around the whole session" do
      query = GraphQL::Query.new(Dummy::Schema, valid_query_string, context: { tracers: [TestTracing] })
      traces = TestTracing.with_trace do
        session = validator.validation_session(query)
        while session.pending?
          session.advance(1)
        end
      end
      validate_traces = traces.select { |t| t[:key] == "validate" }
      assert validate_traces.any?
    end
  end
end
