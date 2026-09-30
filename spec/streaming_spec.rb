require 'spec_helper'
require 'dentaku'

describe 'Dentaku streaming input protocol' do
  # A data source that serves a large sampled sequence in fixed-size
  # windows. Any single read covering the whole sequence is rejected, which
  # proves Dentaku never asks for an implicit full materialization.
  class ForbiddenFullReadSource
    attr_reader :reads, :length_calls

    def initialize(values = (1..1000).to_a, window_size: 64,
                   name: 'samples', fail_range: nil, fail_length: false)
      @values = values
      @window_size = window_size
      @name = name
      @fail_range = fail_range
      @fail_length = fail_length
      @reads = []
      @length_calls = 0
    end

    def dentaku_stream?
      true
    end

    def dentaku_stream_name
      @name
    end

    def dentaku_stream_window_size
      @window_size
    end

    def dentaku_stream_length
      @length_calls += 1
      raise 'boom: length unavailable' if @fail_length

      @values.length
    end

    def dentaku_stream_read(range)
      raise 'full-sequence reads are forbidden' if
        range.begin.zero? && range.end >= @values.length - 1
      raise "read failure on #{range.begin}..#{range.end}" if
        @fail_range && range == @fail_range

      @reads << range
      @values[range]
    end
  end

  let(:calculator) { Dentaku::Calculator.new }

  describe 'index access' do
    it 'fetches only the window backing the requested index' do
      source = ForbiddenFullReadSource.new
      calculator.store(samples: source)

      expect(calculator.evaluate!('samples[130]')).to eq(131)
      expect(source.reads).to eq([128..191])
      expect(source.length_calls).to eq(1)
    end

    it 'answers nil out of range without reading data' do
      source = ForbiddenFullReadSource.new((1..10).to_a)
      result = calculator.evaluate!('x[100]', x: source)

      expect(result).to be_nil
      expect(source.reads).to eq([])
    end

    it 'answers nil for a negative index without reading data' do
      source = ForbiddenFullReadSource.new((1..10).to_a)
      result = calculator.evaluate!('x[-1]', x: source)

      expect(result).to be_nil
      expect(source.reads).to eq([])
    end

    it 'rejects non-integer indexes with a clear error' do
      source = ForbiddenFullReadSource.new((1..10).to_a)

      expect {
        calculator.evaluate!('x[y]', x: source, y: 'z')
      }.to raise_error(Dentaku::ArgumentError, /integer index/)
    end

    it 'reads the same window once when an index is used repeatedly' do
      source = ForbiddenFullReadSource.new((1..100).to_a, window_size: 32)
      formula = 'samples[10] + samples[11] + samples[10]'

      expect(calculator.evaluate!(formula, samples: source)).to eq(34)
      expect(source.reads).to eq([0..31])
    end
  end

  describe 'short-circuiting collection functions' do
    it 'ANY stops reading once a match is found' do
      source = ForbiddenFullReadSource.new((1..1000).to_a, window_size: 64)

      expect(calculator.evaluate!('ANY(samples, s, s == 200)',
                                  samples: source)).to eq(true)
      expect(source.reads).to eq([0..63, 64..127, 128..191, 192..255])
    end

    it 'ALL stops reading once a violation is found' do
      values = (1..1000).to_a
      values[140] = -1
      source = ForbiddenFullReadSource.new(values, window_size: 64)

      expect(calculator.evaluate!('ALL(samples, s, s > 0)',
                                  samples: source)).to eq(false)
      expect(source.reads).to eq([0..63, 64..127, 128..191])
    end

    it 'ALL reads every window when the predicate always holds' do
      source = ForbiddenFullReadSource.new((1..1000).to_a, window_size: 250)

      expect(calculator.evaluate!('ALL(samples, s, s > 0)',
                                  samples: source)).to eq(true)
      expect(source.reads).to eq([0..249, 250..499, 500..749, 750..999])
    end

    it 'short-circuits through IF so the unselected branch never reads' do
      source = ForbiddenFullReadSource.new((1..1000).to_a, window_size: 64)

      formula = 'IF(true, ANY(samples, s, s == 10), SUM(samples))'
      expect(calculator.evaluate!(formula, samples: source)).to eq(true)
      expect(source.reads).to eq([0..63])
    end

    it 'short-circuits through AND/OR combinators' do
      source = ForbiddenFullReadSource.new((1..1000).to_a, window_size: 64)

      expect(calculator.evaluate!('false AND ALL(samples, s, s > 0)',
                                  samples: source)).to eq(false)
      expect(source.reads).to eq([])

      fresh = ForbiddenFullReadSource.new((1..1000).to_a, window_size: 64)
      expect(calculator.evaluate!('true OR ANY(fresh, s, s > 10)',
                                  fresh: fresh)).to eq(true)
      expect(fresh.reads).to eq([])
    end
  end

  describe 'aggregates' do
    it 'SUM consumes windows without materializing the sequence' do
      source = ForbiddenFullReadSource.new((1..1000).to_a, window_size: 250)

      expect(calculator.evaluate!('SUM(samples)', samples: source))
        .to eq(500_500)
      expect(source.reads).to eq([0..249, 250..499, 500..749, 750..999])
    end

    it 'AVG streams and divides by the observed count' do
      source = ForbiddenFullReadSource.new((1..4).to_a, window_size: 2)

      expect(calculator.evaluate!('AVG(samples)', samples: source))
        .to eq(BigDecimal('2.5'))
      expect(source.reads).to eq([0..1, 2..3])
    end

    it 'MIN and MAX stream windows' do
      values = [3, 9, 1, 7, 42, 0, 5]
      source = ForbiddenFullReadSource.new(values, window_size: 3)

      expect(calculator.evaluate!('MIN(samples)', samples: source)).to eq(0)
      expect(calculator.evaluate!('MAX(samples)', samples: source)).to eq(42)
    end

    it 'COUNT only asks for the length' do
      source = ForbiddenFullReadSource.new((1..1000).to_a, window_size: 128)

      expect(calculator.evaluate!('COUNT(samples)', samples: source)).to eq(1000)
      expect(source.reads).to eq([])
      expect(source.length_calls).to eq(1)
    end

    it 'REDUCE folds in read order' do
      values = [2, 3, 4, 5]
      source = ForbiddenFullReadSource.new(values, window_size: 2)

      result = calculator.evaluate!(
        'REDUCE(samples, memo, s, memo + s, 100)', samples: source
      )
      expect(result).to eq(114)
      expect(source.reads).to eq([0..1, 2..3])
    end

    it 'MUL folds streamed samples' do
      source = ForbiddenFullReadSource.new([2, 3, 4], window_size: 2)

      expect(calculator.evaluate!('MUL(samples)', samples: source)).to eq(24)
      expect(source.reads).to eq([0..1, 2..2])
    end

    it 'AVG raises on an empty streaming sequence' do
      source = ForbiddenFullReadSource.new([], window_size: 10)

      expect {
        calculator.evaluate!('AVG(samples)', samples: source)
      }.to raise_error(Dentaku::ArgumentError, /AVG/)
    end

    it 'SUM accepts an empty streaming sequence without reading' do
      source = ForbiddenFullReadSource.new([], window_size: 10)

      expect(calculator.evaluate!('SUM(samples)', samples: source)).to eq(0)
      expect(source.reads).to eq([])
    end
  end

  describe 'repeated reads within one calculation' do
    it 'serves repeated windows from one stable snapshot' do
      source = ForbiddenFullReadSource.new((1..10).to_a, window_size: 4)
      formula = 'samples[1] + samples[2] + SUM(samples)'

      expect(calculator.evaluate!(formula, samples: source)).to eq(60)
      expect(source.reads).to eq([0..3, 4..7, 8..9])
    end

    it 'starts a fresh snapshot for each evaluation' do
      source = ForbiddenFullReadSource.new((1..10).to_a, window_size: 4)
      calculator.store(samples: source)

      calculator.evaluate!('samples[1]')
      calculator.evaluate!('samples[1]')

      expect(source.reads).to eq([0..3, 0..3])
    end
  end

  describe 'unsupported operations' do
    it 'MAP rejects streaming input instead of materializing' do
      source = ForbiddenFullReadSource.new((1..10).to_a, window_size: 4)

      expect {
        calculator.evaluate!('MAP(samples, s, s * 2)', samples: source)
      }.to raise_error(Dentaku::ArgumentError) { |error|
        expect(error.reason).to eq(:streaming_unsupported)
        expect(error.message).to include('MAP')
        expect(error.message).to include('index, COUNT, SUM')
      }
      expect(source.reads).to eq([])
    end

    it 'FILTER rejects streaming input' do
      source = ForbiddenFullReadSource.new((1..10).to_a, window_size: 4)

      expect {
        calculator.evaluate!('FILTER(samples, s, s > 5)', samples: source)
      }.to raise_error(Dentaku::ArgumentError, /FILTER/)
    end

    it 'PLUCK rejects streaming input' do
      source = ForbiddenFullReadSource.new((1..10).to_a, window_size: 4)

      expect {
        calculator.evaluate!('PLUCK(samples, value)', samples: source)
      }.to raise_error(Dentaku::ArgumentError, /PLUCK/)
    end

    it 'does not treat a plain Array as a stream' do
      expect(calculator.evaluate!('MAP(xs, x, x + 1)', xs: [1, 2, 3]))
        .to eq([2, 3, 4])
    end

    it 'rejects more than one streaming argument to an aggregate' do
      a = ForbiddenFullReadSource.new([1, 2], window_size: 2)
      b = ForbiddenFullReadSource.new([3, 4], window_size: 2)

      expect {
        calculator.evaluate!('SUM(a, b)', a: a, b: b)
      }.to raise_error(Dentaku::ArgumentError) { |error|
        expect(error.reason).to eq(:streaming_unsupported)
        expect(error.message).to include('at most one streaming sequence')
      }
    end

    it 'allows a stream combined with materialized scalar arguments' do
      source = ForbiddenFullReadSource.new([1, 2, 3, 4], window_size: 2)

      expect(calculator.evaluate!('SUM(source, 10, 20)', source: source))
        .to eq(40)
    end
  end

  describe 'read failures' do
    it 'reports the formula position and data range for window reads' do
      source = ForbiddenFullReadSource.new((1..100).to_a, window_size: 32,
                                           fail_range: 32..63)

      begin
        calculator.evaluate!('SUM(samples)', samples: source)
        raise 'expected a StreamReadError'
      rescue Dentaku::StreamReadError => error
        expect(error.source).to eq('samples')
        expect(error.range).to eq('32..63')
        expect(error.formula).to eq('SUM(samples)')
        expect(error.message).to include('samples')
        expect(error.message).to include('32..63')
        expect(error.message).to include('SUM(samples)')
      end
    end

    it 'annotates index access failures with the subexpression' do
      source = ForbiddenFullReadSource.new((1..100).to_a, window_size: 32,
                                           fail_range: 32..63)

      expect {
        calculator.evaluate!('1 + samples[40]', samples: source)
      }.to raise_error(Dentaku::StreamReadError) { |error|
        expect(error.range).to eq('32..63')
        expect(error.formula).to eq('samples[40] in 1 + samples[40]')
      }
    end

    it 'wraps length failures with the formula location' do
      source = ForbiddenFullReadSource.new((1..10).to_a, fail_length: true)

      expect {
        calculator.evaluate!('COUNT(samples)', samples: source)
      }.to raise_error(Dentaku::StreamLengthError) { |error|
        expect(error.formula).to eq('COUNT(samples)')
        expect(error.message).to include('length')
      }
    end

    it 'returns nil in permissive mode when a read fails' do
      source = ForbiddenFullReadSource.new((1..100).to_a, window_size: 32,
                                           fail_range: 32..63)

      expect(calculator.evaluate('SUM(samples)', samples: source)).to be_nil
    end

    it 'raises StreamProtocolError when a window is not an Array' do
      source = Object.new
      def source.dentaku_stream?; true; end
      def source.dentaku_stream_length; 10; end
      def source.dentaku_stream_read(_range); 'not-an-array'; end
      def source.dentaku_stream_window_size; 4; end

      expect {
        calculator.evaluate!('SUM(samples)', samples: source)
      }.to raise_error(Dentaku::StreamProtocolError, /expected Array/)
    end

    it 'raises StreamProtocolError when a window has the wrong size' do
      source = Object.new
      def source.dentaku_stream?; true; end
      def source.dentaku_stream_length; 10; end
      def source.dentaku_stream_read(_range); [1, 2]; end
      def source.dentaku_stream_window_size; 4; end

      expect {
        calculator.evaluate!('samples[0]', samples: source)
      }.to raise_error(Dentaku::StreamProtocolError, /expected 4 samples, got 2/)
    end

    it 'raises StreamLengthError when length is not a non-negative Integer' do
      source = Object.new
      def source.dentaku_stream?; true; end
      def source.dentaku_stream_length; 'ten'; end

      expect {
        calculator.evaluate!('COUNT(samples)', samples: source)
      }.to raise_error(Dentaku::StreamProtocolError, /non-negative Integer/)
    end
  end

  describe 'Dentaku::Streaming::Base' do
    it 'lets a subclass implement only length and read_window' do
      source_class = Class.new(Dentaku::Streaming::Base) do
        def initialize
          super(window_size: 3, name: 'csv')
        end

        def length
          6
        end

        def read_window(range)
          (1..6).to_a[range]
        end
      end

      result = calculator.evaluate!('SUM(samples)', samples: source_class.new)
      expect(result).to eq(21)
    end

    it 'requires subclasses to implement the hooks' do
      expect {
        calculator.evaluate!('COUNT(samples)', samples: Dentaku::Streaming::Base.new)
      }.to raise_error(NotImplementedError, /#length/)
    end
  end
end
