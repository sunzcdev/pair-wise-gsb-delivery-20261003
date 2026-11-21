# frozen_string_literal: true

require "cases/helper"
require "tmpdir"
require "fileutils"

class SQLite3StatementDependencyTest < ActiveRecord::SQLite3TestCase
  self.use_transactional_tests = false

  setup do
    @connection = ActiveRecord::ConnectionAdapters::SQLite3Adapter.new(
      database: ":memory:",
      adapter: "sqlite3",
      timeout: 100,
    )
    @connection.connect!

    @connection.create_table(:parents) do |t|
      t.string :name
    end
    @connection.create_table(:children) do |t|
      t.string :label
      t.integer :parent_id
    end

    @connection.execute("INSERT INTO parents (name) VALUES ('p1')")
    @connection.execute("INSERT INTO children (label, parent_id) VALUES ('c1', 1)")

    watch_rebuilds
  end

  teardown do
    ActiveSupport::Notifications.unsubscribe(@subscriber)
    @connection.disconnect!
  end

  test "only statements that read the changed table are rebuilt" do
    parents_sql = "SELECT * FROM parents WHERE id = ?"
    children_sql = "SELECT * FROM children WHERE id = ?"

    select(parents_sql, 1)
    select(children_sql, 1)

    @connection.add_column(:children, :note, :string, default: "n")

    # The statement on the untouched table is reused as is. The changed table's
    # statement is rebuilt lazily, the next time it runs, not eagerly by the DDL.
    select(parents_sql, 1)
    assert_not_includes @rebuilt_sql, parents_sql

    select(children_sql, 1)
    assert_equal [children_sql], @rebuilt_sql
  end

  test "the rebuilt statement returns the new column and keeps reusing afterwards" do
    children_sql = "SELECT * FROM children WHERE id = ?"
    select(children_sql, 1)

    @connection.add_column(:children, :note, :string, default: "n")

    @rebuilt_sql.clear
    row = select(children_sql, 1).first
    assert_equal({ "id" => 1, "label" => "c1", "parent_id" => 1, "note" => "n" }, row)
    assert_equal [children_sql], @rebuilt_sql

    # Once rebuilt, the statement is cached again and reused.
    @rebuilt_sql.clear
    select(children_sql, 1)
    assert_empty @rebuilt_sql
  end

  test "statements reading several tables survive changes to only one of them" do
    join_sql = "SELECT children.id FROM children JOIN parents ON parents.id = children.parent_id"
    select(join_sql)

    @connection.create_table(:unrelated) { |t| t.string :name }

    select(join_sql)
    assert_empty @rebuilt_sql

    @connection.add_column(:parents, :nickname, :string)

    select(join_sql)
    assert_equal [join_sql], @rebuilt_sql
  end

  test "a rolled back transaction leaves the pool consistent for the reverted schema" do
    children_sql = "SELECT * FROM children WHERE id = ?"
    select(children_sql, 1)

    @connection.transaction do
      @connection.add_column(:children, :temporary, :string, default: "t")

      row = select(children_sql, 1).first
      assert_includes row.keys, "temporary"

      raise ActiveRecord::Rollback
    end

    @rebuilt_sql.clear
    row = select(children_sql, 1).first
    assert_not_includes row.keys, "temporary"
    assert_equal [children_sql], @rebuilt_sql

    @rebuilt_sql.clear
    select(children_sql, 1)
    assert_empty @rebuilt_sql
  end

  test "a committed transactional column addition rebuilds the statement once" do
    children_sql = "SELECT * FROM children WHERE id = ?"
    select(children_sql, 1)

    @connection.transaction do
      @connection.add_column(:children, :kept, :string, default: "k")
    end

    @rebuilt_sql.clear
    row = select(children_sql, 1).first
    assert_equal "k", row["kept"]
    assert_equal [children_sql], @rebuilt_sql

    @rebuilt_sql.clear
    select(children_sql, 1)
    assert_empty @rebuilt_sql
  end

  test "statements derived from objects SQLite cannot attribute are rebuilt conservatively" do
    pragma_sql = "SELECT count(*) FROM pragma_table_info('children')"
    catalog_sql = "SELECT count(*) FROM sqlite_master"

    select(pragma_sql)
    select(catalog_sql)

    @connection.create_table(:another) { |t| t.string :name }

    select(pragma_sql)
    select(catalog_sql)

    assert_includes @rebuilt_sql, pragma_sql
    assert_includes @rebuilt_sql, catalog_sql
  end

  test "a statement with no table dependency survives schema changes" do
    constant_sql = "SELECT 1 AS one"
    select(constant_sql)

    @connection.create_table(:another) { |t| t.string :name }

    select(constant_sql)
    assert_empty @rebuilt_sql
  end

  private
    def select(sql, *binds)
      @connection.select_all(sql, "SQL", binds.map { |bind| Arel.sql(bind.to_s) }, preparable: true)
    end

    def watch_rebuilds
      @rebuilt_sql = []
      @subscriber = ActiveSupport::Notifications.subscribe("sql.active_record") do |*args|
        payload = args.last
        @rebuilt_sql << payload[:sql] if payload[:statement_rebuilt]
      end
    end
