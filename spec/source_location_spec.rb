require 'spec_helper'
require 'dentaku'
require 'dentaku/tokenizer'

describe 'source locations' do
  let(:calculator) { Dentaku::Calculator.new }

  def evaluate_error(formula, *args)
    calculator.evaluate!(formula, *args)
    nil
  rescue ::StandardError => e
    e
  end

  describe Dentaku::SourceRange do
    it 'extracts the marked text from the original expression' do
      range = described_class.new(2, 5)
      expect(range.extract('abcdef')).to eq('cde')
      expect(range.length).to eq(3)
      expect(range.to_range).to eq(2...5)
    end
  end

  describe 'tokenizer errors' do
    it 'points at the character that cannot be tokenized' do
      error = evaluate_error('1 + $')
      expect(error).to be_a(Dentaku::TokenizerError)
      expect(error.source_range.extract('1 + $')).to eq('$')
    end

    it 'points at an extra closing parenthesis' do
      formula = '(1 + 2))'
      error = evaluate_error(formula)
      expect(error).to be_a(Dentaku::TokenizerError)
      expect(error.source_range.extract(formula)).to eq(')')
      expect(error.source_range.start_offset).to eq(7)
    end

    it 'is not shifted by comments, even across lines' do
      formula = "1 /* comment\nspanning lines */ + $"
      error = evaluate_error(formula)
      expect(error.source_range.extract(formula)).to eq('$')
      expect(error.source_range.start_offset).to eq(formula.index('$'))
    end

    it 'maps offsets back through alias expansion' do
      calc = Dentaku::Calculator.new(aliases: { 'round' => %w(四舍五入) })
      formula = '四舍五入(1.5) + $'
      begin
        calc.evaluate!(formula)
        raise 'expected a TokenizerError'
      rescue Dentaku::TokenizerError => e
        expect(e.source_range.extract(formula)).to eq('$')
        expect(e.source_range.start_offset).to eq(formula.index('$'))
      end
    end

    it 'maps a function introduced by an alias back to the alias text' do
      calc = Dentaku::Calculator.new(aliases: { 'round' => %w(rd) })
      formula = 'rd(1.5, 2, 3)'
      begin
        calc.evaluate!(formula)
        raise 'expected a ParseError'
      rescue Dentaku::ParseError => e
        expect(e.source_range.extract(formula)).to start_with('rd')
      end
    end
  end

  describe 'parse errors' do
    it 'marks the name of an undefined function' do
      formula = '1 + nope(2)'
      error = evaluate_error(formula)
      expect(error).to be_a(Dentaku::ParseError)
      expect(error.source_range.extract(formula)).to eq('nope')
    end

    it 'marks the operator with too few operands' do
      formula = '1 +'
      error = evaluate_error(formula)
      expect(error).to be_a(Dentaku::ParseError)
      expect(error.source_range.extract(formula)).to eq('+')
    end
  end

  describe 'evaluation errors' do
    it 'marks the exact subexpression that divided by zero' do
      formula = '10 + 1 / 0'
      error = evaluate_error(formula)
      expect(error).to be_a(Dentaku::ZeroDivisionError)
      expect(error.source_range.extract(formula)).to eq('1 / 0')
    end

    it 'marks unbound variables' do
      formula = 'abc + 1'
      error = evaluate_error(formula)
      expect(error).to be_a(Dentaku::UnboundVariableError)
      expect(error.source_range.extract(formula)).to eq('abc')
    end

    it 'keeps offsets correct for Chinese identifiers in multi-line formulas' do
      formula = "价格 * 2 +\n数量 / 0"
      error = evaluate_error(formula, 价格: 3, 数量: 1)
      expect(error).to be_a(Dentaku::ZeroDivisionError)
      expect(error.source_range.extract(formula)).to eq('数量 / 0')
      expect(error.source_range.start_offset).to eq(formula.index('数量'))
    end

    it 'keeps offsets correct after escaped strings' do
      formula = 'IF("a\"b" = "a\"b", 1 / 0, 0)'
      error = evaluate_error(formula)
      expect(error).to be_a(Dentaku::ZeroDivisionError)
      expect(error.source_range.extract(formula)).to eq('1 / 0')
      expect(error.source_range.start_offset).to eq(formula.index('1 / 0'))
    end

    it 'tokenizes escaped quotes as a single string with an exact range' do
      formula = 'animal = "a\"b"'
      tokens = Dentaku::Tokenizer.new.tokenize(formula)
      string = tokens.find { |t| t.category == :string }
      expect(string.value).to eq('a"b')
      expect(string.source_range.extract(formula)).to eq('"a\"b"')
    end

    it 'exposes the error with its range to the evaluate block' do
      formula = '1 / 0'
      seen = nil
      calculator.evaluate(formula) { |_expr, error| seen = error }
      expect(seen).to be_a(Dentaku::ZeroDivisionError)
      expect(seen.source_range.extract(formula)).to eq('1 / 0')
    end
  end

  describe 'custom functions' do
    it 'marks which call failed, even when the same call appears twice' do
      calculator.add_function(:reciprocal, :numeric, ->(x) { raise 'division by zero' if x.zero?; 1.0 / x })
      formula = 'reciprocal(4) + reciprocal(0)'

      error = evaluate_error(formula)
      expect(error).not_to be_nil

      # the marked range selects the second call and only the second call
      expect(error.source_range.extract(formula)).to eq('reciprocal(0)')
      expect(formula.scan('reciprocal').length).to eq(2)
      expect(error.source_range.start_offset).to eq(formula.rindex('reciprocal'))
      expect(error.source_range.start_offset).not_to eq(formula.index('reciprocal'))
    end

    it 'records the full chain of nested call sites, innermost first' do
      calculator.add_function(:inner, :numeric, ->(x) { raise 'too big' if x > 5; x })
      calculator.add_function(:outer, :numeric, ->(x) { x })
      formula = 'outer(inner(9))'

      error = evaluate_error(formula)
      expect(error.source_range.extract(formula)).to eq('inner(9)')
      expect(error.call_trace.map { |range| range.extract(formula) })
        .to eq(['inner(9)', 'outer(inner(9))'])
    end

    it 'does not include argument values in the error result' do
      calculator.add_function(:lookup, :string, ->(_key) { raise Dentaku::ArgumentError.for(:invalid_value) })
      formula = 'lookup("s3cr3t-token")'

      error = evaluate_error(formula)
      expect(error.message).not_to include('s3cr3t-token')
      expect(error.source_range.extract(formula)).to eq('lookup("s3cr3t-token")')
    end

    it 'attaches the call site to foreign exceptions without changing their class' do
      calculator.add_function(:shout, :string, ->(word) { word.upcase! })
      formula = 'shout(22)'

      error = evaluate_error(formula)
      expect(error).to be_a(NoMethodError)
      expect(error.source_range.extract(formula)).to eq('shout(22)')
    end
  end

  describe Dentaku::MathDomainError do
    it 'does not retain the argument values passed to the function' do
      error = evaluate_error('asin(2)')
      expect(error).to be_a(described_class)
      expect(error.function_name).to eq('asin')
      expect(error.arg_count).to eq(1)
      expect(error).not_to respond_to(:args)
      expect(error.source_range.extract('asin(2)')).to eq('asin(2)')
    end
  end
end
