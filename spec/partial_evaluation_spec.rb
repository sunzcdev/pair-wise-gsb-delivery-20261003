require 'spec_helper'
require 'dentaku'
require 'dentaku/calculator'

describe Dentaku::Calculator, '#partially_evaluate' do
  let(:calculator) { described_class.new }

  def partial(expr, data = {})
    calculator.partially_evaluate(expr, data)
  end

  describe 'result object' do
    it 'returns a PartialEvaluation with residual, missing inputs and errors' do
      result = partial('1 + 2 + x')
      expect(result).to be_a(Dentaku::PartialEvaluation)
      expect(result.residual).to be_a(Dentaku::AST::Node)
      expect(result.missing_variables).to eq(%w(x))
      expect(result.errors).to eq([])
      expect(result.resolvable?).to be(false)
    end

    it 'folds the already computable part of an expression' do
      expect(partial('(1 + 2) * 3 + x').to_s).to eq('9 + x')
    end

    it 'folds to a single value when every input is present' do
      result = partial('2 * (3 + 4)', {})
      expect(result.resolvable?).to be(true)
      expect(result.value).to eq(14)
    end

    it 'raises UnboundVariableError from #value while inputs are missing' do
      result = partial('a + b', 'a' => 1)
      expect { result.value }.to raise_error(Dentaku::UnboundVariableError) do |error|
        expect(error.unbound_variables).to eq(%w(b))
      end
    end
  end

  describe 'unknown values, nil and errors' do
    it 'keeps unbound identifiers as variables and reports them' do
      result = partial('a + b * c', 'b' => 2)
      expect(result.to_s).to eq('a + 2 * c')
      expect(result.missing_variables).to match_array(%w(a c))
    end

    it 'treats an explicitly supplied nil as NULL, not a missing input' do
      result = partial('IF(a = NULL, b, c)', 'a' => nil)
      expect(result.to_s).to eq('b')
      expect(result.missing_variables).to eq(%w(b))
      expect(result.missing_variables).not_to include('a')
    end

    it 'does not list a variable bound to nil as missing in an arithmetic residual' do
      result = partial('a + 1', 'a' => nil)
      expect(result.missing_variables).to eq([])
      expect(result.errors).not_to be_empty
      expect(result.to_s).to eq('NULL + 1')
    end

    it 'collects evaluation errors separately instead of raising' do
      result = partial('a / b', 'a' => 1, 'b' => 0)
      expect(result.missing_variables).to eq([])
      expect(result.errors.first).to be_a(Dentaku::ZeroDivisionError)
      expect(result.resolvable?).to be(false)
    end

    it 'keeps an erroring subtree in the residual so it stays recomputable' do
      result = partial('(a / 0) + b', 'a' => 1)
      expect(result.to_s).to eq('1 / 0 + b')
      expect(result.errors.map(&:class)).to include(Dentaku::ZeroDivisionError)
      expect(result.missing_variables).to eq(%w(b))
    end

    it 'raises the recorded error from #value' do
      result = partial('1 / 0')
      expect { result.value }.to raise_error(Dentaku::ZeroDivisionError)
    end
  end

  describe 'residual expressions' do
    it 'prints a residual expression that can be parsed and evaluated again' do
      result = partial('1 + 2 + x')
      expect(result.to_s).to eq('3 + x')
      expect(calculator.evaluate!(result.to_s, 'x' => 10)).to eq(13)
    end

    it 'accepts the residual AST node directly for continued evaluation' do
      result = partial('a + 5', {})
      continued = calculator.partially_evaluate(result.residual, 'a' => 37)
      expect(continued.value).to eq(42)
    end

    it 'preserves precedence when collapsing a selected branch' do
      formula = 'IF(flag, (base + 1) * 2, 99)'
      result = partial(formula, 'flag' => true)
      expect(result.to_s).to eq('(base + 1) * 2')

      expect(calculator.evaluate!(result.to_s, 'base' => 10)).to eq(22)
      expect(calculator.evaluate!(formula, 'flag' => true, 'base' => 10)).to eq(22)
    end

    it 'preserves precedence when collapsing a CASE branch' do
      formula = 'CASE kind WHEN 1 THEN a + 1 WHEN 2 THEN b ELSE c END'
      result = partial(formula, 'kind' => 1)
      expect(result.to_s).to eq('a + 1')
      expect(calculator.evaluate!(result.to_s, 'a' => 41)).to eq(42)
    end

    it 'works with an array of expressions' do
      results = calculator.partially_evaluate(['1 + x', '2 + 3'], 'x' => 9)
      expect(results.map(&:value)).to eq([10, 5])
    end
  end

  describe 'IF branches' do
    it 'folds the predicate and reduces only the selected branch' do
      formula = 'IF(x > 0, a + 1, b + 2)'
      expect(partial(formula, 'x' => 5, 'a' => 41).value).to eq(42)
      expect(partial(formula, 'x' => -5, 'b' => 40).value).to eq(42)
    end

    it 'keeps both branches when the predicate is unknown, folding pure arithmetic only' do
      result = partial('IF(p, a + 1 + 2, b + 10)', {})
      expect(result.to_s).to eq('IF(p, a + 1 + 2, b + 10)')
      expect(result.missing_variables).to match_array(%w(p a b))
    end

    it 'does not report variables belonging only to the discarded branch' do
      result = partial('IF(x > 0, a, unused)', 'x' => 1)
      expect(result.missing_variables).to eq(%w(a))
      expect(result.missing_variables).not_to include('unused')
    end
  end

  describe 'short-circuit combinators' do
    it 'short circuits AND when the left operand is false' do
      expect(partial('false and x').to_s).to eq('false')
      expect(partial('a and b', 'a' => false).to_s).to eq('false')
    end

    it 'short circuits OR when the left operand is true' do
      expect(partial('true or x').to_s).to eq('true')
      expect(partial('a or b', 'a' => true).to_s).to eq('true')
    end

    it 'reduces the right operand only when the left cannot decide' do
      result = partial('a and (b + 1 > 2)', {})
      expect(result.to_s).to eq('a and b + 1 > 2')
      expect(result.missing_variables).to match_array(%w(a b))
    end

    it 'does not report a right-hand variable hidden by short circuit' do
      result = partial('a or unreachable', 'a' => true)
      expect(result.missing_variables).to eq([])
    end
  end

  describe 'CASE expressions' do
    it 'reduces to the matched when clause' do
      formula = 'CASE k WHEN 1 THEN a WHEN 2 THEN b + 1 ELSE c END'
      expect(partial(formula, 'k' => 2).to_s).to eq('b + 1')
    end

    it 'reduces to the else clause when every guard is known and misses' do
      formula = 'CASE k WHEN 1 THEN a ELSE c + 10 END'
      expect(partial(formula, 'k' => 9).to_s).to eq('c + 10')
    end

    it 'keeps every clause while the switch is unknown' do
      result = partial('CASE k WHEN 1 THEN a ELSE b END', {})
      expect(result.to_s).to eq('CASE k WHEN 1 THEN a ELSE b END')
      expect(result.missing_variables).to match_array(%w(k a b))
    end
  end

  describe 'function call invariance' do
    let(:calls) { Hash.new(0) }

    def make_calculator
      counter = calls
      calculator.add_function(:mycount, :numeric, ->(*args) {
        counter[:mycount] += 1
        args.length
      })
    end

    it 'never calls a function whose arguments are not fully known' do
      make_calculator
      result = partial('MYCOUNT(1, 2, x) + 100')
      expect(calls[:mycount]).to eq(0)
      expect(result.to_s).to eq('MYCOUNT(1, 2, x) + 100')
    end

    it 'calls a fully determined function exactly once' do
      make_calculator
      result = partial('MYCOUNT(1, 2, 3)')
      expect(calls[:mycount]).to eq(1)
      expect(result.value).to eq(3)
    end

    it 'matches the number of calls a complete evaluation of the same data makes' do
      make_calculator
      formula = 'IF(flag, MYCOUNT(1, 2), MYCOUNT(a, b, c)) + MYCOUNT(9)'

      full_calls = { mycount: 0 }
      full_calculator = described_class.new
      full_calculator.add_function(:mycount, :numeric, ->(*args) {
        full_calls[:mycount] += 1
        args.length
      })
      data = { 'flag' => true, 'a' => 1, 'b' => 2, 'c' => 3 }
      full_calculator.evaluate!(formula, data)

      partial(formula, data)
      expect(calls[:mycount]).to eq(full_calls[:mycount])
    end

    it 'does not execute the unselected IF branch even when it contains a function' do
      make_calculator
      result = partial('IF(flag, 1, MYCOUNT(x))', 'flag' => true)
      expect(calls[:mycount]).to eq(0)
      expect(result.to_s).to eq('1')
    end

    it 'does not execute the right operand of a short-circuited OR' do
      make_calculator
      result = partial('flag or MYCOUNT(x)', 'flag' => true)
      expect(calls[:mycount]).to eq(0)
      expect(result.to_s).to eq('true')
    end

    it 'does not call a volatile function while folding' do
      counter = { n: 0 }
      calculator.add_function(:vol, :numeric, ->(x) { counter[:n] += 1; x }, volatile: true)
      result = partial('VOL(1 + 2) + 10')
      expect(counter[:n]).to eq(0)
      expect(result.to_s).to eq('VOL(3) + 10')
    end

    it 'continues the residual with exactly one call after data is supplied' do
      make_calculator
      result = partial('MYCOUNT(1, x)')
      expect(calls[:mycount]).to eq(0)

      finished = calculator.partially_evaluate(result.residual, 'x' => 2)
      expect(calls[:mycount]).to eq(1)
      expect(finished.value).to eq(2)
    end

    it 'does not fold a pure custom function nested in an unselected branch' do
      make_calculator
      result = partial('IF(flag, 1, MYCOUNT(MYCOUNT(9)))', 'flag' => true)
      expect(calls[:mycount]).to eq(0)
      expect(result.to_s).to eq('1')
    end

    it 'never evaluates a selected pure guard more often than a complete evaluation' do
      make_calculator
      formula = 'IF(MYCOUNT(1) > 0, t, u)'

      full_counter = { mycount: 0 }
      full_calculator = described_class.new
      full_calculator.add_function(:mycount, :numeric, ->(*args) {
        full_counter[:mycount] += 1
        args.length
      })
      full_calculator.evaluate!(formula, 't' => 5, 'u' => 6)

      partial(formula, 't' => 5, 'u' => 6)
      expect(calls[:mycount]).to be <= full_counter[:mycount]
      expect(calls[:mycount]).to eq(1)
    end

    it 'preserves both a missing input and an evaluation error' do
      make_calculator
      result = partial('MYCOUNT(1) / 0 + missing')
      expect(calls[:mycount]).to eq(1)
      expect(result.missing_variables).to eq(%w(missing))
      expect(result.errors.map(&:class)).to include(Dentaku::ZeroDivisionError)
      expect(result.to_s).to eq('1 / 0 + missing')
    end
  end

  describe 'collections and access' do
    it 'folds a constant array literal' do
      expect(partial('{1, 2, 3}[1] + 5').value).to eq(7)
    end

    it 'leaves array access with an unknown index as a residual' do
      result = partial('{10, 20, 30}[i]', {})
      expect(result.missing_variables).to eq(%w(i))
      expect(result.to_s).to eq('{10, 20, 30}[i]')
    end

    it 'folds enum functions only when the collection and body are determined' do
      expect(partial('SUM({1, 2, 3})').value).to eq(6)
      expect(partial('MAP({1, 2, 3}, x, x * 2)').value).to eq([2, 4, 6])

      result = partial('MAP(items, x, x * 2)', {})
      expect(result.missing_variables).to eq(%w(items))

      result = partial('FILTER({1, 2, 3, 4}, x, x > bound)', {})
      expect(result.missing_variables).to eq(%w(bound))
      expect(partial('FILTER({1, 2, 3, 4}, x, x > 2)').value).to eq([3, 4])
    end
  end

  describe 'stored formulas and values' do
    it 'inlines a stored formula and keeps its unknown inputs in the residual' do
      calculator.store('base', calculator.ast('10 + y'))
      result = partial('base * 2')
      expect(result.to_s).to eq('(10 + y) * 2')
      expect(result.missing_variables).to eq(%w(y))
      expect(calculator.evaluate!(result.to_s, 'y' => 5)).to eq(30)
    end

    it 'folds through a stored formula that can be fully resolved' do
      calculator.store('h', calculator.ast('2 + 3'))
      expect(partial('h * 2').value).to eq(10)
    end

    it 'does not execute the unselected branch of a stored formula' do
      calls = 0
      calculator.add_function(:cf, :numeric, ->(x) { calls += 1; x })
      calculator.store('g', calculator.ast('IF(flag, CF(1), CF(2))'))
      result = partial('g + 1', 'flag' => true)
      expect(calls).to eq(1)
      expect(result.value).to eq(2)
    end

    it 'prints a folded date as a parseable date literal' do
      require 'date'
      result = partial('d', 'd' => Date.new(2020, 1, 1))
      expect(result.to_s).to eq('2020-01-01')
      reparsed = calculator.evaluate(result.to_s)
      expect(reparsed).to be_a(DateTime)
      expect(reparsed.strftime('%Y-%m-%d')).to eq('2020-01-01')
    end
  end

  describe 'module-level entry point' do
    it 'exposes Dentaku.partially_evaluate' do
      result = Dentaku.partially_evaluate('1 + x')
      expect(result).to be_a(Dentaku::PartialEvaluation)
      expect(result.missing_variables).to eq(%w(x))
    end
  end
end
