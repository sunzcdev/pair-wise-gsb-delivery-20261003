# frozen_string_literal: true

require "cases/helper"
require "support/schema_dumping_helper"

if ActiveRecord::Base.lease_connection.supports_virtual_columns?
  class SQLite3VirtualColumnTest < ActiveRecord::SQLite3TestCase
    skip_under_ractor_proxy

    include SchemaDumpingHelper

    class VirtualColumn < ActiveRecord::Base
    end

    def setup
      @connection = ActiveRecord::Base.lease_connection
      @connection.create_table :virtual_columns, force: true do |t|
        t.string  :name
        t.virtual :upper_name, type: :string, as: "UPPER(name)", stored: true
        t.virtual :lower_name, type: :string, as: "LOWER(name)", stored: false
        t.virtual :octet_name, type: :integer, as: "LENGTH(name)"
        t.virtual :mutated_name, type: :string, as: "REPLACE(name, 'l', 'L')"
        t.integer :column1
      end
      VirtualColumn.create(name: "Rails", column1: 10)
    end

    def teardown
      @connection.drop_table :virtual_columns, if_exists: true
      VirtualColumn.reset_column_information
    end

    def test_virtual_column_with_full_inserts
      partial_inserts_was = VirtualColumn.partial_inserts
      VirtualColumn.partial_inserts = false
      assert_nothing_raised do
        VirtualColumn.create!(name: "Rails")
      end
    ensure
      VirtualColumn.partial_inserts = partial_inserts_was
    end

    def test_stored_column
      column = VirtualColumn.columns_hash["upper_name"]
      assert_predicate column, :virtual?
      assert_predicate column, :virtual_stored?
      assert_equal "RAILS", VirtualColumn.take.upper_name
    end

    def test_explicit_virtual_column
      column = VirtualColumn.columns_hash["lower_name"]
      assert_predicate column, :virtual?
      assert_not_predicate column, :virtual_stored?
      assert_equal "rails", VirtualColumn.take.lower_name
    end

    def test_implicit_virtual_column
      column = VirtualColumn.columns_hash["octet_name"]
      assert_predicate column, :virtual?
      assert_not_predicate column, :virtual_stored?
      assert_equal 5, VirtualColumn.take.octet_name
    end

    def test_virtual_column_with_comma_in_definition
      column = VirtualColumn.columns_hash["mutated_name"]
      assert_predicate column, :virtual?
      assert_not_predicate column, :virtual_stored?
      assert_not_nil column.default_function
      assert_equal "RaiLs", VirtualColumn.take.mutated_name
    end

    def test_change_table_with_stored_generated_column
      @connection.change_table :virtual_columns do |t|
        t.virtual :decr_column1, type: :integer, as: "column1 - 1", stored: true
      end
      VirtualColumn.reset_column_information
      column = VirtualColumn.columns_hash["decr_column1"]
      assert_predicate column, :virtual?
      assert_predicate column, :virtual_stored?
      assert_equal 9, VirtualColumn.take.decr_column1
    end

    def test_change_table_with_explicit_virtual_generated_column
      @connection.change_table :virtual_columns do |t|
        t.virtual :incr_column1, type: :integer, as: "column1 + 1", stored: false
      end
      VirtualColumn.reset_column_information
      column = VirtualColumn.columns_hash["incr_column1"]
      assert_predicate column, :virtual?
      assert_not_predicate column, :virtual_stored?
      assert_equal 11, VirtualColumn.take.incr_column1
    end

    def test_change_table_with_implicit_virtual_generated_column
      @connection.change_table :virtual_columns do |t|
        t.virtual :sqr_column1, type: :integer, as: "pow(column1, 2)"
      end
      VirtualColumn.reset_column_information
      column = VirtualColumn.columns_hash["sqr_column1"]
      assert_predicate column, :virtual?
      assert_not_predicate column, :virtual_stored?
      assert_equal 100, VirtualColumn.take.sqr_column1
    end

    def test_schema_dumping
      output = dump_table_schema("virtual_columns")
      assert_match(/t\.virtual\s+"upper_name",\s+type: :string,\s+as: "UPPER\(name\)", stored: true$/i, output)
    end

    def test_schema_dumping_keeps_virtual_columns_created_with_shorthand_syntax
      # SQLite accepts generated columns declared without the GENERATED ALWAYS
      # keywords and without quoted identifiers, as other tools may emit them.
      @connection.execute <<~SQL
        CREATE TABLE shorthand_virtual_columns (
          id integer PRIMARY KEY,
          name varchar,
          upper_name varchar AS (UPPER(name)) STORED,
          lower_name varchar AS (LOWER(name)),
          name_len integer AS (LENGTH(name))
        )
      SQL

      columns = @connection.columns("shorthand_virtual_columns").index_by(&:name)

      upper_name = columns.fetch("upper_name")
      assert_predicate upper_name, :virtual?
      assert_predicate upper_name, :virtual_stored?
      assert_equal "UPPER(name)", upper_name.default_function

      lower_name = columns.fetch("lower_name")
      assert_predicate lower_name, :virtual?
      assert_not_predicate lower_name, :virtual_stored?
      assert_equal "LOWER(name)", lower_name.default_function

      name_len = columns.fetch("name_len")
      assert_predicate name_len, :virtual?
      assert_not_predicate name_len, :virtual_stored?
      assert_equal "LENGTH(name)", name_len.default_function
    ensure
      @connection.drop_table :shorthand_virtual_columns, if_exists: true
    end

    def test_schema_dumping_keeps_expressions_with_commas_and_keywords
      # The expression contains commas and the STORED keyword inside a string
      # literal, which naive regular expression based reflection used to lose.
      @connection.execute <<~SQL
        CREATE TABLE keyword_virtual_columns (
          id integer PRIMARY KEY,
          name varchar,
          mutated_name varchar GENERATED ALWAYS AS (REPLACE(name, 'stored', 'x')) VIRTUAL,
          nested integer AS (CASE WHEN (LENGTH(name) > 2) THEN round(LENGTH(name) * 1.5, 2) ELSE 0 END) STORED
        )
      SQL

      columns = @connection.columns("keyword_virtual_columns").index_by(&:name)

      mutated_name = columns.fetch("mutated_name")
      assert_predicate mutated_name, :virtual?
      assert_not_predicate mutated_name, :virtual_stored?
      assert_equal "REPLACE(name, 'stored', 'x')", mutated_name.default_function

      nested = columns.fetch("nested")
      assert_predicate nested, :virtual?
      assert_predicate nested, :virtual_stored?
      assert_equal "CASE WHEN (LENGTH(name) > 2) THEN round(LENGTH(name) * 1.5, 2) ELSE 0 END", nested.default_function
    ensure
      @connection.drop_table :keyword_virtual_columns, if_exists: true
    end

    def test_reflection_keeps_unquoted_collation_and_autoincrement_clauses
      # Foreign tools may emit identifiers and collation names without quotes,
      # which the reflection used not to recognize alongside generated columns.
      @connection.execute <<~SQL
        CREATE TABLE unquoted_clause_columns (
          id integer PRIMARY KEY AUTOINCREMENT,
          name varchar COLLATE NOCASE,
          upper_name varchar AS (UPPER(name)) STORED
        )
      SQL

      columns = @connection.columns("unquoted_clause_columns").index_by(&:name)
      assert_predicate columns.fetch("id"), :auto_increment?
      assert_equal "NOCASE", columns.fetch("name").collation

      upper_name = columns.fetch("upper_name")
      assert_predicate upper_name, :virtual_stored?
      assert_equal "UPPER(name)", upper_name.default_function
    ensure
      @connection.drop_table :unquoted_clause_columns, if_exists: true
    end

    def test_schema_dump_round_trips_generated_columns_and_expression_indexes
      @connection.execute <<~SQL
        CREATE TABLE round_trip_widgets (
          id integer PRIMARY KEY AUTOINCREMENT,
          name varchar NOT NULL,
          upper_name varchar GENERATED ALWAYS AS (UPPER(name)) STORED,
          lower_name varchar AS (LOWER(name)),
          name_len integer AS (LENGTH(name))
        )
      SQL
      @connection.execute "CREATE UNIQUE INDEX index_round_trip_widgets_on_upper_name ON round_trip_widgets (upper_name)"
      @connection.execute "CREATE INDEX index_round_trip_widgets_on_expression ON round_trip_widgets (LOWER(name), name_len)"

      output = dump_table_schema("round_trip_widgets")
      assert_match(/t\.virtual\s+"upper_name",\s+type: :string,\s+as: "UPPER\(name\)", stored: true/, output)
      assert_match(/t\.virtual\s+"lower_name",\s+type: :string,\s+as: "LOWER\(name\)", stored: false/, output)
      assert_match(/t\.virtual\s+"name_len",\s+type: :integer,\s+as: "LENGTH\(name\)", stored: false/, output)
      assert_match(/t\.index\s+"LOWER\(name\), name_len", name: "index_round_trip_widgets_on_expression"/, output)
      assert_match(/t\.index\s+\["upper_name"\], name: "index_round_trip_widgets_on_upper_name", unique: true/, output)

      # Reload the dumped definitions into a fresh table and make sure the
      # generated values and index constraints behave identically.
      @connection.create_table :reloaded_widgets, force: :cascade do |t|
        t.string :name, null: false
        t.virtual :upper_name, type: :string, as: "UPPER(name)", stored: true
        t.virtual :lower_name, type: :string, as: "LOWER(name)", stored: false
        t.virtual :name_len, type: :integer, as: "LENGTH(name)", stored: false
        t.index "LOWER(name), name_len", name: "index_reloaded_widgets_on_expression"
        t.index ["upper_name"], name: "index_reloaded_widgets_on_upper_name", unique: true
      end

      @connection.execute "INSERT INTO reloaded_widgets (name) VALUES ('Rails')"
      row = @connection.execute("SELECT upper_name, lower_name, name_len FROM reloaded_widgets").first
      assert_equal({ "upper_name" => "RAILS", "lower_name" => "rails", "name_len" => 5 }, row)

      error = assert_raises(ActiveRecord::RecordNotUnique) do
        @connection.execute "INSERT INTO reloaded_widgets (name) VALUES ('RAILS')"
      end
      assert_match(/UNIQUE constraint failed/i, error.message)
    ensure
      @connection.drop_table :round_trip_widgets, if_exists: true
      @connection.drop_table :reloaded_widgets, if_exists: true
    end

    def test_supports_virtual_columns_reflects_sqlite_version
      if @connection.database_version >= "3.31.0"
        assert_predicate @connection, :supports_virtual_columns?
      else
        assert_not_predicate @connection, :supports_virtual_columns?
      end
    end

    def test_creating_a_generated_column_on_unsupported_sqlite_raises
      @connection.stub(:supports_virtual_columns?, false) do
        error = assert_raises(ActiveRecord::DatabaseVersionError) do
          @connection.create_table :unsupported_virtual_columns, force: true do |t|
            t.string :name
            t.virtual :upper_name, type: :string, as: "UPPER(name)"
          end
        end
        assert_match(/Generated columns require SQLite 3\.31\.0/, error.message)
      end
    ensure
      @connection.drop_table :unsupported_virtual_columns, if_exists: true
    end

    def test_expression_index_on_unsupported_sqlite_raises
      @connection.stub(:supports_expression_index?, false) do
        @connection.create_table :plain_widgets, force: true do |t|
          t.string :name
        end

        error = assert_raises(ActiveRecord::DatabaseVersionError) do
          @connection.add_index :plain_widgets, "LOWER(name)", name: "index_plain_widgets_on_lower_name"
        end
        assert_match(/Expression indexes require SQLite 3\.9\.0/, error.message)
      end
    ensure
      @connection.drop_table :plain_widgets, if_exists: true
    end

    def test_plain_index_still_works_when_expression_indexes_are_unsupported
      @connection.stub(:supports_expression_index?, false) do
        @connection.create_table :indexed_widgets, force: true do |t|
          t.string :name
          t.index :name, name: "index_indexed_widgets_on_name"
        end

        assert_includes @connection.indexes("indexed_widgets").map(&:name), "index_indexed_widgets_on_name"
      end
    ensure
      @connection.drop_table :indexed_widgets, if_exists: true
    end

    def test_build_fixture_sql
      fixtures = ActiveRecord::FixtureSet.create_fixtures(FIXTURES_ROOT, :virtual_columns).first
      assert_equal 2, fixtures.size
    end

    def test_virtual_column_does_not_deduplicate_with_regular_column
      @connection.create_table :regular_columns, force: true do |t|
        t.string :name
      end

      regular_model = Class.new(ActiveRecord::Base) { self.table_name = "regular_columns" }

      # Ensure deduplication has seen the virtual column first
      VirtualColumn.columns_hash

      regular_col = regular_model.columns_hash["name"]
      virtual_col = VirtualColumn.columns_hash["upper_name"]

      assert_predicate virtual_col, :virtual?
      assert_not_predicate regular_col, :virtual?
      assert_not_equal regular_col, virtual_col

      # The regular column must not be excluded from inserts
      record = regular_model.create!(name: "hello")
      record.reload
      assert_equal "hello", record.name
    ensure
      @connection.drop_table :regular_columns, if_exists: true
    end
  end
end
