require 'spec_helper'
require 'dentaku'

describe Dentaku::Units do
  let(:calculator) { Dentaku::Calculator.new(units: true) }

  def quantity(value, unit)
    Dentaku::Units::Quantity.new(value, unit)
  end

  describe 'declaring units on input variables' do
    it 'accepts {value:, unit:} hashes' do
      result = calculator.evaluate!('t', t: { value: 5, unit: :s })
      expect(result).to be_a(Dentaku::Units::Quantity)
      expect(result.value).to eq(5)
      expect(result.units).to eq(s: 1)
    end

    it 'accepts Quantity values directly' do
      result = calculator.evaluate!('t * 2', t: quantity(5, :ms))
      expect(result.value).to eq(10)
      expect(result.units).to eq(ms: 1)
    end

    it 'wraps plain numerics in units declared at construction' do
      calc = Dentaku::Calculator.new(units: { distance: :m, time: :s })
      result = calc.evaluate!('distance / time', distance: 100, time: 10)
      expect(result.units).to eq(m: 1, s: -1)
    end

    it 'raises for unknown units' do
      expect { calculator.evaluate!('x', x: { value: 1, unit: :furlong }) }
        .to raise_error(ArgumentError, /unknown unit/)
    end

    it 'supports registering custom units' do
      Dentaku::Units.register(:ft, :length, 0.3048)
      result = calculator.evaluate!('a + b', a: { value: 1, unit: :m }, b: { value: 1, unit: :ft })
      expect(result.value).to eq(BigDecimal('1.3048'))
    end
  end

  describe 'arithmetic with unit conversion' do
    it 'mixes seconds and milliseconds' do
      result = calculator.evaluate!('t1 + t2', t1: { value: 90, unit: :s }, t2: { value: 1500, unit: :ms })
      expect(result.value).to eq(BigDecimal('91.5'))
      expect(result.units).to eq(s: 1)
    end

    it 'reports the result in the left operand unit' do
      result = calculator.evaluate!('t1 + t2', t1: { value: 1500, unit: :ms }, t2: { value: 90, unit: :s })
      expect(result.value).to eq(BigDecimal('91500'))
      expect(result.units).to eq(ms: 1)
    end

    it 'subtracts compatible units' do
      result = calculator.evaluate!('t1 - t2', t1: { value: 90, unit: :s }, t2: { value: 1500, unit: :ms })
      expect(result.value).to eq(BigDecimal('88.5'))
    end

    it 'computes speed as a derived dimension' do
      result = calculator.evaluate!('distance / time',
        distance: { value: 100, unit: :m }, time: { value: 10, unit: :s })
      expect(result.value).to eq(BigDecimal('10'))
      expect(result.units).to eq(m: 1, s: -1)
      expect(result.dimension).to eq(length: 1, time: -1)
    end

    it 'scales quantities by dimensionless numbers' do
      result = calculator.evaluate!('distance * 2', distance: { value: 5, unit: :km })
      expect(result.value).to eq(10)
      expect(result.units).to eq(km: 1)
    end

    it 'returns a plain numeric when dimensions cancel out' do
      result = calculator.evaluate!('a / b', a: { value: 100, unit: :m }, b: { value: 50, unit: :m })
      expect(result).to eq(BigDecimal('2'))
      expect(result).not_to be_a(Dentaku::Units::Quantity)
    end

    it 'raises quantities to integer powers' do
      result = calculator.evaluate!('x ^ 2', x: { value: 3, unit: :m })
      expect(result.value).to eq(9)
      expect(result.units).to eq(m: 2)
    end

    it 'rejects non-integer powers of dimensioned quantities' do
      expect { calculator.evaluate!('x ^ 0.5', x: { value: 3, unit: :m }) }
        .to raise_error(Dentaku::UnitError, /non-integer power/)
    end

    it 'computes modulo in the left operand unit' do
      result = calculator.evaluate!('a % b', a: { value: 90, unit: :s }, b: { value: 1500, unit: :ms })
      expect(result.value).to eq(BigDecimal('0'))
      expect(result.units).to eq(s: 1)
    end

    it 'applies percentages without dropping units' do
      result = calculator.evaluate!('a * 10%', a: { value: 90, unit: :s })
      expect(result.value).to eq(BigDecimal('9'))
      expect(result.units).to eq(s: 1)
    end

    it 'negates quantities' do
      result = calculator.evaluate!('-a', a: { value: 90, unit: :s })
      expect(result.value).to eq(-90)
      expect(result.units).to eq(s: 1)
    end
  end

  describe 'incompatible operations' do
    it 'rejects adding length and time, locating the subexpression' do
      error = nil
      begin
        calculator.evaluate!('length + duration',
          length: { value: 100, unit: :m }, duration: { value: 10, unit: :s })
      rescue Dentaku::UnitError => e
        error = e
      end

      expect(error).not_to be_nil
      expect(error.message).to include('cannot add quantities with incompatible dimensions (length vs time)')
      expect(error.expression).to eq('length + duration')
      expect(error.message).to include('in "length + duration"')
    end

    it 'locates the innermost offending subexpression' do
      begin
        calculator.evaluate!('(length + duration) * 2 + rest',
          length: { value: 100, unit: :m }, duration: { value: 10, unit: :s }, rest: 1)
        raise 'expected UnitError'
      rescue Dentaku::UnitError => e
        expect(e.expression).to eq('length + duration')
      end
    end

    it 'rejects subtracting incompatible dimensions' do
      expect {
        calculator.evaluate!('a - b', a: { value: 1, unit: :kg }, b: { value: 1, unit: :s })
      }.to raise_error(Dentaku::UnitError, /cannot subtract/)
    end

    it 'rejects adding a dimensioned quantity to a plain number' do
      expect {
        calculator.evaluate!('a + 1', a: { value: 1, unit: :m })
      }.to raise_error(Dentaku::UnitError, /length vs dimensionless/)
    end

    it 'rejects comparisons of incompatible dimensions' do
      error = nil
      begin
        calculator.evaluate!('a < b', a: { value: 1, unit: :m }, b: { value: 1, unit: :s })
      rescue Dentaku::UnitError => e
        error = e
      end

      expect(error).not_to be_nil
      expect(error.message).to include('cannot compare quantities with incompatible dimensions (length vs time)')
      expect(error.expression).to eq('a < b')
    end

    it 'rejects equality checks of incompatible dimensions' do
      expect {
        calculator.evaluate!('a = b', a: { value: 1, unit: :m }, b: { value: 1, unit: :s })
      }.to raise_error(Dentaku::UnitError)
    end
  end

  describe 'comparisons with conversion' do
    it 'compares compatible units by base value' do
      expect(calculator.evaluate!('t1 > t2',
        t1: { value: 90, unit: :s }, t2: { value: 1500, unit: :ms })).to eq(true)
      expect(calculator.evaluate!('t1 < t2',
        t1: { value: 90, unit: :s }, t2: { value: 1500, unit: :ms })).to eq(false)
    end

    it 'compares equal quantities across units' do
      expect(calculator.evaluate!('a = b',
        a: { value: 1, unit: :km }, b: { value: 1000, unit: :m })).to eq(true)
    end
  end

  describe 'supported functions' do
    it 'propagates units through SUM' do
      result = calculator.evaluate!('SUM(a, b)', a: { value: 1, unit: :s }, b: { value: 500, unit: :ms })
      expect(result.value).to eq(BigDecimal('1.5'))
      expect(result.units).to eq(s: 1)
    end

    it 'propagates units through AVG' do
      result = calculator.evaluate!('AVG(a, b)', a: { value: 1, unit: :s }, b: { value: 500, unit: :ms })
      expect(result.value).to eq(BigDecimal('0.75'))
    end

    it 'propagates units through MIN and MAX' do
      data = { a: { value: 1, unit: :s }, b: { value: 500, unit: :ms } }
      expect(calculator.evaluate!('MIN(a, b)', data).units).to eq(ms: 1)
      expect(calculator.evaluate!('MAX(a, b)', data).units).to eq(s: 1)
    end

    it 'propagates units through ABS, ROUND, ROUNDUP and ROUNDDOWN' do
      expect(calculator.evaluate!('ABS(x)', x: { value: -3, unit: :m }).value).to eq(3)
      expect(calculator.evaluate!('ROUND(x, 1)', x: { value: 1.26, unit: :m }).value).to eq(BigDecimal('1.3'))
      expect(calculator.evaluate!('ROUNDUP(x, 1)', x: { value: 1.21, unit: :m }).value).to eq(BigDecimal('1.3'))
      expect(calculator.evaluate!('ROUNDDOWN(x, 1)', x: { value: 1.29, unit: :m }).value).to eq(BigDecimal('1.2'))
    end

    it 'raises when aggregating incompatible dimensions' do
      expect {
        calculator.evaluate!('SUM(a, b)', a: { value: 1, unit: :m }, b: { value: 1, unit: :s })
      }.to raise_error(Dentaku::UnitError, /SUM\(a, b\)/)
    end
  end

  describe 'conditional branches' do
    it 'returns the taken branch quantity when dimensions agree' do
      result = calculator.evaluate!('IF(x > 0, a, b)',
        x: 1, a: { value: 1, unit: :m }, b: { value: 2000, unit: :mm })
      expect(result.value).to eq(1)
      expect(result.units).to eq(m: 1)
    end

    it 'rejects IF branches with incompatible dimensions' do
      error = nil
      begin
        calculator.evaluate!('IF(x > 0, a, b)',
          x: 1, a: { value: 1, unit: :m }, b: { value: 2, unit: :s })
      rescue Dentaku::UnitError => e
        error = e
      end

      expect(error).not_to be_nil
      expect(error.message).to include('conditional branches have incompatible dimensions (length vs time)')
      expect(error.expression).to eq('IF(x > 0, a, b)')
    end

    it 'rejects a dimensioned branch against a dimensionless one' do
      expect {
        calculator.evaluate!('IF(x > 0, a, 5)', x: 1, a: { value: 1, unit: :m })
      }.to raise_error(Dentaku::UnitError, /length vs dimensionless/)
    end

    it 'checks CASE branches' do
      result = calculator.evaluate!('CASE x WHEN 1 THEN a ELSE b END',
        x: 2, a: { value: 1, unit: :m }, b: { value: 2, unit: :km })
      expect(result.value).to eq(2)
      expect(result.units).to eq(km: 1)

      expect {
        calculator.evaluate!('CASE x WHEN 1 THEN a ELSE b END',
          x: 1, a: { value: 1, unit: :m }, b: { value: 2, unit: :s })
      }.to raise_error(Dentaku::UnitError, /conditional branches/)
    end
  end

  describe 'custom functions' do
    it 'propagates quantities through custom functions' do
      calculator.add_function('double', :numeric, ->(x) { x * 2 })
      result = calculator.evaluate!('double(t)', t: { value: 5, unit: :s })
      expect(result.value).to eq(10)
      expect(result.units).to eq(s: 1)
    end

    it 'locates dimension errors inside custom functions at the call site' do
      calculator.add_function('bad', :numeric, ->(x) { x + 1 })
      error = nil
      begin
        calculator.evaluate!('bad(t)', t: { value: 5, unit: :s })
      rescue Dentaku::UnitError => e
        error = e
      end

      expect(error).not_to be_nil
      expect(error.message).to include('time vs dimensionless')
      expect(error.expression).to eq('BAD(t)')
    end
  end

  describe 'plain formulas' do
    it 'keeps original behavior without units' do
      expect(calculator.evaluate!('1 + 2 * 3')).to eq(7)
      expect(calculator.evaluate!('IF(1 > 0, 10, 20)')).to eq(10)
      expect(calculator.evaluate!('SUM(1, 2, 3)')).to eq(6)
    end

    it 'keeps original behavior when units mode is off' do
      calc = Dentaku::Calculator.new
      expect(calc.evaluate!('a + b', a: 1, b: 2)).to eq(3)
      expect(calc.evaluate!('a', a: { value: 1, unit: :m })).to eq(value: 1, unit: :m)
    end
  end
end
