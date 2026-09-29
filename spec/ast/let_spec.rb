require 'spec_helper'
require 'dentaku/ast/functions/let'
require 'dentaku'

describe Dentaku::AST::Let do
  let(:calculator) { Dentaku::Calculator.new }

  it 'binds a name for the body expression only' do
    expect(calculator.evaluate!('LET(x, 2, x * x + x)')).to eq(6)
  end

  it 'avoids repeating a shared subexpression' do
    result = calculator.evaluate!('LET(s, a + b, s * s - s)', a: 1, b: 2)
    expect(result).to eq(6)
  end

  it 'makes earlier bindings visible to later value expressions' do
    expect(calculator.evaluate!('LET(x, 2, y, x * 3, x + y)')).to eq(8)
  end

  it 'resolves not-yet-bound names against the outer scope' do
    expect(calculator.evaluate!('LET(y, x + 1, x, 2, y)', x: 10)).to eq(11)
  end

  it 'raises for a not-yet-bound name missing from the outer scope' do
    expect { calculator.evaluate!('LET(y, x + 1, x, 2, y)') }.to raise_error(
      Dentaku::UnboundVariableError
    )
  end

  it 'lets a later binding shadow an earlier binding of the same name' do
    expect(calculator.evaluate!('LET(x, 1, x, x + 10, x)')).to eq(11)
  end

  it 'shadows an external variable only inside the body' do
    expect(calculator.evaluate!('LET(x, 5, x + 1) + x', x: 100)).to eq(106)
  end

  it 'supports nesting with shadowing' do
    expect(calculator.evaluate!('LET(x, 1, LET(x, 2, x * 10) + x)')).to eq(21)
  end

  it 'does not write bindings back to calculator memory' do
    calculator.store(x: 1)
    expect(calculator.evaluate!('LET(y, 40, x + y + 1)')).to eq(42)
    expect(calculator.memory).to eq('x' => 1)
  end

  it 'leaves calculator memory untouched when a shadowing body fails' do
    calculator.store(x: 7)
    expect { calculator.evaluate!('LET(x, 0, 1 / x)') }.to raise_error(
      Dentaku::ZeroDivisionError
    )
    expect(calculator.memory).to eq('x' => 7)
  end

  it 'excludes local bindings from dependency analysis' do
    expression = 'LET(x, a + 1, y, x * 2, x + y + b)'
    expect(calculator.dependencies(expression)).to match_array(%w[a b])
    expect(calculator.identifiers(expression)).to match_array(%w[a b])
  end

  it 'does not treat a stored variable shadowed by a binding as a dependency' do
    calculator.store(a: 1)
    expect(calculator.dependencies('LET(a, 5, a + b)')).to eq(%w[b])
  end

  it 'requires name/value pairs followed by a body expression' do
    expect { calculator.evaluate!('LET(x, 1, x, 2)') }.to raise_error(
      Dentaku::ParseError,
      'LET() requires name/value binding pairs followed by a body expression'
    )
  end

  it 'requires binding names to be identifiers' do
    expect { calculator.evaluate!('LET(1, 2, 3)') }.to raise_error(
      Dentaku::ParseError,
      'LET() requires binding names to be identifiers'
    )
  end
end
