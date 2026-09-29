require 'test_helper'

class I18nCountTest < I18n::TestCase
  test "distinguishes 1, 1.0 and 1.00 by visible fraction digits" do
    assert_equal 0, I18n::Count.new("1").fraction_digits
    assert_equal 1, I18n::Count.new("1.0").fraction_digits
    assert_equal 2, I18n::Count.new("1.00").fraction_digits
  end

  test "exposes read-only operands" do
    count = I18n::Count.new("-12.340")
    assert_equal 12, count.integer_part
    assert_equal "340", count.fraction_part
    assert_equal 3, count.fraction_digits
    assert_equal 340, count.fraction_value
    assert_equal true, count.negative?
    assert_equal(-1, count.sign)
    assert_equal false, count.zero?
  end

  test "operands for integers" do
    count = I18n::Count.new("7")
    assert_equal 7, count.integer_part
    assert_equal "", count.fraction_part
    assert_equal 0, count.fraction_digits
    assert_equal 0, count.fraction_value
    assert_equal false, count.negative?
    assert_equal 1, count.sign
  end

  test "zero" do
    assert I18n::Count.new("0").zero?
    assert I18n::Count.new("0.00").zero?
    assert I18n::Count.new("-0").zero?
    assert_equal 0, I18n::Count.new("-0.0").sign
  end

  test "to_s preserves the original decimal representation" do
    assert_equal "1", I18n::Count.new("1").to_s
    assert_equal "1.0", I18n::Count.new("1.0").to_s
    assert_equal "1.00", I18n::Count.new("1.00").to_s
    assert_equal "-1.00", I18n::Count.new("-1.00").to_s
    assert_equal "1", I18n::Count.new("+1").to_s
  end

  test "preserves very long fractions" do
    text = "0.123456789012345678901234567890123456789"
    count = I18n::Count.new(text)
    assert_equal 39, count.fraction_digits
    assert_equal text, count.to_s
    assert_equal Rational("123456789012345678901234567890123456789") / 10**39, count.to_r
  end

  test "to_numeric returns an Integer for integers and a Rational for decimals, never a Float" do
    assert_equal 1, I18n::Count.new("1").to_numeric
    assert_instance_of Integer, I18n::Count.new("1").to_numeric
    assert_equal Rational(1, 1), I18n::Count.new("1.00").to_numeric
    assert_instance_of Rational, I18n::Count.new("1.00").to_numeric
    refute_instance_of Float, I18n::Count.new("0.1").to_numeric
  end

  test "converts to plain numerics" do
    count = I18n::Count.new("-2.50")
    assert_equal(-2, count.to_i)
    assert_equal(-2.5, count.to_f)
    assert_equal Rational(-5, 2), count.to_r
  end

  test "compares equal to plain numerics by value" do
    assert I18n::Count.new("1.00") == 1
    assert I18n::Count.new("1.00") == 1.0
    assert I18n::Count.new("0.0") == 0
    assert I18n::Count.new("-0") == 0
    refute I18n::Count.new("1.00") == 2
    assert I18n::Count.new("1.0") == I18n::Count.new("1.00")
    assert I18n::Count.new("2") > I18n::Count.new("1.5")
    assert I18n::Count.new("-1") < 0
  end

  test "delegates numeric messages so legacy rules keep working" do
    assert_equal 1, I18n::Count.new("21") % 10
    assert_equal 11, I18n::Count.new("111") % 100
    assert_equal Rational(3, 2), I18n::Count.new("21.5") % 10
    assert (2..10).include?(I18n::Count.new("5") % 100)
    refute I18n::Count.new("21").respond_to?(:call)
  end

  test "I18n::Count.numeric is the compatibility entry for legacy rules" do
    assert_equal 1, I18n::Count.numeric(I18n::Count.new("1"))
    assert_equal Rational(1, 1), I18n::Count.numeric(I18n::Count.new("1.00"))
    assert_equal 1.5, I18n::Count.numeric(1.5)
    assert_equal 2, I18n::Count.numeric(2)
  end

  test "parse wraps decimal text, passes everything else through" do
    assert_instance_of I18n::Count, I18n::Count.parse("1.00")
    count = I18n::Count.new("1")
    assert_same count, I18n::Count.parse(count)
    assert_equal 1, I18n::Count.parse(1)
    assert_equal 1.5, I18n::Count.parse(1.5)
    assert_equal "not a number", I18n::Count.parse("not a number")
  end

  test "raises on invalid decimal text" do
    assert_raises(I18n::ArgumentError) { I18n::Count.new("abc") }
    assert_raises(I18n::ArgumentError) { I18n::Count.new("1.2.3") }
  end
end
