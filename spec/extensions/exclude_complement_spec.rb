# frozen_string_literal: true
require_relative "spec_helper"

describe "exclude_complement extension" do
  before do
    @ds = Sequel.mock[:t].extension(:exclude_complement)
    @c = Sequel::SQL::BooleanExpression
  end

  it "#exclude_complement should invert a field comparison and keep NULL rows" do
    @ds.exclude_complement(a: 1).sql.must_equal "SELECT * FROM t WHERE ((a != 1) OR (a IS NULL))"
  end

  it "#exclude_complement should handle the != comparison operator" do
    @ds.exclude_complement(@c.new(:"!=", :a, 1)).sql.must_equal "SELECT * FROM t WHERE ((a = 1) OR (a IS NULL))"
  end

  it "#exclude_complement should handle inequality comparisons" do
    @ds.exclude_complement(Sequel.expr(:a) > 1).sql.must_equal "SELECT * FROM t WHERE ((a <= 1) OR (a IS NULL))"
    @ds.exclude_complement(Sequel.expr(:a) < 1).sql.must_equal "SELECT * FROM t WHERE ((a >= 1) OR (a IS NULL))"
    @ds.exclude_complement(Sequel.expr(:a) >= 1).sql.must_equal "SELECT * FROM t WHERE ((a < 1) OR (a IS NULL))"
    @ds.exclude_complement(Sequel.expr(:a) <= 1).sql.must_equal "SELECT * FROM t WHERE ((a > 1) OR (a IS NULL))"
  end

  it "#exclude_complement should combine multiple hash pairs with OR" do
    @ds.exclude_complement(a: 1, b: 2).sql.must_equal "SELECT * FROM t WHERE ((a != 1) OR (a IS NULL) OR (b != 2) OR (b IS NULL))"
  end

  it "#exclude_complement should complement IN with an array and keep NULL rows" do
    @ds.exclude_complement(a: [1, 2]).sql.must_equal "SELECT * FROM t WHERE ((a NOT IN (1, 2)) OR (a IS NULL))"
  end

  it "#exclude_complement should remove NULL entries from an IN value list" do
    @ds.exclude_complement(a: [1, nil]).sql.must_equal "SELECT * FROM t WHERE ((a NOT IN (1)) OR (a IS NULL))"
  end

  it "#exclude_complement should match every row when IN only contains NULL or is empty" do
    @ds.exclude_complement(a: [nil]).sql.must_equal "SELECT * FROM t WHERE 't'"
    @ds.exclude_complement(a: []).sql.must_equal "SELECT * FROM t WHERE 't'"
  end

  it "#exclude_complement should handle IN with a set" do
    @ds.exclude_complement(a: Set[1, 2]).sql.must_equal "SELECT * FROM t WHERE ((a NOT IN (1, 2)) OR (a IS NULL))"
  end

  it "#exclude_complement should complement NOT IN and keep NULL rows" do
    @ds.exclude_complement(@c.new(:"NOT IN", :a, [1, 2])).sql.must_equal "SELECT * FROM t WHERE ((a IN (1, 2)) OR (a IS NULL))"
  end

  it "#exclude_complement should keep every row for NOT IN containing NULL" do
    @ds.exclude_complement(@c.new(:"NOT IN", :a, [1, nil])).sql.must_equal "SELECT * FROM t WHERE 't'"
    @ds.exclude_complement(@c.new(:"NOT IN", :a, [nil])).sql.must_equal "SELECT * FROM t WHERE 't'"
  end

  it "#exclude_complement should match no rows for NOT IN of an empty list" do
    @ds.exclude_complement(@c.new(:"NOT IN", :a, [])).sql.must_equal "SELECT * FROM t WHERE 'f'"
  end

  it "#exclude_complement should complement IN with a subquery and keep NULL rows" do
    @ds.exclude_complement(a: @ds.db[:s].select(:x)).sql.must_equal "SELECT * FROM t WHERE ((a NOT IN (SELECT x FROM s)) OR (a IS NULL))"
  end

  it "#exclude_complement should complement NOT IN with a subquery" do
    @ds.exclude_complement(@c.new(:"NOT IN", :a, @ds.db[:s].select(:x))).sql.must_equal "SELECT * FROM t WHERE ((a IN (SELECT x FROM s)) OR (a IS NULL))"
  end

  it "#exclude_complement should apply De Morgan's laws to AND and OR" do
    @ds.exclude_complement(Sequel.expr(a: 1) & {b: 2}).sql.must_equal "SELECT * FROM t WHERE ((a != 1) OR (a IS NULL) OR (b != 2) OR (b IS NULL))"
    @ds.exclude_complement(Sequel.expr(a: 1) | {b: 2}).sql.must_equal "SELECT * FROM t WHERE (((a != 1) OR (a IS NULL)) AND ((b != 2) OR (b IS NULL)))"
  end

  it "#exclude_complement should complement ranges" do
    @ds.exclude_complement(a: 1..5).sql.must_equal "SELECT * FROM t WHERE ((a < 1) OR (a IS NULL) OR (a > 5) OR (a IS NULL))"
  end

  it "#exclude_complement should invert IS and IS NOT without adding NULL clauses" do
    @ds.exclude_complement(a: nil).sql.must_equal "SELECT * FROM t WHERE (a IS NOT NULL)"
    @ds.exclude_complement(a: true).sql.must_equal "SELECT * FROM t WHERE (a IS NOT TRUE)"
    @ds.exclude_complement(a: false).sql.must_equal "SELECT * FROM t WHERE (a IS NOT FALSE)"
  end

  it "#exclude_complement should handle a bare boolean column" do
    @ds.exclude_complement(:a).sql.must_equal "SELECT * FROM t WHERE (NOT a OR (a IS NULL))"
  end

  it "#exclude_complement should handle boolean constants" do
    @ds.exclude_complement(true).sql.must_equal "SELECT * FROM t WHERE 'f'"
    @ds.exclude_complement(false).sql.must_equal "SELECT * FROM t WHERE 't'"
    @ds.exclude_complement(Sequel::NULL).sql.must_equal "SELECT * FROM t WHERE NOT NULL"
    @ds.exclude_complement(Sequel::NOTNULL).sql.must_equal "SELECT * FROM t WHERE NULL"
  end

  it "#exclude_complement should handle LIKE and NOT LIKE" do
    @ds.exclude_complement(Sequel.like(:a, "x")).sql.must_equal "SELECT * FROM t WHERE ((a NOT LIKE 'x' ESCAPE '\\') OR (a IS NULL))"
    @ds.exclude_complement(@c.new(:"NOT LIKE", :a, "x")).sql.must_equal "SELECT * FROM t WHERE ((a LIKE 'x' ESCAPE '\\') OR (a IS NULL))"
  end

  it "#exclude_complement should handle a NOT wrapper" do
    @ds.exclude_complement(~Sequel.expr(a: 1)).sql.must_equal "SELECT * FROM t WHERE ((a = 1) OR (a IS NULL))"
  end

  it "#exclude_complement should add a NULL clause for each column in a column to column comparison" do
    @ds.exclude_complement(Sequel.expr(:a) > :b).sql.must_equal "SELECT * FROM t WHERE ((a <= b) OR (a IS NULL) OR (b IS NULL))"
  end

  it "#exclude_complement should handle qualified identifiers" do
    @ds.exclude_complement(Sequel[:t][:a] > 1).sql.must_equal "SELECT * FROM t WHERE ((t.a <= 1) OR (t.a IS NULL))"
  end

  it "#exclude_complement_having should affect the HAVING clause" do
    @ds.exclude_complement_having(a: 1).sql.must_equal "SELECT * FROM t HAVING ((a != 1) OR (a IS NULL))"
  end

  it "should not change the three-valued logic of exclude" do
    @ds.exclude(a: 1).sql.must_equal "SELECT * FROM t WHERE (a != 1)"
    @ds.exclude_having(a: 1).sql.must_equal "SELECT * FROM t HAVING (a != 1)"
  end

  it "should combine with an existing WHERE clause using AND" do
    @ds.where(x: 9).exclude_complement(a: 1).sql.must_equal "SELECT * FROM t WHERE ((x = 9) AND ((a != 1) OR (a IS NULL)))"
  end

  it "should keep parameters bound instead of interpolating them" do
    ps = @ds.exclude_complement(a: :$a).prepare(:select, :ps)
    ps.sql.must_equal "SELECT * FROM t WHERE ((a != $a) OR (a IS NULL))"
  end

  it "should not treat a prepared statement placeholder symbol as a nullable column" do
    @ds.exclude_complement(@c.new(:>, :a, :$a)).sql.must_equal "SELECT * FROM t WHERE ((a <= $a) OR (a IS NULL))"
  end

  it "should raise for a raw SQL fragment" do
    proc { @ds.exclude_complement(Sequel.lit("a > 1")) }.must_raise Sequel::Error
  end

  it "should raise for a comparison against a SQL function" do
    proc { @ds.exclude_complement(Sequel.function(:foo, :a) => 1) }.must_raise Sequel::Error
  end

  it "should raise for an EXISTS subquery" do
    proc { @ds.exclude_complement(@ds.db[:s].exists) }.must_raise Sequel::Error
  end

  it "should raise for a CASE expression in a comparison" do
    proc { @ds.exclude_complement(Sequel.case({a: 1}, 0) => 1) }.must_raise Sequel::Error
  end

  it "should raise for a bare placeholder symbol" do
    proc { @ds.exclude_complement(:$a) }.must_raise Sequel::Error
  end

  it "should raise when no argument is provided without a block" do
    proc { @ds.exclude_complement }.must_raise Sequel::Error
  end

  it "should work on a frozen dataset" do
    @ds.freeze.exclude_complement(a: 1).sql.must_equal "SELECT * FROM t WHERE ((a != 1) OR (a IS NULL))"
  end
end