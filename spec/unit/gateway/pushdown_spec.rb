# frozen_string_literal: true

require_relative '../relation/pushdown_spec_helper'

RSpec.describe ROM::SQL::Gateway, '#supports_sql_feature?', :sqlite do
  it 'delegates Sequel feature probes to the real connection' do
    database = Sequel.sqlite
    gateway = ROM::SQL::Gateway.new(database)

    expect(gateway.supports_sql_feature?(:cte)).to be(true)
    expect(gateway.supports_sql_feature?(:window_functions)).to be(true)
  end

  it 'answers false for features the connection does not expose' do
    database = Sequel.sqlite
    gateway = ROM::SQL::Gateway.new(database)

    expect(gateway.supports_sql_feature?(:made_up_capability)).to be(false)
  end

  it 'keeps capability answers independent across two gateways' do
    first_database = Sequel.sqlite
    second_database = Sequel.sqlite
    first_gateway = ROM::SQL::Gateway.new(first_database)
    second_gateway = ROM::SQL::Gateway.new(second_database)

    second_database.define_singleton_method(:supports_bespoke?) { true }

    expect(first_gateway.supports_sql_feature?(:bespoke)).to be(false)
    expect(second_gateway.supports_sql_feature?(:bespoke)).to be(true)
  end

  it 'marks its connection so a relation can resolve the owning gateway' do
    database = Sequel.sqlite
    gateway = ROM::SQL::Gateway.new(database)

    expect(database.rom_sql_gateway).to be(gateway)
  end

  it 'works with connection doubles that only implement the predicate' do
    connection = Class.new do
      def database_type = :double
      def supports_special_thing? = true
      def loggers = []
      def extension(*); end
    end.new

    gateway = ROM::SQL::Gateway.allocate.tap do |gw|
      gw.instance_variable_set(:@connection, connection)
      gw.send(:bind_connection, connection)
    end

    expect(gateway.supports_sql_feature?(:special_thing)).to be(true)
    expect(gateway.supports_sql_feature?(:other_thing)).to be(false)
  end
end
