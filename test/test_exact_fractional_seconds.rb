require 'minitest_helper'

describe 'exact fractional seconds mode' do
  PICO = Rational(999_999_999_999, 1_000_000_000_000)

  def parse_value(toml)
    Tomlrb.parse("v = #{toml}", exact_fractional_seconds: true)['v']
  end

  describe 'Local Time' do
    it 'keeps sub-nanosecond fractions without float drift' do
      v = parse_value('23:59:59.999999999999')
      _(v).must_be_kind_of Tomlrb::LocalTime
      _(v.to_s).must_equal '23:59:59.999999999999'
      _(v.sec).must_equal 59
      _(v.fraction).must_equal PICO
      _(v.nsec).must_equal 999_999_999
    end

    it 'never carries the next second when converting to Time' do
      v = parse_value('23:59:59.999999999999')
      t = v.to_time(2000, 1, 2)
      _(t.hour).must_equal 23
      _(t.min).must_equal 59
      _(t.sec).must_equal 59
      _(t.subsec).must_equal PICO
      _(t.nsec).must_equal 999_999_999
    end

    it 'does not attach a machine date or zone' do
      v = parse_value('08:30:00.25')
      _(v.to_s).must_equal '08:30:00.25'
      _(v.fraction).must_equal Rational(1, 4)
      t = v.to_time(2001, 2, 3, '+00:00')
      _([t.year, t.month, t.day]).must_equal [2001, 2, 3]
      _(t.utc_offset).must_equal 0
    end

    it 'normalizes a zero fraction away' do
      _(parse_value('00:00:00.000000000000').to_s).must_equal '00:00:00'
      _(parse_value('00:00:00').to_s).must_equal '00:00:00'
    end

    it 'strips redundant trailing zeroes consistently' do
      a = parse_value('00:32:00.555000')
      b = parse_value('00:32:00.555')
      _(a.to_s).must_equal '00:32:00.555'
      _(a).must_equal b
      _(a.fraction).must_equal b.fraction
    end

    it 'compares values by their exact fraction' do
      a = parse_value('00:00:00.000000000001')
      b = parse_value('00:00:00.000000000000')
      _(a).wont_equal b
      _(b).must_equal parse_value('00:00:00')
    end
  end

  describe 'Local Date-Time' do
    it 'keeps sub-nanosecond fractions without float drift' do
      v = parse_value('1979-05-27T00:32:00.999999999999')
      _(v).must_be_kind_of Tomlrb::LocalDateTime
      _(v.to_s).must_equal '1979-05-27T00:32:00.999999999999'
      _(v.fraction).must_equal PICO
    end

    it 'expresses the same value through ==, to_s and to_time' do
      v = parse_value('1979-05-27T00:32:00.999999999999')
      t = v.to_time
      _(t.sec).must_equal 0
      _(t.subsec).must_equal PICO
      _(t.nsec).must_equal 999_999_999
      _(t.utc_offset).must_equal 0
      expected = Tomlrb::LocalDateTime.new('1979', '05', '27', '00', '32',
                                           '00.999999999999', exact: true)
      _(v).must_equal expected
      _(v.to_s).must_equal expected.to_s
      _(v.to_time).must_equal expected.to_time
    end

    it 'honours an explicit offset on conversion but stays local by default' do
      v = parse_value('1979-05-27T07:32:00.5')
      _(v.to_time.utc_offset).must_equal 0
      shifted = v.to_time('-07:00')
      _(shifted.utc_offset).must_equal(-7 * 3600)
      _(shifted.subsec).must_equal Rational(1, 2)
    end

    it 'normalizes zero fractions and trailing zeroes' do
      _(parse_value('1979-05-27T00:00:00.000000000000').to_s)
        .must_equal '1979-05-27T00:00:00'
      a = parse_value('1979-05-27T07:32:00.555000000000')
      b = parse_value('1979-05-27T07:32:00.555')
      _(a.to_s).must_equal '1979-05-27T07:32:00.555'
      _(a).must_equal b
    end
  end

  describe 'Offset Date-Time' do
    it 'keeps sub-nanosecond fractions and the declared offset' do
      v = parse_value('1979-05-27T00:32:00.999999999999Z')
      _(v).must_be_kind_of Tomlrb::OffsetDateTime
      _(v.to_s).must_equal '1979-05-27T00:32:00.999999999999+00:00'
      _(v.fraction).must_equal PICO
      _(v.to_time.subsec).must_equal PICO
      _(v.to_time.nsec).must_equal 999_999_999
    end

    it 'treats the same instant in different offsets as equal' do
      west = parse_value('1979-05-27T07:32:00.999999999999-07:00')
      utc = parse_value('1979-05-27T14:32:00.999999999999Z')
      east = parse_value('1979-05-28T02:32:00.999999999999+12:00')
      _(west).must_equal utc
      _(utc).must_equal east
      _(west.to_time).must_equal utc.to_time
    end

    it 'keeps the wall-clock fields of the declared offset' do
      v = parse_value('1979-05-27T07:32:00.000000000001-07:00')
      _(v.hour).must_equal 7
      _(v.utc_offset).must_equal(-7 * 3600)
      _(v.to_s).must_equal '1979-05-27T07:32:00.000000000001-07:00'
      _(v.to_time.nsec).must_equal 0
      _(v.to_time.subsec).must_equal Rational(1, 1_000_000_000_000)
    end

    it 'normalizes zero fractions and trailing zeroes' do
      _(parse_value('1979-05-27T00:00:00.000000000000Z').to_s)
        .must_equal '1979-05-27T00:00:00+00:00'
      a = parse_value('1979-05-27T07:32:00.555000000000-07:00')
      b = parse_value('1979-05-27T07:32:00.555-07:00')
      _(a.to_s).must_equal '1979-05-27T07:32:00.555-07:00'
      _(a).must_equal b
    end
  end

  describe 'default compatibility' do
    it 'still returns a Time for offset date-times without the option' do
      v = Tomlrb.parse('v = 1979-05-27T07:32:00.555-07:00')['v']
      _(v).must_be_kind_of Time
    end

    it 'accepts the exact: alias' do
      v = Tomlrb.parse('v = 1979-05-27T00:32:00.5Z', exact: true)['v']
      _(v).must_be_kind_of Tomlrb::OffsetDateTime
      _(v.fraction).must_equal Rational(1, 2)
    end

    it 'keeps local types as local in both modes' do
      v = Tomlrb.parse('v = 1979-05-27T07:32:00.5', exact: true)['v']
      _(v).must_be_kind_of Tomlrb::LocalDateTime
      _(v.to_time.utc_offset).must_equal 0
    end
  end
end
