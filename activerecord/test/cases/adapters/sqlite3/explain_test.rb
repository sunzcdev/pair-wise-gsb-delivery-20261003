# frozen_string_literal: true

require "cases/helper"
require "models/author"
require "models/post"

class SQLite3ExplainTest < ActiveRecord::SQLite3TestCase
  fixtures :authors, :author_addresses

  def test_explain_for_one_query
    explain = Author.where(id: 1).explain.inspect
    assert_match %r(EXPLAIN for: SELECT "authors"\.\* FROM "authors" WHERE "authors"\."id" = (?:\? \[\["id", 1\]\]|1)), explain
    assert_match(/(SEARCH )?(TABLE )?authors USING (INTEGER )?PRIMARY KEY/, explain)
  end

  def test_explain_with_eager_loading
    explain = Author.where(id: 1).includes(:posts).explain.inspect
    assert_match %r(EXPLAIN for: SELECT "authors"\.\* FROM "authors" WHERE "authors"\."id" = (?:\? \[\["id", 1\]\]|1)), explain
    assert_match(/(SEARCH )?(TABLE )?authors USING (INTEGER )?PRIMARY KEY/, explain)
    assert_match %r(EXPLAIN for: SELECT "posts"\.\* FROM "posts" WHERE "posts"\."author_id" = (?:\? \[\["author_id", 1\]\]|1)), explain
    assert_match(/(SEARCH |(SCAN )?(TABLE ))posts/, explain)
  end

  def test_explain_with_eager_loading_labels_main_query_and_preload_in_order
    explain = Author.where(id: 1).includes(:posts).explain.inspect

    main_heading = "-- #{ActiveRecord::ExplainRegistry::MAIN_QUERY}"
    preload_heading = "-- #{ActiveRecord::ExplainRegistry::PRELOAD}"

    assert_operator explain.index(main_heading), :<, explain.index(preload_heading)

    main_section, preload_section = explain.split(preload_heading)
    assert_match %r(SELECT "authors"\.\* FROM "authors"), main_section
    assert_no_match %r(SELECT "posts"\.\* FROM "posts"), main_section
    assert_match %r(SELECT "posts"\.\* FROM "posts"), preload_section
    assert_no_match %r(SELECT "authors"\.\* FROM "authors"), preload_section
  end

  def test_explain_inside_another_capture_does_not_mix_plans
    helper_explain = nil
    outer_capture = ActiveRecord::Base.collecting_queries_for_explain do
      helper_explain = Author.where(id: 2).explain.inspect
      Author.where(id: 1).includes(:posts).to_a
    end

    assert_match %r(SELECT "authors"\.\* FROM "authors" WHERE "authors"\."id" = (?:\? \[\["id", 2\]\]|2)), helper_explain
    assert_no_match %r(SELECT "posts"\.\* FROM "posts"), helper_explain

    assert_equal [ActiveRecord::ExplainRegistry::MAIN_QUERY, ActiveRecord::ExplainRegistry::PRELOAD], outer_capture.owners
    assert_equal 2, outer_capture.size
    assert_equal 1, outer_capture.children.size
    assert_match %r(SELECT "authors"\.\* FROM "authors"), outer_capture.queries_for(ActiveRecord::ExplainRegistry::MAIN_QUERY).first.sql
    assert_match %r(SELECT "posts"\.\* FROM "posts"), outer_capture.queries_for(ActiveRecord::ExplainRegistry::PRELOAD).first.sql
  end

  def test_explain_releases_capture_after_a_failed_statement
    assert_raises(ActiveRecord::StatementInvalid) do
      ActiveRecord::Base.collecting_queries_for_explain do
        ActiveRecord::Base.lease_connection.select_all("SELECT * FROM no_such_table", "SQL")
      end
    end

    assert_nil ActiveRecord::ExplainRegistry.current_capture
    assert_not ActiveRecord::ExplainRegistry.collect?

    explain = Author.where(id: 1).explain.inspect
    assert_match %r(EXPLAIN for: SELECT "authors"\.\* FROM "authors"), explain
    assert_match(/(SEARCH )?(TABLE )?authors USING (INTEGER )?PRIMARY KEY/, explain)
  end

  def test_explain_relation_raises_and_releases_its_capture
    assert_raises(ActiveRecord::StatementInvalid) do
      Author.where("definitely_not_a_column = 1").includes(:posts).explain.inspect
    end

    assert_nil ActiveRecord::ExplainRegistry.current_capture
    assert_not ActiveRecord::ExplainRegistry.collect?

    explain = Author.where(id: 1).explain.inspect
    assert_match %r(EXPLAIN for: SELECT "authors"\.\* FROM "authors"), explain
  end

  def test_explain_output_still_works_for_aggregate_proxies
    message = Author.all.explain.count

    assert_match %r(EXPLAIN for: SELECT COUNT\(\*\) FROM "authors"), message
    assert_no_match(/^--/, message)
  end
end
