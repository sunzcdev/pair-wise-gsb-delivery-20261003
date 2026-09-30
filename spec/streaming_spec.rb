require 'spec_helper'
require 'dentaku'

describe Dentaku::Streaming do
  # A range-readable data source backed by a Ruby Array that refuses to ever
  # hand back the whole sequence in a single call, and records every physical
  # interval it served.
  class GuardedRangeSource
    include Dentaku::Streaming::Source

    WholeSequenceRead = Class.new(StandardError)

    def initialize(samples, name: "samples", fail_from: nil)
      @samples   = samples
      @name      = name
      @fail_from = fail_from
      @requests  = []
    end

    attr_reader :requests

    def stream_name
      @name
    end

    def stream_length
      @samples.length
    end

    def read_range(start_index, count)
      raise WholeSequenceRead, "whole-sequence read is forbidden" if count >= @samples.length
      raise IOError, "storage offline at index #{start_index}" if @fail_from && start_index >= @fail_from

      @requests << [start_index, count]
      @samples[start_index, count]
    end

    def served_ranges
      @requests.map { |start, count| start...(start + count) }
    end
  end

  let(:data) { (1..10).to_a }
  let(:calculator) { Dentaku::Calculator.new(stream_batch_size: 4) }

  def source(samples = data, **kwargs)
    GuardedRangeSource.new(samples, **kwargs)
  end

  describe 'index access' do
    it 'reads exactly the requested sample' do
      src = source

      expect(calculator.evaluate!('samples[3]', samples: src)).to eq(4)
      expect(src.requests).to eq([[3, 1]])
    end

    it 'supports negative indices with a single point read' do
      src = source

      expect(calculator.evaluate!('samples[-1]', samples: src)).to eq(10)
      expect(src.requests).to eq([[9, 1]])
    end

    it 'reports out-of-range indices with the formula position' do
      expect {
        calculator.evaluate!('samples[100]', samples: source)
      }.to raise_error(Dentaku::StreamIndexError) { |error|
        expect(error.index).to eq(100)
        expect(error.length).to eq(10)
        expect(error.message).to include('index 100 out of range')
        expect(error.location.line).to eq(1)
      }
    end

    it 'rejects non-integer indices' do
      expect {
        calculator.evaluate!('samples[k]', samples: source, k: 1.5)
      }.to raise_error(Dentaku::ArgumentError)
    end

    it 'never calls the whole-array [] method on the source' do
      src = source
      expect(src).not_to receive(:[])
      calculator.evaluate!('samples[2]', samples: src)
    end
  end

  describe 'short-circuit evaluation' do
    it 'ANY stops scanning at the first match and never reads the tail' do
      src = source

      result = calculator.evaluate!('ANY(samples, s, s > 2)', samples: src)

      expect(result).to eq(true)
      expect(src.served_ranges).to eq([0...4])
    end

    it 'ALL stops at the first failing sample' do
      src = source

      result = calculator.evaluate!('ALL(samples, s, s > 5)', samples: src)

      expect(result).to eq(false)
      expect(src.served_ranges).to eq([0...4])
    end

    it 'scans all batches when the predicate never short-circuits' do
      src = source

      result = calculator.evaluate!('ALL(samples, s, s > 0)', samples: src)

      expect(result).to eq(true)
      expect(src.served_ranges).to eq([0...4, 4...8, 8...10])
    end

    it 'IF does not touch the stream on the untaken branch' do
      src = source

      expect(calculator.evaluate!('IF(true, 1, SUM(samples))', samples: src)).to eq(1)
      expect(src.requests).to be_empty

      src_two = source
      expect(calculator.evaluate!('IF(false, 1, SUM(samples))', samples: src_two)).to eq(55)
      expect(src_two.served_ranges).to eq([0...4, 4...8, 8...10])
    end

    it 'logical combinators skip the stream when already decisive' do
      src = source
      expect(calculator.evaluate!('false AND ANY(samples, s, s > 0)', samples: src)).to eq(false)
      expect(src.requests).to be_empty
    end
  end

  describe 'aggregates' do
    it 'SUM reads fixed-size batches and never the whole sequence' do
      src = source

      expect(calculator.evaluate!('SUM(samples)', samples: src)).to eq(55)
      expect(src.served_ranges).to eq([0...4, 4...8, 8...10])
    end

    it 'AVG streams samples one batch at a time' do
      src = source((1..6).to_a)

      expect(calculator.evaluate!('AVG(samples)', samples: src)).to eq(BigDecimal('3.5'))
      expect(src.served_ranges).to eq([0...4, 4...6])
    end

    it 'MIN/MAX/MUL stream the input' do
      expect(calculator.evaluate!('MIN(samples)', samples: source)).to eq(1)
      expect(calculator.evaluate!('MAX(samples)', samples: source)).to eq(10)

      src = source([2, 3, 4, 5, 6, 1])
      expect(calculator.evaluate!('MUL(samples)', samples: src)).to eq(720)
      expect(src.served_ranges).to eq([0...4, 4...6])
    end

    it 'COUNT only reads the length, never any sample interval' do
      src = source

      expect(calculator.evaluate!('COUNT(samples)', samples: src)).to eq(10)
      expect(src.requests).to be_empty
    end

    it 'still supports ordinary arrays' do
      expect(calculator.evaluate!('SUM(samples)', samples: [1, 2, 3, 4])).to eq(10)
      expect(calculator.evaluate!('AVG(samples)', samples: [2, 4])).to eq(3)
      expect(calculator.evaluate!('COUNT(samples)', samples: [1, 2])).to eq(2)
    end
  end

  describe 'unsupported operations' do
    it 'refuses MAP/FILTER/PLUCK/REDUCE rather than materializing' do
      [
        'MAP(samples, s, s + 1)',
        'FILTER(samples, s, s > 1)',
        'PLUCK(samples, value, 0)',
        'REDUCE(samples, memo, s, memo + s, 0)'
      ].each do |formula|
        expect {
          calculator.evaluate!(formula, samples: source)
        }.to raise_error(Dentaku::StreamNotSupportedError) { |error|
          expect(error.message).to include('does not support streaming inputs')
          expect(error.message).to include('SUM')
          expect(error.location).not_to be_nil
        }
      end
    end

    it 'refuses arithmetic and comparisons on streams' do
      expect {
        calculator.evaluate!('samples + 1', samples: source)
      }.to raise_error(Dentaku::StreamNotSupportedError, /arithmetic/)

      expect {
        calculator.evaluate!('samples > 1', samples: source)
      }.to raise_error(Dentaku::StreamNotSupportedError, /comparison/)
    end

    it 'does not implicitly materialize through Array()' do
      src = source
      expect(src).not_to receive(:to_a)
      expect(src).not_to receive(:each)
      expect {
        calculator.evaluate!('MAP(samples, s, s)', samples: src)
      }.to raise_error(Dentaku::StreamNotSupportedError)
    end
  end

  describe 'read failures' do
    it 'wraps data source errors with operation, interval and formula position' do
      src = source((1..20).to_a, fail_from: 8)

      expect {
        calculator.evaluate!('SUM(samples)', samples: src)
      }.to raise_error(Dentaku::StreamReadError) { |error|
        expect(error.cause).to be_a(IOError)
        expect(error.range).to eq(8...12)
        expect(error.operation).to eq('SUM')
        expect(error.source).to equal(src)
        expect(error.message).to include('[8..11]')
        expect(error.message).to include('formula position 1:1')
        expect(error.message).to include("near 'SUM(samples'")
      }

      expect(src.served_ranges).to eq([0...4, 4...8])
    end

    it 'attributes index read failures to the access expression' do
      src = source((1..20).to_a, fail_from: 15)

      expect {
        calculator.evaluate!('samples[15]', samples: src)
      }.to raise_error(Dentaku::StreamReadError) { |error|
        expect(error.range).to eq(15...16)
        expect(error.message).to include('samples[]')
        expect(error.location.line).to eq(1)
      }
    end

    it 'reports protocol violations' do
      src = Object.new
      def src.stream_length; 10; end
      def src.read_range(_start, _count); :not_an_array; end

      expect {
        calculator.evaluate!('SUM(samples)', samples: src)
      }.to raise_error(Dentaku::StreamProtocolError, /must return an Array/)
    end
  end

  describe 'repeated reads within one calculation' do
    it 'serves overlapping reads from cache and never fetches twice' do
      src = source

      result = calculator.evaluate!(
        'samples[2] + samples[2] + samples[3]', samples: src
      )

      expect(result).to eq(10)
      expect(src.requests).to eq([[2, 1], [3, 1]])
    end

    it 'merges a later overlapping range request into the missing suffix' do
      src = source((1..20).to_a)

      calculator.evaluate!('samples[2] + SUM(samples)', samples: src)
      # first the index read at 2, then batch 0..3 only fetches 0..1
      expect(src.requests.first(2)).to eq([[2, 1], [0, 2]])
      expect(src.requests).not_to include([0, 4])
    end

    it 'gives each evaluation a fresh read cache' do
      src = source

      calculator.evaluate!('samples[2]', samples: src)
      calculator.evaluate!('samples[2]', samples: src)

      expect(src.requests).to eq([[2, 1], [2, 1]])
    end

    it 'exposes the access log on the calculator after evaluation' do
      src = source
      calculator.evaluate!('SUM(samples)', samples: src)

      session = calculator.last_stream_session
      expect(session.reads.map(&:range)).to eq([0...4, 4...8, 8...10])
      expect(session.reads.first.source).to equal(src)
    end
  end

  describe 'protocol detection' do
    it 'treats duck-typed objects as streams but not arrays, strings or hashes' do
      expect(Dentaku::Streaming.stream?(source)).to eq(true)
      expect(Dentaku::Streaming.stream?([1, 2])).to eq(false)
      expect(Dentaku::Streaming.stream?('abc')).to eq(false)
      expect(Dentaku::Streaming.stream?(a: 1)).to eq(false)
      expect(Dentaku::Streaming.stream?(42)).to eq(false)
      expect(Dentaku::Streaming.stream?(nil)).to eq(false)
    end
  end
end
