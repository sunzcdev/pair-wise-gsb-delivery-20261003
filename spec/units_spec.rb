require 'spec_helper'
require 'dentaku'

describe 'unit dimension mode' do
  let(:calculator) { Dentaku::Calculator.new(units: true) }

  def quantity(value, unit)
    Dentaku::Units::Quantity.new(value, unit)
  end

  describe 'unit declarations' do
    it 'tags declared variables with their unit' do
      calculator.declare_units(t1: :s, t2: :ms)
      result = calculator.evaluate!('t1 + t2', t1: 2, t2: 500)
      expect(result).to eq quantity(2.5, :s)
    end

    it 'accepts quantity values directly, without declarations' do
      result = calculator.evaluate!('t1 + t2', t1: quantity(2, :s), t2: quantity(500, :ms))
      expect(result).to eq quantity(2.5, :s)
    end

    it 'requires units mode for declarations' do
      plain = Dentaku::Calculator.new
      expect {
        plain.declare_units(a: :s)
      }.to raise_error(Dentaku::Units::ModeError)
    end

    it 'raises for unknown units' do
      calculator.declare_units(a: :furlong)
      expect {
        calculator.evaluate!('a + 1', a: 1)
      }.to raise_error(Dentaku::Units::UnknownUnitError)
    end

    it 'leaves values for undeclared variables alone' do
      calculator.declare_units(a: :s)
      expect(calculator.evaluate!('a * b', a: 2, b: 3)).to eq quantity(6, :s)
    end
  end

  describe 'arithmetic' do
    it 'converts compatible units automatically on add/subtract' do
      expect(calculator.evaluate!('a + b', a: quantity(2, :s), b: quantity(500, :ms)))
        .to eq quantity(2.5, :s)
      expect(calculator.evaluate!('a - b', a: quantity(1, :km), b: quantity(250, :m)))
        .to eq quantity(0.75, :km)
    end

    it 'keeps the left operand unit in the result' do
      result = calculator.evaluate!('a + b', a: quantity(500, :ms), b: quantity(2, :s))
      expect(result.unit.label).to eq 'ms'
      expect(result.value).to eq 2500
    end

    it 'derives compound dimensions on multiply/divide' do
      speed = calculator.evaluate!('distance / duration',
                                   distance: quantity(120, :km), duration: quantity(2, :h))
      expect(speed.value).to eq 60
      expect(speed.unit.dimensions).to eq(length: 1, time: -1)
      expect(speed.base_value.to_f).to be_within(0.0001).of(16.6667)

      area = calculator.evaluate!('w * h', w: quantity(2, :m), h: quantity(3, :m))
      expect(area.unit.dimensions).to eq(length: 2)
    end

    it 'collapses cancelling dimensions to a plain dimensionless ratio' do
      ratio = calculator.evaluate!('elapsed / total',
                                   elapsed: quantity(500, :ms), total: quantity(2, :s))
      expect(ratio).to eq 0.25
      expect(ratio).not_to be_a(Dentaku::Units::Quantity)
    end

    it 'scales quantities by plain numbers' do
      expect(calculator.evaluate!('d * 2', d: quantity(1.5, :s))).to eq quantity(3, :s)
      expect(calculator.evaluate!('d / 2', d: quantity(1.5, :s))).to eq quantity(0.75, :s)
      expect(calculator.evaluate!('2 * d', d: quantity(1.5, :s))).to eq quantity(3, :s)
    end

    it 'rejects adding a dimensioned quantity to a plain number' do
      expect {
        calculator.evaluate!('d + 5', d: quantity(1, :s))
      }.to raise_error(Dentaku::Units::MismatchError, /time vs dimensionless/)
    end

    it 'supports exponentiation with dimensionless exponents' do
      result = calculator.evaluate!('a ^ 2', a: quantity(3, :m))
      expect(result.value).to eq 9
      expect(result.unit.dimensions).to eq(length: 2)
    end

    it 'rejects dimensioned exponents' do
      expect {
        calculator.evaluate!('a ^ b', a: quantity(3, :m), b: quantity(2, :s))
      }.to raise_error(Dentaku::Units::MismatchError)
    end

    it 'supports modulo with compatible units' do
      expect(calculator.evaluate!('a % b', a: quantity(2500, :ms), b: quantity(1, :s)))
        .to eq quantity(500, :ms)
    end
  end

  describe 'comparisons' do
    it 'compares compatible units after conversion' do
      expect(calculator.evaluate!('a > b', a: quantity(2, :s), b: quantity(500, :ms))).to be true
      expect(calculator.evaluate!('a = b', a: quantity(1000, :ms), b: quantity(1, :s))).to be true
      expect(calculator.evaluate!('a <= b', a: quantity(1, :km), b: quantity(500, :m))).to be false
    end

    it 'raises for incompatible comparisons' do
      expect {
        calculator.evaluate!('a > b', a: quantity(1, :m), b: quantity(1, :s))
      }.to raise_error(Dentaku::Units::MismatchError, /cannot compare.*in subexpression `a > b`/)
    end
  end

  describe 'error location' do
    it 'points at the innermost failing subexpression' do
      expect {
        calculator.evaluate!('1 * x + (length + duration) * 2',
                             x: quantity(1, :m), length: quantity(10, :m), duration: quantity(5, :s))
      }.to raise_error(Dentaku::Units::MismatchError, /in subexpression `length \+ duration`/)
    end

    it 'describes both dimensions and operands' do
      expect {
        calculator.evaluate!('length + duration', length: quantity(10, :m), duration: quantity(5, :s))
      }.to raise_error(Dentaku::Units::MismatchError, /cannot add 10 m and 5 s: incompatible dimensions \(length vs time\)/)
    end

    it 'returns nil from evaluate' do
      result = calculator.evaluate('a + b', a: quantity(1, :m), b: quantity(1, :s))
      expect(result).to be_nil
    end

    it 'yields the error to the evaluate block' do
      yielded = nil
      calculator.evaluate('a + b', { a: quantity(1, :m), b: quantity(1, :s) }) do |_expr, error|
        yielded = error
      end
      expect(yielded).to be_a(Dentaku::Units::MismatchError)
      expect(yielded.message).to match(/in subexpression `a \+ b`/)
    end
  end

  describe 'conditional branches' do
    it 'accepts compatible IF branches' do
      result = calculator.evaluate!('IF(ok, a, b)', ok: true, a: quantity(1, :s), b: quantity(500, :ms))
      expect(result).to eq quantity(1, :s)
    end

    it 'rejects incompatible IF branches' do
      expect {
        calculator.evaluate!('IF(ok, a, b)', ok: true, a: quantity(1, :s), b: quantity(500, :m))
      }.to raise_error(Dentaku::Units::MismatchError, /cannot return/)
    end

    it 'rejects a dimensioned branch against a plain one' do
      expect {
        calculator.evaluate!('IF(ok, a, 0)', ok: false, a: quantity(1, :s))
      }.to raise_error(Dentaku::Units::MismatchError, /dimensionless vs time/)
    end

    it 'does not evaluate guarded failing branches' do
      result = calculator.evaluate!('IF(x = 0, 0, 100 / x)', x: 0)
      expect(result).to eq 0
    end

    it 'accepts compatible CASE branches' do
      result = calculator.evaluate!(
        'CASE x WHEN 1 THEN a WHEN 2 THEN b ELSE c END',
        x: 2, a: quantity(1, :s), b: quantity(500, :ms), c: quantity(2, :s)
      )
      expect(result).to eq quantity(500, :ms)
    end

    it 'rejects incompatible CASE branches' do
      expect {
        calculator.evaluate!(
          'CASE x WHEN 1 THEN a WHEN 2 THEN b ELSE c END',
          x: 1, a: quantity(1, :s), b: quantity(500, :m), c: quantity(2, :s)
        )
      }.to raise_error(Dentaku::Units::MismatchError, /cannot return/)
    end
  end

  describe 'custom functions' do
    it 'tags plain numeric results with the declared unit' do
      calculator.add_function(:double, :numeric, ->(x) { x * 2 }, unit: :ms)
      result = calculator.evaluate!('double(150)')
      expect(result).to eq quantity(300, :ms)
    end

    it 'converts quantity results to the declared unit' do
      calculator.add_function(:total, :numeric, ->(a, b) { a + b }, unit: :ms)
      result = calculator.evaluate!('total(a, b)', a: quantity(1, :s), b: quantity(500, :ms))
      expect(result).to eq quantity(1500, :ms)
    end

    it 'rejects results incompatible with the declared unit' do
      calculator.add_function(:identity, :numeric, ->(x) { x }, unit: :s)
      expect {
        calculator.evaluate!('identity(d)', d: quantity(1, :m))
      }.to raise_error(Dentaku::Units::MismatchError, /cannot convert/)
    end

    it 'passes results through unchecked without a declared unit' do
      calculator.add_function(:identity, :numeric, ->(x) { x })
      expect(calculator.evaluate!('identity(d)', d: quantity(1, :m))).to eq quantity(1, :m)
    end

    it 'ignores the declared unit outside units mode' do
      plain = Dentaku::Calculator.new
      plain.add_function(:double, :numeric, ->(x) { x * 2 }, unit: :ms)
      expect(plain.evaluate!('double(150)')).to eq 300
    end
  end

  describe 'supported functions' do
    it 'propagates units through ABS, MIN, MAX, ROUND, SUM, AVG' do
      expect(calculator.evaluate!('ABS(a)', a: quantity(-2, :s))).to eq quantity(2, :s)
      expect(calculator.evaluate!('MIN(a, b)', a: quantity(2, :s), b: quantity(500, :ms)))
        .to eq quantity(500, :ms)
      expect(calculator.evaluate!('MAX(a, b)', a: quantity(2, :s), b: quantity(500, :ms)))
        .to eq quantity(2, :s)
      expect(calculator.evaluate!('ROUND(a, 1)', a: quantity(1.25, :m))).to eq quantity(1.3, :m)
      expect(calculator.evaluate!('SUM(a, b)', a: quantity(1, :s), b: quantity(500, :ms)))
        .to eq quantity(1.5, :s)
      expect(calculator.evaluate!('AVG(a, b)', a: quantity(1, :s), b: quantity(3, :s)))
        .to eq quantity(2, :s)
    end

    it 'raises when aggregating incompatible units' do
      expect {
        calculator.evaluate!('SUM(a, b)', a: quantity(1, :s), b: quantity(1, :m))
      }.to raise_error(Dentaku::Units::MismatchError)
    end
  end

  describe 'plain formulas' do
    it 'keeps original behavior without units' do
      expect(calculator.evaluate!('1 + 2 * 3')).to eq 7
      expect(calculator.evaluate!('IF(1 < 2, 10, 20)')).to eq 10
      expect(calculator.evaluate!('MAX(1, 2, 3)')).to eq 3
      expect(calculator.evaluate!('SUM(1, 2, 3)')).to eq 6
      expect(calculator.evaluate!('AVG(2, 4)')).to eq 3
      expect(calculator.evaluate!('10 % 3')).to eq 1
    end

    it 'keeps original behavior on calculators without units mode' do
      plain = Dentaku::Calculator.new
      expect(plain.evaluate!('1 + 2 * 3')).to eq 7
      expect(plain.evaluate!('ROUND(1.25, 1)')).to eq BigDecimal('1.3')
    end
  end
end
