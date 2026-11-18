# frozen_string_literal: true
require_relative "spec_helper"

describe "row value condition compatibility compilation" do
  before do
    @d = Sequel.mock.dataset.with_extend do
      def supports_multiple_column_in?; false end
      def supports_row_value?; false end
    end
    # A backend with native row value support, for asserting the syntax is
    # left untouched when emulation is not needed.
    @n = Sequel.mock.dataset
  end

  def row_expr(op, lhs, rhs)
    Sequel::SQL::BooleanExpression.new(op, lhs, rhs)
  end

  it "should keep native row value syntax on backends that support it" do
    @n.literal(row_expr(:>, [:x, :y], [1, 2])).must_equal "((x, y) > (1, 2))"
    @n.literal(row_expr(:"=", [:x, :y], [1, nil])).must_equal "((x, y) = (1, NULL))"
    @n.literal(row_expr(:IN, [:x, :y], Sequel.value_list([[1, 2]]))).
      must_equal "((x, y) IN ((1, 2)))"
  end

  it "should emulate row value equality with AND-ed scalar equality" do
    @d.literal(row_expr(:"=", [:x, :y], [1, 2])).must_equal "((x = 1) AND (y = 2))"
    @d.literal(row_expr(:"=", [:x, :y, :z], [1, 2, 3])).
      must_equal "((x = 1) AND (y = 2) AND (z = 3))"
  end

  it "should keep NULL equality unknown in row value equality instead of matching NULL" do
    # (x, y) = (1, NULL) must use plain = NULL, which stays unknown, rather
    # than IS NULL, which would incorrectly match rows where y IS NULL.
    @d.literal(row_expr(:"=", [:x, :y], [1, nil])).must_equal "((x = 1) AND (y = NULL))"
  end

  it "should emulate row value inequality as the negation of emulated equality" do
    @d.literal(row_expr(:"!=", [:x, :y], [1, 2])).must_equal "((x != 1) OR (y != 2))"
    # A NULL operand keeps the negated comparison unknown rather than true.
    @d.literal(row_expr(:"!=", [:x, :y], [1, nil])).must_equal "((x != 1) OR (y != NULL))"
  end

  it "should emulate lexicographic row value ordering with prefix expansion" do
    @d.literal(row_expr(:<, [:x, :y], [1, 2])).
      must_equal "((x < 1) OR ((x = 1) AND (y < 2)))"
    @d.literal(row_expr(:>, [:x, :y, :z], [1, 2, 3])).
      must_equal "((x > 1) OR ((x = 1) AND (y > 2)) OR ((x = 1) AND (y = 2) AND (z > 3)))"
  end

  it "should keep inclusive ordering unknown on a NULL final column" do
    @d.literal(row_expr(:<=, [:x, :y], [1, nil])).
      must_equal "((x < 1) OR ((x = 1) AND (y <= NULL)))"
    @d.literal(row_expr(:>=, [:x, :y], [1, 2])).
      must_equal "((x > 1) OR ((x = 1) AND (y >= 2)))"
  end

  it "should check row arity before compiling row value comparisons" do
    proc{@d.literal(row_expr(:"=", [:x, :y], [1, 2, 3]))}.must_raise Sequel::Error
    proc{@d.literal(row_expr(:>, [:x], [1, 2]))}.must_raise Sequel::Error
  end

  it "should only emulate array-on-array row value comparisons" do
    # A scalar comparison must not be touched by the row value emulation.
    @d.literal(row_expr(:>, :x, 1)).must_equal "(x > 1)"
  end

  it "should emulate a finite multi-column IN with OR-ed row matches" do
    @d.literal(row_expr(:IN, [:x, :y], Sequel.value_list([[1, 2], [3, 4]]))).
      must_equal "(((x = 1) AND (y = 2)) OR ((x = 3) AND (y = 4)))"
  end

  it "should preserve parameter and duplicate value order in emulated IN" do
    sql = @d.literal(row_expr(:IN, [:x, :y], Sequel.value_list([[1, 2], [1, 2], [3, 4]])))
    sql.must_equal "(((x = 1) AND (y = 2)) OR ((x = 1) AND (y = 2)) OR ((x = 3) AND (y = 4)))"
  end

  it "should keep unknown results for IN candidates containing a NULL instead of false" do
    # The NULL marker (CASE WHEN col IS NULL THEN 1 ELSE 0 END) = NULL is
    # NULL when the column is actually NULL and FALSE otherwise, so a NULL
    # candidate never matches positively but stays unknown where applicable.
    @d.literal(row_expr(:IN, [:x, :y], Sequel.value_list([[1, nil]]))).
      must_equal "(((x = 1) AND ((CASE WHEN (y IS NULL) THEN 1 ELSE 0 END) = NULL)))"
    @d.literal(row_expr(:IN, [:x, :y], Sequel.value_list([[nil, nil]]))).
      must_equal "((((CASE WHEN (x IS NULL) THEN 1 ELSE 0 END) = NULL) AND ((CASE WHEN (y IS NULL) THEN 1 ELSE 0 END) = NULL)))"
  end

  it "should compile NOT IN as the negation of the whole emulated IN predicate" do
    in_sql = @d.literal(row_expr(:IN, [:x, :y], Sequel.value_list([[1, nil], [3, 4]])))
    not_in_sql = @d.literal(row_expr(:"NOT IN", [:x, :y], Sequel.value_list([[1, nil], [3, 4]])))
    not_in_sql.must_equal "NOT #{in_sql}"
    # No NULL candidates: still a single NOT wrapper preserving three-valued logic.
    @d.literal(row_expr(:"NOT IN", [:x, :y], Sequel.value_list([[1, 2], [3, 4]]))).
      must_equal "NOT (((x = 1) AND (y = 2)) OR ((x = 3) AND (y = 4)))"
  end

  it "should check candidate row arity before compiling emulated IN" do
    proc do
      @d.literal(row_expr(:IN, [:x, :y], Sequel.value_list([[1, 2], [3, 4, 5]])))
    end.must_raise Sequel::Error
    # The error is raised even when the malformed row is not the first one.
    proc do
      @d.literal(row_expr(:"NOT IN", [:x, :y], Sequel.value_list([[1, 2], [3]])))
    end.must_raise Sequel::Error
  end

  it "should keep the empty collection rules for emulated IN/NOT IN" do
    @d.literal(row_expr(:IN, [:x, :y], Sequel.value_list([]))).must_equal "(1 = 0)"
    @d.literal(row_expr(:"NOT IN", [:x, :y], Sequel.value_list([]))).must_equal "(1 = 1)"
  end

  it "should not build string concatenations of tuples" do
    sql = @d.literal(row_expr(:IN, [:x, :y], Sequel.value_list([[1, 2]])))
    sql.wont_include "||"
    sql.must_include "(x = 1)"
  end
end
