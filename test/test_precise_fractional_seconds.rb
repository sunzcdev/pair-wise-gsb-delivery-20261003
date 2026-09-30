require 'minitest_helper'

describe 'precise_fractional_seconds option' do
  FRAC = '999999999999'
  FRAC_RATIONAL = Rational(999_999_999_999, 10**12)

  def parse_precise(toml)
    Tomlrb.parse(toml, precise_fractional_seconds: true)
  end

  describe 'offset date-time' do
    it 'keeps fractions beyond nanoseconds without rounding into the next second' do
      time = parse_precise("d = 1979-05-27T23:59:59.#{FRAC}Z")['d']
      _(time).must_be_instance_of Time
      _([time.hour, time.min, time.sec]).must_equal [23, 59, 59]
      _(time.subsec).must_equal FRAC_RATIONAL
    end

    it 'expresses the same value for == and subsec across different offsets' do
      utc      = parse_precise("d = 1979-05-27T23:32:00.#{FRAC}Z")['d']
      negative = parse_precise("d = 1979-05-27T16:32:00.#{FRAC}-07:00")['d']
      positive = parse_precise("d = 1979-05-28T08:32:00.#{FRAC}+09:00")['d']
      _(negative).must_equal utc
      _(positive).must_equal utc
      _(negative.subsec).must_equal FRAC_RATIONAL
      _(positive.subsec).must_equal FRAC_RATIONAL
      _(negative.utc_offset).must_equal(-7 * 3600)
      _(positive.utc_offset).must_equal(9 * 3600)
    end

    it 'handles zero fractions' do
      time = parse_precise('d = 1979-05-27T07:32:00.000000000000Z')['d']
      _(time.subsec).must_equal 0
      _(time).must_equal Time.new(1979, 5, 27, 7, 32, 0, '+00:00')
    end
  end

  describe 'local date-time' do
    subject { parse_precise("d = 1979-05-27T23:59:59.#{FRAC}")['d'] }

    it 'keeps fractions beyond nanoseconds in to_s without rounding up' do
      _(subject.to_s).must_equal "1979-05-27T23:59:59.#{FRAC}"
    end

    it 'expresses the same value for ==, to_s and to_time' do
      same = parse_precise("d = 1979-05-27T23:59:59.#{FRAC}")['d']
      _(subject).must_equal same
      _(subject.to_time).must_equal same.to_time
      _(subject.to_time.subsec).must_equal FRAC_RATIONAL
      _([subject.hour, subject.min, subject.sec]).must_equal [23, 59, 59]
    end

    it 'does not fill in the machine time zone for to_time' do
      _(subject.to_time.utc_offset).must_equal 0
      _(subject.to_time('+09:00').utc_offset).must_equal 9 * 3600
      _(subject.to_time('+09:00').subsec).must_equal FRAC_RATIONAL
    end

    it 'handles zero fractions' do
      dt = parse_precise('d = 1979-05-27T00:32:00.000000000000')['d']
      _(dt.to_s).must_equal '1979-05-27T00:32:00'
      _(dt.to_time.subsec).must_equal 0
    end
  end

  describe 'local time' do
    subject { parse_precise("t = 23:59:59.#{FRAC}")['t'] }

    it 'keeps fractions beyond nanoseconds in to_s without rounding up' do
      _(subject.to_s).must_equal "23:59:59.#{FRAC}"
    end

    it 'expresses the same value for ==, to_s and to_time' do
      same = parse_precise("t = 23:59:59.#{FRAC}")['t']
      _(subject).must_equal same
      time = subject.to_time(1979, 5, 27)
      _(time).must_equal same.to_time(1979, 5, 27)
      _(time.subsec).must_equal FRAC_RATIONAL
      _([time.hour, time.min, time.sec]).must_equal [23, 59, 59]
    end

    it 'does not fill in the machine date or time zone' do
      time = subject.to_time(1979, 5, 27)
      _([time.year, time.month, time.day]).must_equal [1979, 5, 27]
      _(time.utc_offset).must_equal 0
    end

    it 'handles zero fractions' do
      t = parse_precise('t = 00:32:00.000000000000')['t']
      _(t.to_s).must_equal '00:32:00'
      _(t.to_time(1979, 5, 27).subsec).must_equal 0
    end

    it 'may normalize surplus trailing zeros' do
      t = parse_precise('t = 00:32:00.5000')['t']
      _(t.to_s).must_equal '00:32:00.5'
      _(t.to_time(1979, 5, 27).subsec).must_equal Rational(1, 2)
    end
  end

  describe 'default mode' do
    it 'keeps the existing float based return convention' do
      result = Tomlrb.parse("d = 1979-05-27T07:32:00.999999-07:00\nt = 00:32:00.999999")
      _(result['d']).must_equal Time.new(1979, 5, 27, 7, 32, 0.999999, '-07:00')
      _(result['t'].to_s).must_equal Tomlrb::LocalTime.new('00', '32', 0.999999).to_s
    end
  end
end
