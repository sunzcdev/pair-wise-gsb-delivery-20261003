# frozen_string_literal: true

require "cases/helper"
require "models/numeric_data"

class NumericDataTest < ActiveRecord::TestCase
  def test_big_decimal_conditions
    m = NumericData.new(
      bank_balance: 1586.43,
      big_bank_balance: BigDecimal("1000234000567.95"),
      world_population: 6000000000,
      my_house_population: 3
    )
    assert m.save
    assert_equal 0, NumericData.where("bank_balance > ?", 2000.0).count
  end

  def test_numeric_fields
    m = NumericData.new(
      bank_balance: 1586.43,
      big_bank_balance: BigDecimal("1000234000567.95"),
      world_population: 2**62,
      my_house_population: 3
    )
    assert m.save

    m1 = NumericData.find_by(
      bank_balance: 1586.43,
      big_bank_balance: BigDecimal("1000234000567.95")
    )

    assert_kind_of Integer, m1.world_population
    assert_equal 2**62, m1.world_population

    assert_kind_of Integer, m1.my_house_population
    assert_equal 3, m1.my_house_population

    assert_kind_of BigDecimal, m1.bank_balance
    assert_equal BigDecimal("1586.43"), m1.bank_balance

    assert_kind_of BigDecimal, m1.big_bank_balance
    assert_equal BigDecimal("1000234000567.95"), m1.big_bank_balance
  end

  def test_numeric_fields_with_scale
    m = NumericData.new(
      bank_balance: 1586.43122334,
      big_bank_balance: BigDecimal("234000567.952344"),
      world_population: 2**62,
      my_house_population: 3
    )
    assert m.save

    m1 = NumericData.find_by(
      bank_balance: 1586.43122334,
      big_bank_balance: BigDecimal("234000567.952344")
    )

    assert_kind_of Integer, m1.world_population
    assert_equal 2**62, m1.world_population

    assert_kind_of Integer, m1.my_house_population
    assert_equal 3, m1.my_house_population

    assert_kind_of BigDecimal, m1.bank_balance
    assert_equal BigDecimal("1586.43"), m1.bank_balance

    assert_kind_of BigDecimal, m1.big_bank_balance
    assert_equal BigDecimal("234000567.95"), m1.big_bank_balance
  end

  def test_numeric_fields_without_scale_cast_long_strings
    m = NumericData.new(
      world_population: "18446744073709551615",
      atoms_in_universe: "1" * 55
    )

    assert_equal 18446744073709551615, m.world_population
    assert_equal ("1" * 55).to_i, m.atoms_in_universe
  end

  if current_adapter?(:PostgreSQLAdapter)
    def test_numeric_fields_with_nan
      m = NumericData.new(
        bank_balance: BigDecimal("NaN"),
        big_bank_balance: BigDecimal("NaN"),
        world_population: 2**62,
        my_house_population: 3
      )
      assert_predicate m.bank_balance, :nan?
      assert_predicate m.big_bank_balance, :nan?
      assert m.save

      m1 = NumericData.find_by(
        bank_balance: BigDecimal("NaN"),
        big_bank_balance: BigDecimal("NaN")
      )

      assert_predicate m1.bank_balance, :nan?
      assert_predicate m1.big_bank_balance, :nan?
    end
  end

  test "decimal range bounds are interpreted at the column scale" do
    create_balance(1.00)
    create_balance(1.01)
    create_balance(1.02)
    create_balance(1.03)
    create_balance(nil)

    assert_balance_range BigDecimal("1.005")..BigDecimal("1.02"), [1.01, 1.02]
  end

  test "an upper bound with more decimals than the column stores is narrowed into the range" do
    create_balance(1.00)
    create_balance(1.01)
    create_balance(1.02)
    create_balance(1.03)

    assert_balance_range BigDecimal("1.00")..BigDecimal("1.019"), [1.00, 1.01]
    assert_balance_range BigDecimal("1.00")..BigDecimal("1.021"), [1.00, 1.01, 1.02]
  end

  test "an excluded upper bound selects the last storable value below it" do
    create_balance(1.00)
    create_balance(1.01)
    create_balance(1.02)
    create_balance(1.03)

    assert_balance_range BigDecimal("1.00")...BigDecimal("1.02"), [1.00, 1.01]
    assert_balance_range BigDecimal("1.00")...BigDecimal("1.025"), [1.00, 1.01, 1.02]
  end

  test "a range with no storable value selects nothing and its negation selects everything" do
    create_balance(1.00)
    create_balance(1.01)
    create_balance(1.02)

    assert_balance_range BigDecimal("1.005")..BigDecimal("1.005"), []
    assert_balance_range BigDecimal("1.001")...BigDecimal("1.009"), []
    assert_balance_range BigDecimal("1.03")..BigDecimal("1.00"), []
  end

  test "a range on storable values selects the same rows in both directions" do
    create_balance(1.00)
    create_balance(1.01)
    create_balance(1.02)
    create_balance(1.03)
    create_balance(nil)

    assert_balance_range BigDecimal("1.00")..BigDecimal("1.02"), [1.00, 1.01, 1.02]
    assert_balance_range BigDecimal("1.00")...BigDecimal("1.02"), [1.00, 1.01]
  end

  test "a range with an open bound keeps the SQL meaning of nil" do
    create_balance(1.00)
    create_balance(1.01)
    create_balance(1.02)
    create_balance(nil)

    assert_balance_range nil..BigDecimal("1.015"), [1.00, 1.01]
    assert_balance_range BigDecimal("1.005")..nil, [1.01, 1.02]
  end

  test "decimal range bounds carry across a whole number" do
    create_balance(9.99)
    create_balance(10.00)
    create_balance(10.01)

    assert_balance_range BigDecimal("9.995")..BigDecimal("10.005"), [10.00]
    assert_balance_range BigDecimal("9.99")...BigDecimal("10.01"), [9.99, 10.00]
  end

  test "decimal range bounds are interpreted on unscaled decimal columns too" do
    create_balance(1, column: :unscaled_bank_balance)
    create_balance(2, column: :unscaled_bank_balance)
    create_balance(3, column: :unscaled_bank_balance)

    relation = NumericData.where(unscaled_bank_balance: BigDecimal("1.5")..BigDecimal("3.5"))
    assert_equal [2, 3], relation.order(:unscaled_bank_balance).pluck(:unscaled_bank_balance)
    assert_equal [1], NumericData.where.not(unscaled_bank_balance: BigDecimal("1.5")..BigDecimal("3.5")).pluck(:unscaled_bank_balance)
  end

  test "a decimal range on a column without a reported scale is reported instead of guessed" do
    model = Class.new(ActiveRecord::Base) do
      self.table_name = "numeric_data"
      def self.name
        "ScaleFreeNumericData"
      end
    end
    model.attribute :bank_balance, Class.new(ActiveRecord::Type::Decimal) {
      def self.name
        "ScaleFreeDecimal"
      end

      def stored_decimal_scale
        nil
      end

      def stored_decimal_scale_known?
        false
      end
    }.new

    error = assert_raises(ActiveRecord::RangePrecisionError) do
      model.where(bank_balance: BigDecimal("1.001")..BigDecimal("1.009")).to_a
    end
    assert_match(/bank_balance/, error.message)
    assert_match(/ScaleFreeDecimal/, error.message)

    assert_raises(ActiveRecord::RangePrecisionError) do
      model.where.not(bank_balance: BigDecimal("1.001")..BigDecimal("1.009")).to_a
    end

    assert_equal 0, model.where(bank_balance: 1..2).count
  end

  private
    def create_balance(value, column: :bank_balance)
      NumericData.create!(column => value)
    end

    def assert_balance_range(range, expected)
      expected = expected.map { |value| BigDecimal(value.to_s) }

      relation = NumericData.where(bank_balance: range)
      actual = relation.where.not(bank_balance: nil).order(:bank_balance).pluck(:bank_balance)
      assert_equal expected, actual, "forward: #{relation.to_sql}"

      negated = NumericData.where.not(bank_balance: range)
      negated_values = negated.where.not(bank_balance: nil).order(:bank_balance).pluck(:bank_balance)
      stored = NumericData.where.not(bank_balance: nil).order(:bank_balance).pluck(:bank_balance)
      assert_equal stored - expected, negated_values, "negated: #{negated.to_sql}"
    end
end
