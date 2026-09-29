# encoding: utf-8
require 'test_helper'

class I18nCountTest < I18n::TestCase
  test "preserves the decimal representation of text input" do
    assert_equal "1",    I18n::Count.new("1").to_s
    assert_equal "1.0",  I18n::Count.new("1.0").to_s
    assert_equal "1.00", I18n::Count.new("1.00").to_s
  end

  test "distinguishes the visible fraction digits of 1, 1.0 and 1.00" do
    assert_equal 0, I18n::Count.new("1").decimal_places
    assert_equal 1, I18n::Count.new("1.0").decimal_places
    assert_equal 2, I18n::Count.new("1.00").decimal_places
  end

  test "exposes CLDR style read-only operands" do
    count = I18n::Count.new("-12.340")
    assert_equal Rational(12340, 1000), count.n
    assert_equal Rational(12340, 1000), count.absolute_value
    assert_equal 12,  count.i
    assert_equal 12,  count.integer_part
    assert_equal 3,   count.v
    assert_equal 3,   count.decimal_places
    assert_equal 340, count.f
    assert_equal 340, count.fraction_part
    assert_equal 34,  count.t
    assert_equal 34,  count.trimmed_fraction_part
    assert_equal "-12.340", count.source
  end

  test "exposes sign predicates" do
    negative = I18n::Count.new("-1.5")
    assert negative.negative?
    refute negative.positive?
    refute negative.zero?
    assert_equal(-1, negative.sign)

    positive = I18n::Count.new("1.5")
    refute positive.negative?
    assert positive.positive?
    assert_equal 1, positive.sign

    zero = I18n::Count.new("0.00")
    refute zero.negative?
    refute zero.positive?
    assert zero.zero?
    assert_equal 0, zero.sign
  end

  test "handles very long fractions without precision loss" do
    source = "1." + "0" * 49 + "1"
    count = I18n::Count.new(source)
    assert_equal 50, count.decimal_places
    assert_equal 1, count.t
    assert_equal source, count.to_s
    assert_equal Rational(source), count.to_r
    refute_equal count.to_r, source.to_f.to_r
  end

  test "handles negative very long fractions" do
    source = "-0." + "0" * 99 + "5"
    count = I18n::Count.new(source)
    assert_equal 100, count.decimal_places
    assert_equal 0, count.integer_part
    assert count.negative?
    assert_equal source, count.to_s
    assert_equal Rational(source), count.to_r
  end

  test "to_numeric returns an Integer without visible fraction digits and an exact Rational otherwise" do
    integral = I18n::Count.new("1").to_numeric
    assert_instance_of Integer, integral
    assert_equal 1, integral

    assert_instance_of Rational, I18n::Count.new("1.00").to_numeric
    assert_equal 1, I18n::Count.new("1.00").to_numeric

    decimal = I18n::Count.new("1.50").to_numeric
    assert_instance_of Rational, decimal
    assert_equal Rational(3, 2), decimal
  end

  test "compares equal to numerics with the same value" do
    assert I18n::Count.new("1.00") == 1
    assert I18n::Count.new("0.0") == 0
    assert I18n::Count.new("1.0") == I18n::Count.new("1.00")
    assert I18n::Count.new("2.5") > 2
    assert I18n::Count.new("-1") < 0
    refute I18n::Count.new("1") == "1"
  end

  test "supports the numeric protocol expected by legacy pluralization rules" do
    assert_equal 1,  I18n::Count.new("21") % 10
    assert_equal 21, I18n::Count.new("21") % 100
    assert (2..10).include?(I18n::Count.new("5") % 100)
    assert_equal 1, 10 % I18n::Count.new("3")
    assert_equal Rational(3, 2), I18n::Count.new("1.0") + I18n::Count.new("0.5")
    assert_equal Rational(1, 2), I18n::Count.new("1.5") - 1
    assert_equal 3, I18n::Count.new("1.5") * 2
    assert_equal Rational(1, 2), I18n::Count.new("1.0") / 2
  end

  test "abs and unary minus preserve the decimal representation" do
    assert_equal "1.50",  I18n::Count.new("-1.50").abs.to_s
    assert_equal "-1.50", (-I18n::Count.new("1.50")).to_s
    assert_equal "1.50",  (-I18n::Count.new("-1.50")).to_s
  end

  test "accepts numerics" do
    assert_equal "1",    I18n::Count.new(1).to_s
    assert_equal "1.5",  I18n::Count.new(1.5).to_s
    assert_equal "1.00", I18n::Count[I18n::Count.new("1.00")].to_s
  end

  test "raises ArgumentError for invalid input" do
    assert_raises(ArgumentError) { I18n::Count.new("abc") }
    assert_raises(ArgumentError) { I18n::Count.new("1.") }
    assert_raises(ArgumentError) { I18n::Count.new(".5") }
    assert_raises(ArgumentError) { I18n::Count.new(nil) }
    assert_raises(ArgumentError) { I18n::Count.new(Object.new) }
  end

  test "is immutable" do
    assert I18n::Count.new("1.00").frozen?
  end
end
