# frozen_string_literal: true

require "cases/helper"

module ActiveRecord
  module ConnectionAdapters
    class StatementPoolTest < ActiveRecord::TestCase
      class TestPool < StatementPool
        private
          def dealloc(stmt)
            raise ArgumentError unless stmt
          end
      end

      setup do
        @pool = TestPool.new
      end

      test "#delete doesn't call dealloc if the statement didn't exist" do
        stmt = Object.new
        sql = "SELECT 1"
        @pool[sql] = stmt
        assert_same stmt, @pool[sql]
        assert_same stmt, @pool.delete(sql)
        assert_nil @pool.delete(sql)
      end

      test "#store keeps metadata alongside the statement" do
        stmt = Object.new
        metadata = { tables: ["accounts"] }

        @pool.store("SELECT 1", stmt, metadata)

        assert_same stmt, @pool["SELECT 1"]
        assert_equal metadata, @pool.metadata("SELECT 1")
      end

      test "#metadata is nil for a statement cached without it" do
        @pool["SELECT 1"] = Object.new

        assert_nil @pool.metadata("SELECT 1")
      end

      test "#delete also drops the metadata" do
        @pool.store("SELECT 1", Object.new, { tables: ["accounts"] })

        @pool.delete("SELECT 1")

        assert_nil @pool.metadata("SELECT 1")
      end

      test "#clear also drops the metadata" do
        @pool.store("SELECT 1", Object.new, { tables: ["accounts"] })

        @pool.clear

        assert_nil @pool.metadata("SELECT 1")
      end

      test "#reset also drops the metadata" do
        @pool.store("SELECT 1", Object.new, { tables: ["accounts"] })

        @pool.reset

        assert_nil @pool.metadata("SELECT 1")
      end

      test "evicting a statement drops its metadata" do
        pool = TestPool.new(1)
        pool.store("SELECT 1", Object.new, { tables: ["accounts"] })
        pool.store("SELECT 2", Object.new, { tables: ["comments"] })

        assert_nil pool.metadata("SELECT 1")
        assert_equal({ tables: ["comments"] }, pool.metadata("SELECT 2"))
      end
    end
  end
end
