require 'spec_helper'
require 'dentaku/exceptions'

describe Dentaku::UnboundVariableError do
  it 'includes variable name(s) in message' do
    exception = described_class.new(['length'])
    expect(exception.unbound_variables).to include('length')
  end
end

describe Dentaku::Error do
  it 'is included in every Dentaku exception, even those subclassing Ruby built-ins' do
    [
      Dentaku::UnboundVariableError.new(['x']),
      Dentaku::MathDomainError.new(:asin, [2]),
      Dentaku::NodeError.new(:numeric, :string, :left),
      Dentaku::ParseError.for(:node_invalid),
      Dentaku::TokenizerError.for(:parse_error),
      Dentaku::ArgumentError.for(:invalid_value),
      Dentaku::ZeroDivisionError.new,
    ].each do |exception|
      expect(exception).to be_a(described_class)
    end
  end

  it 'preserves stdlib ancestry for exceptions subclassing Ruby built-ins' do
    expect(Dentaku::ArgumentError.for(:invalid_value)).to be_a(::ArgumentError)
    expect(Dentaku::ZeroDivisionError.new).to be_a(::ZeroDivisionError)
  end
end

describe Dentaku::ParseError do
  it 'builds a message from reason and metadata' do
    exception = described_class.for(:too_few_operands, operation: Dentaku::AST::Addition, expected: 2, actual: 1)
    expect(exception.message).to eq('Dentaku::AST::Addition has too few operands (given 1, expected 2)')
  end

  it 'constructs with sparse metadata' do
    expect(described_class.for(:node_invalid).message).to eq('Node is invalid')
  end

  it 'describes an :incompatible expectation as requiring compatible operands' do
    exception = described_class.for(:node_invalid, operation: Dentaku::AST::Addition, expected: :incompatible, actual: :string)
    expect(exception.message).to eq('Dentaku::AST::Addition requires compatible operands, but got string')
  end

  it 'allows call sites to override the default message' do
    expect {
      raise described_class.for(:node_invalid), 'Case missing switch variable'
    }.to raise_error(described_class, 'Case missing switch variable')
  end
end

describe Dentaku::TokenizerError do
  it 'builds a message from reason and metadata' do
    exception = described_class.for(:parse_error, at: '$5')
    expect(exception.message).to eq("parse error at: '$5'")
  end
end

describe Dentaku::ArgumentError do
  it 'builds an incompatible_type message from a function context' do
    exception = described_class.for(:incompatible_type, function_name: 'AND', expected: :logical, actual: 5)
    expect(exception.message).to eq('AND() requires logical arguments, but got Integer')
  end

  it 'builds an incompatible_type message from expected and actual' do
    exception = described_class.for(:incompatible_type, expected: Integer, actual: 2.5)
    expect(exception.message).to eq('Float is not compatible with Integer')
  end

  it 'builds an invalid_operator message' do
    exception = described_class.for(:invalid_operator, operation: Dentaku::AST::Addition, operator: :+)
    expect(exception.message).to eq('Dentaku::AST::Addition requires operands that respond to +')
  end

  it 'describes an at-least arity as a range' do
    exception = described_class.for(:too_few_arguments, function_name: 'SUM', expected: 1.., actual: 0)
    expect(exception.message).to eq('SUM() has too few arguments (given 0, expected at least 1)')
  end

  it 'describes an exact arity as a count' do
    exception = described_class.for(:wrong_number_of_arguments, function_name: 'INTERCEPT', expected: 2, actual: 4)
    expect(exception.message).to eq('INTERCEPT() has the wrong number of arguments (given 4, expected 2)')
  end

  it 'constructs with sparse metadata' do
    expect(described_class.for(:invalid_value).message).to eq('Invalid value')
  end

  it 'allows call sites to override the default message' do
    expect {
      raise described_class.for(:invalid_value), 'INTERCEPT() requires arrays of equal length'
    }.to raise_error(described_class, 'INTERCEPT() requires arrays of equal length')
  end

  it 'builds a streaming_unsupported message listing supported operations' do
    exception = described_class.for(
      :streaming_unsupported,
      function_name: 'MAP', supported: %w[SUM ANY]
    )
    expect(exception.message).to eq('MAP() cannot consume a streaming sequence; ' \
                                    'supported operations are SUM, ANY')
  end
end

describe Dentaku::StreamReadError do
  let(:source) do
    Class.new do
      def self.name
        'SensorFeed'
      end

      def dentaku_stream_name
        'sensor-a'
      end
    end.new
  end

  it 'carries source, range, formula location and underlying cause' do
    cause = StandardError.new('connection lost')
    exception = described_class.new(
      source: source, range: 100..199, operation: :read,
      formula: 'SUM(samples)', cause_error: cause
    )

    expect(exception.source).to eq('sensor-a')
    expect(exception.range).to eq('100..199')
    expect(exception.formula).to eq('SUM(samples)')
    expect(exception.cause_error).to eq(cause)
    expect(exception).to be_a(Dentaku::Error)
    expect(exception.message).to include('sensor-a')
    expect(exception.message).to include('100..199')
    expect(exception.message).to include('SUM(samples)')
    expect(exception.message).to include('connection lost')
  end

  it 'rebuilds the message when the formula location is assigned later' do
    exception = described_class.new(
      source: source, range: 0..63, cause_error: StandardError.new('boom')
    )

    expect(exception.formula).to be_nil
    exception.formula = 'samples[5]'
    expect(exception.message).to include('formula samples[5]')
  end
end

describe Dentaku::StreamLengthError do
  it 'reports that length could not be determined' do
    exception = described_class.new(
      source: Object.new, formula: 'COUNT(x)',
      cause_error: StandardError.new('denied')
    )

    expect(exception.message).to include('length')
    expect(exception.message).to include('COUNT(x)')
  end
end

describe Dentaku::StreamProtocolError do
  it 'describes a protocol violation with the requested range' do
    exception = described_class.new(
      source: Object.new, range: 0..3,
      cause_error: 'expected Array, got String'
    )

    expect(exception.message).to include('violated the protocol')
    expect(exception.message).to include('0..3')
    expect(exception.message).to include('expected Array, got String')
  end
end
