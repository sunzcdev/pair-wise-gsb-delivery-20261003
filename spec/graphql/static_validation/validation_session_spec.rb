# frozen_string_literal: true
require "spec_helper"

describe GraphQL::StaticValidation::ValidationSession do
  let(:validator) { GraphQL::StaticValidation::Validator.new(schema: Dummy::Schema) }

  def run_session(query_or_string, budget:, **session_opts)
    query = query_or_string.is_a?(GraphQL::Query) ? query_or_string : GraphQL::Query.new(Dummy::Schema, query_or_string)
    session = validator.validation_session(query, **session_opts)
    result = nil
    loop do
      result = session.advance(work_units: budget)
      break if result.finished?
    end
    [session, result]
  end

  def full_errors(query_string, **opts)
    validator.validate(GraphQL::Query.new(Dummy::Schema, query_string), **opts)[:errors].map(&:to_h)
  end

  let(:fragments_query) do
    <<~GRAPHQL
      query getCheese($id: Int!) {
        cheese(id: $id) {
          ... cheeseFields
          source
          bogus
        }
      }
      fragment cheeseFields on Cheese {
        id
        similarCheese(source: COW) {
          ... cheeseFields
        }
      }
    GRAPHQL
  end

  describe "status lifecycle" do
    it "starts unfinished and ends completed" do
      query = GraphQL::Query.new(Dummy::Schema, "{ __typename }")
      session = validator.validation_session(query)
      assert session.unfinished?
      refute session.completed?
      result = session.advance(work_units: 1000)
      assert result.completed?
      assert result.valid?
      assert session.completed?
      assert_equal GraphQL::StaticValidation::ValidationSession::COMPLETED, result.status
    end

    it "returns UNFINISHED until the whole document has been visited" do
      query = GraphQL::Query.new(Dummy::Schema, fragments_query)
      session = validator.validation_session(query)
      statuses_seen = []
      loop do
        result = session.advance(work_units: 2)
        statuses_seen << result.status
        break if result.finished?
      end
      assert_includes statuses_seen, GraphQL::StaticValidation::ValidationSession::UNFINISHED
      assert_equal GraphQL::StaticValidation::ValidationSession::COMPLETED, statuses_seen.last
    end

    it "treats a zero budget as no progress" do
      query = GraphQL::Query.new(Dummy::Schema, "{ __typename }")
      session = validator.validation_session(query)
      assert session.advance(work_units: 0).unfinished?
      assert session.advance(work_units: 0).unfinished?
      assert session.advance(work_units: 1000).completed?
    end

    it "rejects a negative budget" do
      query = GraphQL::Query.new(Dummy::Schema, "{ __typename }")
      session = validator.validation_session(query)
      err = assert_raises(ArgumentError) { session.advance(work_units: -3) }
      assert_match(/work_units/, err.message)
    end
  end

  describe "error parity with Validator#validate" do
    [
      "{ __typename }",
      "query Q($id: Int!) { cheese(id: $id) { id bogus } cheese(id: 1) { bad } }",
      "query Q { cheese(id: 1) { ... on Cheese { id ... Cyclic } } } fragment Cyclic on Cheese { id ... Cyclic }",
      "{ cheese(id: 1) { ... Nope } }",
      "{ cheese(id: 1) { id id } }",
      "{ cheese(id: 1) { flavor: id id: flavor } }",
      "query Q { cheese(id: 1) { id } } query Q { cheese(id: 2) { id } }",
      "{ cheese(id: \"wrong\") { id } }",
      "{ cheese(id: 1) @nope { id } }",
      "{ cheese(id: 1) { id } } # trailing",
    ].each_with_index do |query_string, idx|
      [1, 2, 3, 7, 29, 1000].each do |budget|
        it "produces identical errors in the same order (query #{idx}, budget #{budget})" do
          expected = full_errors(query_string)
          _session, result = run_session(query_string, budget: budget)
          assert result.completed?
          assert_equal expected, result.errors.map(&:to_h)
        end
      end
    end

    it "honors max_errors like the non-pausable validator" do
      query_string = "{ cheese(id: 1) { a b c d e } }"
      expected = full_errors(query_string, max_errors: 2)
      _session, result = run_session(query_string, budget: 1, max_errors: 2)
      assert_equal expected, result.errors.map(&:to_h)
    end
  end

  describe "partial results are not valid" do
    it "is never valid while unfinished, even when nothing has failed yet" do
      query = GraphQL::Query.new(Dummy::Schema, "{ __typename }")
      session = validator.validation_session(query)
      result = session.advance(work_units: 0)
      assert result.unfinished?
      refute result.valid?
    end

    it "is invalid while unfinished for a document that ultimately fails" do
      session = validator.validation_session(GraphQL::Query.new(Dummy::Schema, fragments_query))
      results = []
      loop do
        result = session.advance(work_units: 1)
        results << result
        break if result.finished?
      end
      unfinished = results[0...-1]
      assert unfinished.any?
      assert unfinished.none?(&:valid?)
      assert results.last.completed?
      refute results.last.valid?
    end

    it "only a completed error-free result is valid" do
      _s, ok = run_session("{ __typename }", budget: 1)
      assert ok.completed? && ok.valid?
      _s, bad = run_session("{ cheese(id: 1) { nope } }", budget: 1)
      assert bad.completed?
      refute bad.valid?
    end
  end

  describe "interleaved sessions" do
    it "never share state or errors" do
      q_a = "{ cheese(id: 1) { id errorA } }"
      q_b = "{ cheese(id: 1) { id errorB } }"
      session_a = validator.validation_session(GraphQL::Query.new(Dummy::Schema, q_a))
      session_b = validator.validation_session(GraphQL::Query.new(Dummy::Schema, q_b))

      result_a = result_b = nil
      loop do
        result_a = session_a.advance(work_units: 2)
        result_b = session_b.advance(work_units: 2)
        break unless result_a.unfinished? || result_b.unfinished?
      end

      a_messages = result_a.errors.map(&:message)
      b_messages = result_b.errors.map(&:message)
      assert(a_messages.any? { |m| m.include?("errorA") })
      assert(b_messages.any? { |m| m.include?("errorB") })
      assert_empty(a_messages.select { |m| m.include?("errorB") })
      assert_empty(b_messages.select { |m| m.include?("errorA") })
    end

    it "have independent visitor contexts" do
      q1 = GraphQL::Query.new(Dummy::Schema, "{ __typename }")
      q2 = GraphQL::Query.new(Dummy::Schema, "{ cheese(id: 1) { id } }")
      s1 = validator.validation_session(q1)
      s2 = validator.validation_session(q2)
      s1.advance(work_units: 1)
      s2.advance(work_units: 1)
      refute_same s1.context, s2.context
      refute_same s1.context.visitor, s2.context.visitor
      refute_same s1.context.errors, s2.context.errors
      # Advancing one never advances the other
      assert_equal 0, s2.context.errors.size
    end
  end

  describe "cancellation" do
    it "moves to CANCELLED and keeps returning the cancelled result" do
      session = validator.validation_session(GraphQL::Query.new(Dummy::Schema, fragments_query))
      session.advance(work_units: 1)
      result = session.cancel
      assert result.cancelled?
      refute result.valid?
      assert session.cancelled?
      again = session.advance(work_units: 1000)
      assert again.cancelled?
      assert_equal GraphQL::StaticValidation::ValidationSession::CANCELLED, again.status
    end

    it "does not run rule callbacks after cancellation" do
      visited_fields = []
      tracking_rule = Module.new do
        define_method(:on_field) do |node, parent|
          visited_fields << node.name
          super(node, parent)
        end
      end
      rules = [tracking_rule] + GraphQL::StaticValidation::ALL_RULES
      session = validator.validation_session(
        GraphQL::Query.new(Dummy::Schema, "{ cheese(id: 1) { id flavor source } }"),
        rules: rules,
      )
      session.advance(work_units: 3)
      session.cancel
      snapshot = visited_fields.dup
      session.advance(work_units: 1000)
      assert_equal snapshot, visited_fields
    end

    it "does not invoke dependency-resolve callbacks after cancellation" do
      resolved = []
      dependency_rule = Module.new do
        def on_document(node, parent)
          context.on_dependency_resolve { |definition, spreads, fragment|
            $resolved_calls << [definition, spreads, fragment]
          }
          super(node, parent)
        end
      end
      query_string = <<~GRAPHQL
        query Q { cheese(id: 1) { ... F1 ... F2 } }
        fragment F1 on Cheese { id }
        fragment F2 on Cheese { id flavor }
      GRAPHQL

      $resolved_calls = []
      session = validator.validation_session(
        GraphQL::Query.new(Dummy::Schema, query_string),
        rules: [dependency_rule],
      )
      session.advance(work_units: 1)
      session.cancel
      session.advance(work_units: 10_000)
      assert_empty $resolved_calls

      # A non-cancelled session does run them (guard against a vacuous test)
      $resolved_calls = []
      _s, result = run_session(query_string, budget: 10_000, rules: [dependency_rule])
      assert result.completed?
      assert $resolved_calls.any?
    end

    it "can be cancelled from inside a rule callback and never resumes" do
      visited_after = []
      cancelling_rule = Module.new do
        define_method(:on_field) do |node, parent|
          context.session.cancel if node.name == "flavor"
          visited_after << node.name
          super(node, parent)
        end
      end
      rules = [cancelling_rule] + GraphQL::StaticValidation::ALL_RULES
      validator_with_rule = GraphQL::StaticValidation::Validator.new(schema: Dummy::Schema, rules: rules)
      session = validator_with_rule.validation_session(
        GraphQL::Query.new(Dummy::Schema, "{ cheese(id: 1) { id flavor source } }"),
      )
      result = nil
      loop do
        result = session.advance(work_units: 1)
        break if result.finished?
      end
      assert result.cancelled?
      assert_includes visited_after, "flavor"
      refute_includes visited_after, "source"
      # cancel is idempotent
      assert session.cancel.cancelled?
    end

    it "is not treated as a valid query" do
      session = validator.validation_session(GraphQL::Query.new(Dummy::Schema, "{ __typename }"))
      session.advance(work_units: 0)
      result = session.cancel
      refute result.valid?
    end
  end

  describe "validate: false" do
    it "completes immediately with no errors, like Validator#validate" do
      session = validator.validation_session(GraphQL::Query.new(Dummy::Schema, "{ bogus }"), validate: false)
      result = session.advance(work_units: 0)
      assert result.completed?
      assert result.valid?
      assert_empty result.errors
    end
  end

  describe "tracing" do
    it "emits one begin/end validate pair spanning all turns" do
      trace_events = []
      custom_trace = Class.new(GraphQL::Tracing::Trace) do
        define_method(:begin_validate) do |query, validate|
          trace_events << [:begin, validate]
        end
        define_method(:end_validate) do |query, validate, errors|
          trace_events << [:end, validate, errors.size]
        end
      end
      query = GraphQL::Query.new(
        Dummy::Schema,
        "{ cheese(id: 1) { bogus } }",
        context: { trace: custom_trace.new },
      )
      session = validator.validation_session(query)
      100.times { break if session.advance(work_units: 1).finished? }
      assert_equal [[:begin, true]], trace_events.select { |t| t[0] == :begin }
      assert_equal [[:end, true, 1]], trace_events.select { |t| t[0] == :end }
    end
  end
end
