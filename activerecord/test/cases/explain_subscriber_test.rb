# frozen_string_literal: true

require "cases/helper"
require "active_record/explain_registry"

if ActiveRecord::Base.lease_connection.supports_explain?
  class ExplainSubscriberTest < ActiveRecord::TestCase
    SUBSCRIBER = ActiveRecord::ExplainRegistry::Subscriber.new

    def setup
      ActiveRecord::ExplainRegistry.reset
      ActiveRecord::ExplainRegistry.start
    end

    def test_collects_nothing_if_the_payload_has_an_exception
      SUBSCRIBER.finish(nil, nil, exception: Exception.new)
      assert_empty queries
    end

    def test_collects_nothing_for_ignored_payloads
      ActiveRecord::ExplainRegistry::Subscriber::IGNORED_PAYLOADS.each do |ip|
        SUBSCRIBER.finish(nil, nil, name: ip)
      end
      assert_empty queries
    end

    def test_collects_nothing_if_collect_is_false
      ActiveRecord::ExplainRegistry.collect = false
      SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select 1 from users", binds: [1, 2])
      assert_empty queries
    end

    def test_collects_pairs_of_queries_and_binds
      sql   = "select 1 from users"
      binds = [1, 2]
      SUBSCRIBER.finish(nil, nil, name: "SQL", sql: sql, binds: binds)
      assert_equal 1, queries.size
      assert_equal sql, queries[0][0]
      assert_equal binds, queries[0][1]
    end

    def test_capture_keeps_queries_out_of_the_flat_registry
      capture = ActiveRecord::ExplainRegistry.capture do
        SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select 1 from users", binds: [])
      end

      assert_equal 1, capture.size
      assert_empty queries
    end

    def test_nested_capture_owns_its_queries
      inner = nil
      outer = ActiveRecord::ExplainRegistry.capture("outer") do
        SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select 1 from users", binds: [])

        inner = ActiveRecord::ExplainRegistry.capture("inner") do
          SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select 2 from posts", binds: [])
        end

        SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select 3 from comments", binds: [])
      end

      assert_equal 2, outer.size
      assert_equal ["select 1 from users", "select 3 from comments"], outer.map(&:sql)
      assert_equal ["select 2 from posts"], inner.map(&:sql)
      assert_equal [inner], outer.children
      assert_equal "inner", inner.owner
    end

    def test_capture_releases_itself_when_the_block_raises
      assert_raises(RuntimeError) do
        ActiveRecord::ExplainRegistry.capture("broken") do
          SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select 1 from users", binds: [])
          raise "boom"
        end
      end

      assert_nil ActiveRecord::ExplainRegistry.current_capture
      assert_empty queries

      capture = ActiveRecord::ExplainRegistry.capture("ok") do
        SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select 2 from posts", binds: [])
      end
      assert_equal ["select 2 from posts"], capture.map(&:sql)
    end

    def test_queries_are_attributed_to_the_current_owner
      capture = ActiveRecord::ExplainRegistry.capture("outer") do
        ActiveRecord::ExplainRegistry.with_owner("preload") do
          SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select 1 from users", binds: [])
        end

        SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select 2 from posts", binds: [])
      end

      assert_equal ["preload", "outer"], capture.map(&:owner)
      assert_equal ["select 1 from users"], capture.queries_for("preload").map(&:sql)
    end

    def test_reset_clears_captures
      ActiveRecord::ExplainRegistry.capture("stale") do
        SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select 1 from users", binds: [])
        ActiveRecord::ExplainRegistry.reset
        assert_nil ActiveRecord::ExplainRegistry.current_capture
      end

      assert_empty queries
      assert_not ActiveRecord::ExplainRegistry.collect?
    end

    def test_a_capture_in_another_thread_is_not_reused
      other_capture = nil
      thread = Thread.new do
        other_capture = ActiveRecord::ExplainRegistry.capture("other") do
          SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select 9 from elsewhere", binds: [])
        end
      end
      thread.join

      mine = ActiveRecord::ExplainRegistry.capture("mine") do
        SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select 1 from users", binds: [])
      end

      assert_equal ["other"], other_capture.owners
      assert_equal ["select 9 from elsewhere"], other_capture.map(&:sql)
      assert_equal ["mine"], mine.owners
      assert_equal ["select 1 from users"], mine.map(&:sql)
    end

    def test_collects_nothing_if_the_statement_is_not_explainable
      SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "SHOW max_identifier_length")
      assert_empty queries
    end

    def test_collects_nothing_if_the_statement_is_only_partially_matched
      SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "select_db yo_mama")
      assert_empty queries
    end

    def test_collects_cte_queries
      SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "with s as (values(3)) select 1 from s")
      assert_equal 1, queries.size
    end

    def test_collects_queries_starting_with_comment
      SUBSCRIBER.finish(nil, nil, name: "SQL", sql: "/* comment */ select 1 from users")
      assert_equal 1, queries.size
    end

    teardown do
      ActiveRecord::ExplainRegistry.reset
    end

    def queries
      ActiveRecord::ExplainRegistry.queries
    end
  end
end
