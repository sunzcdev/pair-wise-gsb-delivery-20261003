require 'spec_helper'
require 'dentaku'
require 'dentaku/calculator'

# Errors raised while tokenizing, parsing, and evaluating a formula must
# carry the character range (into the user's original formula text) that
# caused them, so a formula editor can highlight the offending source.
RSpec.describe 'source locations on errors' do
  let(:calculator) { Dentaku::Calculator.new }

  def error_for(formula, data = {})
    calculator.evaluate!(formula, data)
    nil
  rescue Dentaku::Error => e
    e
  end

  describe 'tokenizer errors' do
    it 'locates an unparseable character' do
      formula = '1 + $'
      error = error_for(formula)
      expect(error).to be_a(Dentaku::TokenizerError)
      expect(error.location).to eq(4...5)
      expect(formula[error.location]).to eq('$')
    end

    it 'locates an unbalanced opening parenthesis' do
      error = error_for('(1 + 2')
      expect(error).to be_a(Dentaku::TokenizerError)
      expect(error.location).to eq(0...1)
    end

    it 'locates an unbalanced closing parenthesis' do
      error = error_for('1 + 2)')
      expect(error).to be_a(Dentaku::TokenizerError)
      expect(error.location).to eq(5...6)
    end

    it 'reports the error to the evaluate block with a location' do
      yielded = nil
      calculator.evaluate('1 + $') { |_expression, error| yielded = error }
      expect(yielded).to be_a(Dentaku::TokenizerError)
      expect(yielded.location).to eq(4...5)
    end
  end

  describe 'parse errors' do
    it 'locates an undefined function name' do
      formula = 'nosuchfn(1) + 2'
      error = error_for(formula)
      expect(error).to be_a(Dentaku::ParseError)
      expect(error.location).to eq(0...8)
      expect(formula[error.location]).to eq('nosuchfn')
    end

    it 'locates an operand with an incompatible type' do
      formula = "1 + 'a'"
      error = error_for(formula)
      expect(error).to be_a(Dentaku::ParseError)
      expect(error.location).to eq(4...7)
      expect(formula[error.location]).to eq("'a'")
    end

    it 'locates a missing operand at the last token' do
      error = error_for('1 +')
      expect(error).to be_a(Dentaku::ParseError)
      expect(error.location).to eq(2...3)
    end
  end

  describe 'evaluation errors' do
    it 'locates an unbound identifier' do
      formula = 'false or x'
      node = calculator.ast(formula)
      expect { node.value({}) }.to raise_error(Dentaku::UnboundVariableError) { |error|
        expect(error.location).to eq(9...10)
        expect(formula[error.location]).to eq('x')
      }
    end

    it 'locates a division by zero' do
      formula = '10 / (5 - 5)'
      error = error_for(formula)
      expect(error).to be_a(Dentaku::ZeroDivisionError)
      expect(error.location).to eq(0...12)
      expect(formula[error.location]).to eq('10 / (5 - 5)')
    end

    it 'keeps the full chain of nested expression locations' do
      formula = '1 + 10 / (5 - 5)'
      error = error_for(formula)
      expect(error).to be_a(Dentaku::ZeroDivisionError)
      expect(error.location).to eq(4...16)
      expect(formula[error.location]).to eq('10 / (5 - 5)')
      expect(error.trace.first).to eq(4...16)
      expect(error.trace.last).to eq(0...16)
      expect(formula[error.trace.last]).to eq(formula)
    end
  end

  describe 'rewritten formulas' do
    it 'maps positions through alias expansion' do
      calc = Dentaku::Calculator.new(aliases: { 'sum' => ['total'] })
      formula = 'total(1, x)'
      node = calc.ast(formula)
      expect { node.value({}) }.to raise_error(Dentaku::UnboundVariableError) { |error|
        expect(error.location).to eq(9...10)
        expect(formula[error.location]).to eq('x')
      }
    end

    it 'maps positions inside aliased function calls' do
      calc = Dentaku::Calculator.new(aliases: { 'sum' => ['total'] })
      formula = 'total(1, 1/0)'
      error = nil
      begin
        calc.evaluate!(formula)
      rescue Dentaku::Error => e
        error = e
      end
      expect(error).to be_a(Dentaku::ZeroDivisionError)
      expect(error.location).to eq(9...12)
      expect(formula[error.location]).to eq('1/0')
    end

    it 'maps positions through stripped comments' do
      formula = '1 + /* hi */ x'
      node = calculator.ast(formula)
      expect { node.value({}) }.to raise_error(Dentaku::UnboundVariableError) { |error|
        expect(error.location).to eq(13...14)
        expect(formula[error.location]).to eq('x')
      }
    end

    it 'gives tokens their original ranges after alias expansion' do
      tokenizer = Dentaku::Tokenizer.new
      tokens = tokenizer.tokenize('total(1)', aliases: { 'sum' => ['total'] })
      expect(tokens.first.location).to eq(0...5)
      expect('total(1)'[tokens.first.location]).to eq('total')
    end
  end

  describe 'multibyte and multiline formulas' do
    it 'locates Chinese identifiers by character offset, not byte offset' do
      formula = '价格 * 数量'
      node = calculator.ast(formula)
      expect { node.value('价格' => 2) }.to raise_error(Dentaku::UnboundVariableError) { |error|
        expect(error.location).to eq(5...7)
        expect(formula[error.location]).to eq('数量')
      }
    end

    it 'computes offsets after strings containing escapes from the raw source' do
      formula = %q{"a\nb" = x}
      node = calculator.ast(formula)
      expect { node.value({}) }.to raise_error(Dentaku::UnboundVariableError) { |error|
        expect(error.location).to eq(9...10)
        expect(formula[error.location]).to eq('x')
      }
    end

    it 'tracks positions across newlines' do
      formula = "1 +\n\nx"
      node = calculator.ast(formula)
      expect { node.value({}) }.to raise_error(Dentaku::UnboundVariableError) { |error|
        expect(error.location).to eq(5...6)
        expect(formula[error.location]).to eq('x')
      }
    end
  end

  describe 'custom function errors' do
    it 'pinpoints the failing call among two identical calls' do
      calls = 0
      calculator.add_function(:flaky, :numeric, ->(x) { calls += 1; raise 'boom' if calls > 1; x })

      formula = 'flaky(1) + flaky(1)'
      error = error_for(formula)

      expect(error).to be_a(Dentaku::FunctionError)
      expect(error.location).to eq(11...19)
      expect(formula[error.location]).to eq('flaky(1)')

      # the range must uniquely select the second call, not the first
      occurrences = formula.enum_for(:scan, 'flaky(1)').map { Regexp.last_match.begin(0) }
      expect(occurrences).to eq([0, 11])
      expect(error.location.begin).to eq(occurrences.last)
      expect(error.trace.first).to eq(error.location)
    end

    it 'does not leak argument values into the error result' do
      calculator.add_function(:store_secret, :string, ->(value) { raise "digest failed for #{value}" })

      formula = 'store_secret(password)'
      error = error_for(formula, password: 's3cret-p4ssword')

      expect(error).to be_a(Dentaku::FunctionError)
      expect(error.message).not_to include('s3cret-p4ssword')
      expect(error.message).to include('store_secret')
      expect(error.location).to eq(0...22)
      expect(formula[error.location]).to eq('store_secret(password)')
      # the original failure stays reachable programmatically
      expect(error.original.message).to include('s3cret-p4ssword')
    end
  end
end
