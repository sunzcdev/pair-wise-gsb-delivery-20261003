require 'spec_helper'
require 'dentaku/ast/functions/let'
require 'dentaku'

describe Dentaku::AST::Let do
  let(:calculator) { Dentaku::Calculator.new }

  it 'binds a repeated subexpression to a local name' do
    result = Dentaku('LET(total, price * quantity, total - total * discount)',
                     price: 10, quantity: 3, discount: 0.5)
    expect(result).to eq(15)
  end

  it 'makes earlier definitions visible to later ones' do
    expect(calculator.evaluate!('LET(a, 1, b, a + 1, a + b)')).to eq(3)
  end

  it 'resolves not-yet-defined names against the outer scope' do
    expect(calculator.evaluate!('LET(a, b, b, 2, a)', b: 10)).to eq(10)
  end

  it 'raises for a not-yet-defined name missing from the outer scope' do
    expect {
      calculator.evaluate!('LET(a, b, b, 2, a)')
    }.to raise_error(Dentaku::UnboundVariableError)
  end

  it 'lets a later duplicate binding shadow the earlier one' do
    expect(calculator.evaluate!('LET(a, 1, a, 2, a)')).to eq(2)
  end

  it 'supports nesting and shadowing of outer locals' do
    expect(calculator.evaluate!('LET(a, 1, LET(a, 2, a) + a)')).to eq(3)
  end

  it 'shadows a same-named external variable without touching memory' do
    calculator.store(x: 5)
    expect(calculator.evaluate!('LET(x, 10, x + 1)')).to eq(11)
    expect(calculator.memory).to eq('x' => 5)
  end

  it 'does not leak bindings into calculator memory' do
    calculator.evaluate!('LET(temp, 42, temp * 2)')
    expect(calculator.memory).to be_empty
  end

  it 'leaves memory unchanged when an inner expression fails' do
    calculator.store(x: 5)

    expect {
      calculator.evaluate!('LET(x, 10, x + missing)')
    }.to raise_error(Dentaku::UnboundVariableError)
    expect(calculator.memory).to eq('x' => 5)

    expect(calculator.evaluate('LET(x, 10, x / 0)')).to be_nil
    expect(calculator.memory).to eq('x' => 5)
  end

  it 'excludes local names from dependency analysis' do
    expect(calculator.dependencies('LET(a, 1, a + b)')).to eq(%w[b])
    expect(calculator.dependencies('LET(a, x, b, a + y, b)')).to eq(%w[x y])
    expect(calculator.identifiers('LET(a, 1, a + b)')).to eq(%w[b])
  end

  it 'counts a name referenced before its definition as a dependency' do
    expect(calculator.dependencies('LET(a, b, b, 2, a)')).to eq(%w[b])
  end

  it 'rejects a binding list without a body expression' do
    expect {
      calculator.evaluate!('LET(a, 1, b, 2)')
    }.to raise_error(Dentaku::ParseError, /name\/value pairs/)
  end

  it 'rejects non-identifier binding names' do
    expect {
      calculator.evaluate!('LET(1, 2, 3)')
    }.to raise_error(Dentaku::ParseError, /identifiers/)
  end
end
