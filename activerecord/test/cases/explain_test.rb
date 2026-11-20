# frozen_string_literal: true

require "cases/helper"
require "models/car"
require "models/bulb"

if ActiveRecord::Base.lease_connection.supports_explain?
  class ExplainTest < ActiveRecord::TestCase
    fixtures :cars, :bulbs

    def base
      ActiveRecord::Base
    end

    def lease_connection
      base.lease_connection
    end

    def test_relation_explain
      message = Car.where(name: "honda").explain.inspect
      assert_match(/^EXPLAIN/, message)
    end

    def test_collecting_queries_for_explain
      capture = ActiveRecord::Base.collecting_queries_for_explain do
        Car.where(name: "honda").to_a
      end

      sql, binds = capture[0]
      assert_match "SELECT", sql
      if binds.any?
        assert_equal 1, binds.length
        assert_equal "honda", binds.last.value
      else
        assert_match "honda", sql
      end
    end

    def test_collecting_queries_for_explain_attributes_main_query_and_preload
      capture = ActiveRecord::Base.collecting_queries_for_explain do
        Car.where(name: "honda").includes(:bulbs).to_a
      end

      assert_equal [ActiveRecord::ExplainRegistry::MAIN_QUERY, ActiveRecord::ExplainRegistry::PRELOAD], capture.owners
      assert_equal 2, capture.size
      assert_match "cars", capture.queries_for(ActiveRecord::ExplainRegistry::MAIN_QUERY).first.sql
      assert_match "bulbs", capture.queries_for(ActiveRecord::ExplainRegistry::PRELOAD).first.sql
    end

    def test_nested_capture_does_not_absorb_the_enclosing_capture
      helper_capture = nil
      outer_capture = ActiveRecord::Base.collecting_queries_for_explain do
        helper_capture = ActiveRecord::Base.collecting_queries_for_explain("helper") do
          Car.where(name: "zyke").to_a
        end

        Car.where(name: "honda").to_a
      end

      assert_equal 1, helper_capture.size
      assert_equal "zyke", helper_capture[0].binds.last.value
      assert_equal 1, outer_capture.size
      assert_equal "honda", outer_capture[0].binds.last.value
      assert_equal [helper_capture], outer_capture.children
      assert_equal outer_capture, helper_capture.parent
      assert_nil ActiveRecord::ExplainRegistry.current_capture
    end

    def test_explain_inside_a_capture_stays_in_its_own_capture
      outer_capture = ActiveRecord::Base.collecting_queries_for_explain do
        message = Car.where(name: "zyke").explain.inspect
        assert_match(/^EXPLAIN/, message)
        assert_no_match(/honda/, message)

        Car.where(name: "honda").to_a
      end

      assert_equal 1, outer_capture.size
      assert_equal "honda", outer_capture[0].binds.last.value
      assert_equal 1, outer_capture.children.size
      assert_equal "zyke", outer_capture.children.first[0].binds.last.value
    end

    def test_explain_inside_a_capture_renders_only_its_own_plans
      inner_message = nil
      ActiveRecord::Base.collecting_queries_for_explain do
        inner_message = Car.where(name: "zyke").includes(:bulbs).explain.inspect
      end

      assert_match(/^EXPLAIN/, inner_message)
      assert_no_match(/honda/, inner_message)
      assert_match(/zyke/, inner_message)
    end

    def test_capture_is_released_when_the_query_raises
      assert_raises(ActiveRecord::StatementInvalid) do
        ActiveRecord::Base.collecting_queries_for_explain("broken") do
          ActiveRecord::Base.lease_connection.select_all("SELECT * FROM missing_table_for_explain", "SQL")
        end
      end

      assert_nil ActiveRecord::ExplainRegistry.current_capture
      assert_not ActiveRecord::ExplainRegistry.collect?

      capture = ActiveRecord::Base.collecting_queries_for_explain do
        Car.where(name: "honda").to_a
      end
      assert_equal 1, capture.size
      assert_equal [ActiveRecord::ExplainRegistry::MAIN_QUERY], capture.owners
    end

    def test_explain_output_labels_each_owner
      message = Car.where(name: "honda").includes(:bulbs).explain.inspect

      assert_match(/^-- #{Regexp.escape(ActiveRecord::ExplainRegistry::MAIN_QUERY)}$/, message)
      assert_match(/^-- #{Regexp.escape(ActiveRecord::ExplainRegistry::PRELOAD)}$/, message)
      assert_operator message.index("-- #{ActiveRecord::ExplainRegistry::MAIN_QUERY}"), :<,
        message.index("-- #{ActiveRecord::ExplainRegistry::PRELOAD}")
    end

    def test_explain_output_keeps_plain_form_for_a_single_owner
      message = Car.where(name: "honda").explain.inspect

      assert_match(/\AEXPLAIN/, message)
      assert_no_match(/^--/, message)
    end

    def test_relation_explain_with_average
      expected_query = capture_sql {
        Car.average(:id)
      }.first
      message = Car.all.explain.average(:id)
      assert_match(/^EXPLAIN/, message)
      assert_match(expected_query, message)
    end

    def test_relation_explain_with_count
      expected_query = capture_sql {
        Car.count
      }.first
      message = Car.all.explain.count
      assert_match(/^EXPLAIN/, message)
      assert_match(expected_query, message)
    end

    def test_relation_explain_with_count_and_argument
      expected_query = capture_sql {
        Car.count(:id)
      }.first
      message = Car.all.explain.count(:id)
      assert_match(/^EXPLAIN/, message)
      assert_match(expected_query, message)
    end

    def test_relation_explain_with_minimum
      expected_query = capture_sql {
        Car.minimum(:id)
      }.first
      message = Car.all.explain.minimum(:id)
      assert_match(/^EXPLAIN/, message)
      assert_match(expected_query, message)
    end

    def test_relation_explain_with_maximum
      expected_query = capture_sql {
        Car.maximum(:id)
      }.first
      message = Car.all.explain.maximum(:id)
      assert_match(/^EXPLAIN/, message)
      assert_match(expected_query, message)
    end

    def test_relation_explain_with_sum
      expected_query = capture_sql {
        Car.sum(:id)
      }.first
      message = Car.all.explain.sum(:id)
      assert_match(/^EXPLAIN/, message)
      assert_match(expected_query, message)
    end

    def test_relation_explain_with_first
      expected_query = capture_sql {
        Car.all.first
      }.first
      message = Car.all.explain.first
      assert_match(/^EXPLAIN/, message)
      assert_match(expected_query.sub(/LIMIT.*/, ""), message)
    end

    def test_relation_explain_with_last
      expected_query = capture_sql {
        Car.all.last
      }.first
      message = Car.all.explain.last
      assert_match(/^EXPLAIN/, message)
      assert_match(expected_query.sub(/LIMIT.*/, ""), message)
    end

    def test_relation_explain_with_pluck
      expected_query = capture_sql {
        Car.all.pluck
      }.first
      message = Car.all.explain.pluck
      assert_match(/^EXPLAIN/, message)
      assert_match(expected_query, message)
    end

    def test_relation_explain_with_pluck_with_args
      expected_query = capture_sql {
        Car.all.pluck(:id, :name)
      }.first
      message = Car.all.explain.pluck(:id, :name)
      assert_match(/^EXPLAIN/, message)
      assert_match(expected_query, message)
    end

    def test_exec_explain_with_no_binds
      sqls    = %w(foo bar)
      binds   = [[], []]
      queries = sqls.zip(binds)

      stub_explain_for_query_plans do
        expected = sqls.map { |sql| "#{expected_explain_clause} #{sql}\nquery plan #{sql}" }.join("\n")
        assert_equal expected, base.exec_explain(queries)
      end
    end

    def test_exec_explain_with_binds
      sqls    = %w(foo bar)
      binds   = [[bind_param("wadus", 1)], [bind_param("chaflan", 2)]]
      queries = sqls.zip(binds)

      stub_explain_for_query_plans(["query plan foo\n", "query plan bar\n"]) do
        expected = <<~SQL
          #{expected_explain_clause} #{sqls[0]} [["wadus", 1]]
          query plan foo

          #{expected_explain_clause} #{sqls[1]} [["chaflan", 2]]
          query plan bar
        SQL
        assert_equal expected, base.exec_explain(queries)
      end
    end

    def test_explain_with_arel
      message = lease_connection.explain(Car.where(name: "honda").arel)
      assert_not_empty message
      assert_match(/cars/i, message)
    end

    def test_exec_explain_with_casted_binds
      sqls    = ["foo", "bar"]
      binds   = [[1, "abcd"], [2]]
      queries = sqls.zip(binds)

      stub_explain_for_query_plans(["query plan foo\n", "query plan bar\n"]) do
        expected = <<~SQL
          #{expected_explain_clause} #{sqls[0]} [["$1", 1], ["$2", "abcd"]]
          query plan foo

          #{expected_explain_clause} #{sqls[1]} [["$1", 2]]
          query plan bar
        SQL
        assert_equal expected, base.exec_explain(queries)
      end
    end

    private
      def stub_explain_for_query_plans(query_plans = ["query plan foo", "query plan bar"])
        explain_called = 0

        # Minitest's `stub` method is unable to correctly replicate method arguments
        # signature, so we need to do a manual stubbing in this case.
        metaclass = lease_connection.singleton_class
        explain_method = metaclass.instance_method(:explain)
        metaclass.define_method(:explain) do |_arel, _binds = [], _options = {}|
          explain_called += 1
          query_plans[explain_called - 1]
        end
        yield
      ensure
        if metaclass
          metaclass.undef_method(:explain)
          metaclass.define_method(:explain, explain_method)
        end
      end

      def bind_param(name, value)
        ActiveRecord::Relation::QueryAttribute.new(name, value, ActiveRecord::Type::Value.new)
      end

      def expected_explain_clause
        if lease_connection.respond_to?(:build_explain_clause)
          lease_connection.build_explain_clause
        else
          "EXPLAIN for:"
        end
      end
  end
end
