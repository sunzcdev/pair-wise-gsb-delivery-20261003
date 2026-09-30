require 'spec_helper'
require 'dentaku'
require 'dentaku/calculator'

describe Dentaku::Calculator do
  describe '#evaluate_partial' do
    let(:calculator) { described_class.new }

    it 'folds the determined parts and reports the missing identifiers' do
      result = calculator.evaluate_partial('a + b * 2', b: 3)

      expect(result).not_to be_complete
      expect(result.expression).to eq('a + 6')
      expect(result.missing).to eq(['a'])
      expect(result.errors).to be_empty
    end

    it 'produces a residual that finishes the calculation once data arrives' do
      result = calculator.evaluate_partial('a + b * 2', b: 3)

      expect(calculator.evaluate(result.expression, a: 1))
        .to eq(calculator.evaluate('a + b * 2', a: 1, b: 3))
    end

    it 'preserves operator precedence in the residual expression' do
      result = calculator.evaluate_partial('(a + 1) * b', a: 2)

      expect(result.expression).to eq('3 * b')
      expect(calculator.evaluate(result.expression, b: 4)).to eq(12)
    end

    it 'keeps needed parentheses when a folded subtree sits under a tighter operator' do
      result = calculator.evaluate_partial('(a + b) * 2', b: 1)

      expect(result.expression).to eq('(a + 1) * 2')
      expect(calculator.evaluate(result.expression, a: 1)).to eq(4)
    end

    it 'folds big decimal results into re-parseable literals' do
      result = calculator.evaluate_partial('10 / 4 + x')

      expect(result.expression).to eq('2.5 + x')
      expect(calculator.evaluate(result.expression, x: 1)).to eq(BigDecimal('3.5'))
    end

    it 'is complete when nothing is missing' do
      result = calculator.evaluate_partial('1 + 2 * 3')

      expect(result).to be_complete
      expect(result.value).to eq(7)
      expect(result.missing).to be_empty
    end

    it 'distinguishes unknown identifiers, explicit nils, and evaluation errors' do
      result = calculator.evaluate_partial('a + 1 + b', a: nil)

      # a is provided (as nil), so it is not missing; b is unknown
      expect(result.missing).to eq(['b'])
      # nil + 1 is a determined-but-invalid operation
      expect(result.errors).to contain_exactly(an_instance_of(Dentaku::ArgumentError))
      expect(result.expression).to eq('NULL + 1 + b')
    end

    it 'reports evaluation errors in determined subtrees separately from missing inputs' do
      result = calculator.evaluate_partial('1 / 0 + x')

      expect(result.missing).to eq(['x'])
      expect(result.errors).to contain_exactly(an_instance_of(Dentaku::ZeroDivisionError))
      expect(result).not_to be_complete
      expect(result.expression).to eq('1 / 0 + x')
    end

    describe 'IF' do
      it 'only evaluates the selected branch' do
        calls = 0
        calculator.add_function(:probe, :numeric, ->(x) { calls += 1; x })

        result = calculator.evaluate_partial('IF(false, probe(1), probe(2) + x)')

        expect(calls).to eq(1)
        expect(result.expression).to eq('2 + x')
        expect(result.missing).to eq(['x'])
      end

      it 'does not execute a volatile function in the unselected branch' do
        calls = 0
        calculator.add_function(:probe, :numeric, ->(x) { calls += 1; x }, volatile: true)

        result = calculator.evaluate_partial('IF(true, 1, probe(2) + x)')

        expect(calls).to eq(0)
        expect(result).to be_complete
        expect(result.value).to eq(1)
      end

      it 'keeps both branches when the predicate is unknown' do
        result = calculator.evaluate_partial('IF(flag, 1 + 1, 2 + 2)')

        expect(result.expression).to eq('IF(flag, 2, 4)')
        expect(result.missing).to eq(['flag'])
        expect(calculator.evaluate(result.expression, flag: false)).to eq(4)
      end
    end

    describe 'short-circuiting' do
      it 'does not touch the right side of a decided AND' do
        calls = 0
        calculator.add_function(:probe, :numeric, ->(x) { calls += 1; x }, volatile: true)

        result = calculator.evaluate_partial('1 > 2 AND probe(1) > 0')

        expect(calls).to eq(0)
        expect(result).to be_complete
        expect(result.value).to eq(false)
      end

      it 'does not touch the right side of a decided OR' do
        calls = 0
        calculator.add_function(:probe, :numeric, ->(x) { calls += 1; x }, volatile: true)

        result = calculator.evaluate_partial('x OR 1 = 1')

        expect(calls).to eq(0)
        expect(result).to be_complete
        expect(result.value).to eq(true)
      end

      it 'drops a redundant constant operand' do
        result = calculator.evaluate_partial('x AND 1 = 1')

        expect(result.expression).to eq('x')
        expect(result.missing).to eq(['x'])
      end
    end

    describe 'function calls' do
      it 'never calls volatile functions during partial evaluation' do
        calls = 0
        calculator.add_function(:probe, :numeric, ->(x) { calls += 1; x }, volatile: true)

        result = calculator.evaluate_partial('probe(3) + 1 + y')

        expect(calls).to eq(0)
        expect(result.expression).to eq('PROBE(3) + 1 + y')
        expect(result.missing).to eq(['y'])

        expect(calculator.evaluate(result.expression, y: 10)).to eq(14)
        expect(calls).to eq(1)
      end

      it 'never calls more functions than a full evaluation' do
        calls = 0
        calculator.add_function(:tally, :numeric, ->(x) { calls += 1; x })
        formula = 'IF(tally(2) > 1, a * 2, 0) OR tally(3) > 100'

        result = calculator.evaluate_partial(formula)
        partial_value = calculator.evaluate(result.expression, a: 5)
        partial_calls = calls

        calls = 0
        full_value = calculator.evaluate(formula, a: 5)

        expect(partial_value).to eq(full_value)
        expect(partial_calls).to be <= calls
      end

      it 'calls a pure function with determined arguments exactly once' do
        calls = 0
        calculator.add_function(:tally, :numeric, ->(x) { calls += 1; x })

        result = calculator.evaluate_partial('tally(2) + x')

        expect(calls).to eq(1)
        expect(result.expression).to eq('2 + x')
      end
    end

    describe 'CASE' do
      it 'folds to the matching branch when the switch is known' do
        result = calculator.evaluate_partial('CASE 2 WHEN 1 THEN a WHEN 2 THEN b + 1 ELSE c END', b: 10)

        expect(result).to be_complete
        expect(result.value).to eq(11)
      end

      it 'keeps the whole expression when the switch is unknown' do
        result = calculator.evaluate_partial('CASE x WHEN 1 THEN a ELSE b END')

        expect(result.missing).to match_array(%w(x a b))
      end
    end

    it 'accepts a pre-parsed AST' do
      node = calculator.ast('a + 1')
      result = calculator.evaluate_partial(node)

      expect(result.missing).to eq(['a'])
      expect(result.expression).to eq('a + 1')
    end
  end
end
