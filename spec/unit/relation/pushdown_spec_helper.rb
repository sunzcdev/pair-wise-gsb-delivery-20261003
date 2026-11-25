# frozen_string_literal: true

# Standalone helper for the pushdown specs. The project-wide spec_helper
# boots every configured adapter (postgres/mysql), while these specs only
# need SQLite plus local connection doubles, so they load their own minimal
# environment.

require 'rspec'
require 'rom-sql'

RSpec.configure do |config|
  config.disable_monkey_patching!
  config.order = :random
  Kernel.srand config.seed
end

PUSHDOWN_USERS_ROWS = [
  { id: 1, name: 'Jane', age: 30 },
  { id: 2, name: 'Joe', age: 20 },
  { id: 3, name: 'Ann', age: 40 }
].freeze

# Minimal Sequel-ish connection double. Only the surface used by
# ROM::SQL::Gateway and the pushdown planning is implemented.
class PushdownFakeConnection
  attr_reader :label
  attr_reader :database_type
  attr_reader :data

  def initialize(label, supports:, rows: PUSHDOWN_USERS_ROWS)
    @label = label
    @database_type = label
    @supports = supports
    @data = { users: rows.map(&:dup) }
  end

  def supports_window_functions?
    @supports
  end

  # rubocop:disable-next Style/OptionalBooleanParameter -- positional arg required by the Ruby protocol
  def respond_to?(name, include_private = false)
    name.to_s.start_with?('supports_') || super
  end

  def method_missing(name, *args, &)
    return false if name.to_s.start_with?('supports_')

    super
  end

  def respond_to_missing?(name, include_private = false)
    name.to_s.start_with?('supports_') || super
  end

  def [](table)
    PushdownFakeDataset.new(self, table)
  end

  def dataset
    PushdownFakeDataset.new(self, :users)
  end

  def loggers
    []
  end

  def extension(*); end

  def tables
    @data.keys
  end

  def table_exists?(_)
    true
  end

  def ==(other)
    other.is_a?(PushdownFakeConnection) && other.label == label
  end
  alias_method :eql?, :==

  def hash
    label.hash
  end
end

# Dataset double that keeps query clauses in `opts` and evaluates simple
# where/order/limit clauses when materialized, mirroring the shape of a
# Sequel dataset closely enough for relation planning.
class PushdownFakeDataset
  include Enumerable

  attr_reader :db
  attr_reader :opts

  def initialize(db, table, opts = {})
    @db = db
    table = table.first if table.is_a?(Array)
    @opts = { from: [table] }.merge(opts)
  end

  def where(expr)
    PushdownFakeDataset.new(db, table_name, opts.merge(where: expr))
  end

  def order(*cols)
    PushdownFakeDataset.new(db, table_name, opts.merge(order: cols.flatten))
  end

  def limit(n)
    PushdownFakeDataset.new(db, table_name, opts.merge(limit: n))
  end

  def select(*cols)
    PushdownFakeDataset.new(db, table_name, opts.merge(select: cols.flatten))
  end

  def each(&)
    selected_rows.each(&)
  end

  def to_a
    selected_rows
  end
  alias_method :all, :to_a

  def map(...)
    to_a.map(...)
  end

  private

  def table_name
    opts[:from].first
  end

  def selected_rows
    result = db.data[table_name]
    result = apply_where(result) if opts.key?(:where)
    result = apply_order(result) if opts.key?(:order)
    result = result.take(opts[:limit]) if opts.key?(:limit)
    result = apply_select(result) if opts.key?(:select)
    result
  end

  def apply_select(rows)
    columns = opts[:select].map { |column| column_name(column) }
    rows.map { |tuple| tuple.slice(*columns) }
  end

  def apply_where(rows)
    conditions =
      if opts[:where].is_a?(Hash)
        opts[:where]
      else
        expression = opts[:where]
        { expression.args[0] => expression.args[1] }
      end

    rows.select do |tuple|
      conditions.all? { |column, value| tuple[column_name(column)] == value }
    end
  end

  def apply_order(rows)
    opts[:order].reduce(rows) do |memo, clause|
      column = clause.respond_to?(:expression) ? clause.expression : clause
      memo.sort_by { |tuple| tuple[column_name(column)] }
    end
  end

  def column_name(column)
    return column.name if column.respond_to?(:name) && column.name.is_a?(Symbol)
    return column.column if column.respond_to?(:column)

    column
  end
end

module PushdownSpecHelper
  Log = ROM::SQL::Relation::Pushdown::ExecutionLog

  module DSL
    def users_schema
      ROM::SQL::Schema.define(
        ROM::Relation::Name.new(:users),
        attributes: [
          ROM::SQL::Schema.build_attribute_info(ROM::Types::Integer, name: :id),
          ROM::SQL::Schema.build_attribute_info(ROM::Types::String, name: :name),
          ROM::SQL::Schema.build_attribute_info(ROM::Types::Integer, name: :age)
        ],
        attr_class: ROM::SQL::Attribute
      )
    end

    def build_gateway(connection)
      ROM::SQL::Gateway.allocate.tap do |gateway|
        gateway.instance_variable_set(:@connection, connection)
        gateway.send(:bind_connection, connection)
      end
    end

    def build_relation(connection)
      build_gateway(connection)
      ROM::SQL::Relation.new(
        connection[:users],
        schema: users_schema,
        name: ROM::Relation::Name.new(:users)
      )
    end

    def sqlite_container
      database = Sequel.sqlite
      database.create_table(:users) do
        primary_key :id
        String :name
        Integer :age
      end
      database[:users].multi_insert(PUSHDOWN_USERS_ROWS)

      configuration = ROM::Configuration.new(:sql, database)
      configuration.relation(:users) { schema(infer: true) }
      [ROM.container(configuration), database]
    end
  end
end

RSpec.configure do |config|
  config.include PushdownSpecHelper::DSL
  config.before { ROM::SQL::Relation::Pushdown::ExecutionLog.clear }
end