end

class SQLite3StatementDependencyCrossConnectionTest < ActiveRecord::SQLite3TestCase
  self.use_transactional_tests = false

  setup do
    @directory = Dir.mktmpdir("ar-statement-dependency")
    @path = File.join(@directory, "test.sqlite3")
    @reader = new_connection(@path)
    @writer = new_connection(@path)

    @reader.create_table(:accounts) { |t| t.string :name }
    @reader.create_table(:tickets) { |t| t.string :subject }
    @reader.execute("INSERT INTO accounts (name) VALUES ('a1')")
    @reader.execute("INSERT INTO tickets (subject) VALUES ('t1')")

    watch_rebuilds
  end

  teardown do
    ActiveSupport::Notifications.unsubscribe(@subscriber)
    @reader.disconnect!
    @writer.disconnect!
    FileUtils.remove_entry(@directory)
  end

  test "a schema change committed by another connection rebuilds only that table's statement" do
    accounts_sql = "SELECT * FROM accounts WHERE id = ?"
    tickets_sql = "SELECT * FROM tickets WHERE id = ?"

    select(@reader, accounts_sql)
    select(@reader, tickets_sql)

    @writer.add_column(:tickets, :note, :string, default: "n")

    assert_equal({ "id" => 1, "name" => "a1" }, select(@reader, accounts_sql).first)
    assert_not_includes @rebuilt_sql, accounts_sql

    assert_equal "n", select(@reader, tickets_sql).first["note"]
    assert_equal [tickets_sql], @rebuilt_sql
  end

  test "a schema change rolled back by another connection is never visible, so nothing is rebuilt" do
    tickets_sql = "SELECT * FROM tickets WHERE id = ?"
    select(@reader, tickets_sql)

    @writer.transaction do
      @writer.add_column(:tickets, :temporary, :string, default: "t")
      raise ActiveRecord::Rollback
    end

    @rebuilt_sql.clear
    ticket = select(@reader, tickets_sql).first
    assert_not_includes ticket.keys, "temporary"
    assert_empty @rebuilt_sql
  end

  private
    def new_connection(database)
      ActiveRecord::ConnectionAdapters::SQLite3Adapter.new(
        database: database, adapter: "sqlite3", timeout: 100
      ).tap(&:connect!)
    end

    def select(connection, sql)
      connection.select_all(sql, "SQL", [Arel.sql("1")], preparable: true)
    end

    def watch_rebuilds
      @rebuilt_sql = []
      @subscriber = ActiveSupport::Notifications.subscribe("sql.active_record") do |*args|
        payload = args.last
        @rebuilt_sql << payload[:sql] if payload[:statement_rebuilt]
      end
    end
end
