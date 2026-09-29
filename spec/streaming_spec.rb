# frozen_string_literal: true
require 'spec_helper'

module JMESPath
  describe '.stream' do
    # A read-only array source that records how many elements were read
    # and how many times it was closed.
    class RecordingSource
      attr_reader :elements, :read_count, :close_count

      def initialize(elements)
        @elements = elements
        @read_count = 0
        @close_count = 0
      end

      def each
        return enum_for(:each) unless block_given?
        @elements.each do |element|
          @read_count += 1
          yield element
        end
      end

      def close
        @close_count += 1
      end
    end

    def source(count = 100)
      RecordingSource.new(
        (0...count).map { |i| { 'foo' => i, 'even' => i.even? } }
      )
    end

    it 'returns a lazy enumerable' do
      expect(JMESPath.stream('[*].foo', source)).to be_a(Enumerable)
    end

    it 'requires sources to respond to #each' do
      expect { JMESPath.stream('[*]', 42) }.to raise_error(Errors::InvalidTypeError)
    end

    it 'accepts sources that do not respond to #close' do
      expect(JMESPath.stream('[*].foo', [{ 'foo' => 1 }]).to_a).to eq([1])
    end

    describe 'streaming projections and filters' do
      it 'does not read the tail when the consumer stops early' do
        src = source
        expect(JMESPath.stream('[*].foo', src).take(3)).to eq([0, 1, 2])
        expect(src.read_count).to eq(3)
      end

      it 'reads only until enough filtered elements match' do
        src = source
        expect(JMESPath.stream('[?even].foo', src).take(3)).to eq([0, 2, 4])
        expect(src.read_count).to eq(5)
      end

      it 'streams non-negative indices without reading the tail' do
        src = source
        expect(JMESPath.stream('[2]', src).to_a).to eq([{ 'foo' => 2, 'even' => true }])
        expect(src.read_count).to eq(3)
      end

      it 'streams slices and stops reading after the slice end' do
        src = source
        expect(JMESPath.stream('[1:4].foo', src).to_a).to eq([1, 2, 3])
        expect(src.read_count).to eq(4)
      end

      it 'streams stepped slices' do
        src = source
        expect(JMESPath.stream('[::25].foo', src).to_a).to eq([0, 25, 50, 75])
      end

      it 'streams flatten expressions' do
        src = RecordingSource.new([[1, 2], [3], 4])
        expect(JMESPath.stream('[]', src).to_a).to eq([1, 2, 3, 4])
      end

      it 'streams piped sub-expressions without reading the tail' do
        src = source
        result = JMESPath.stream('[?even].foo | [1:3]', src)
        expect(result.to_a).to eq([2, 4])
        expect(src.read_count).to be < 100
      end

      it 'matches a regular search when fully consumed' do
        expressions = [
          '[*].foo',
          '[?even].foo',
          '[*][foo, foo]',
          '[*].{value: foo}',
          '[1:10].foo',
          '[::2].foo',
          '[?even].foo | [1:5]',
          '@'
        ]
        expressions.each do |expression|
          src = source
          expected = JMESPath.search(expression, src.elements)
          expect(JMESPath.stream(expression, src).to_a).to eq(expected)
          expect(src.close_count).to eq(1)
        end
      end
    end

    describe 'closing the source' do
      it 'closes the source once when fully consumed' do
        src = source
        JMESPath.stream('[*].foo', src).to_a
        expect(src.close_count).to eq(1)
      end

      it 'closes the source once when the consumer stops early' do
        src = source
        JMESPath.stream('[*].foo', src).take(3)
        expect(src.close_count).to eq(1)
      end

      it 'closes the source once when the consumer breaks' do
        src = source
        JMESPath.stream('[*].foo', src).each { |_value| break }
        expect(src.close_count).to eq(1)
      end

      it 'closes the source once when the query raises' do
        src = RecordingSource.new(['a', 'b', 'c'])
        expect do
          JMESPath.stream('[*].abs(@)', src).to_a
        end.to raise_error(Errors::InvalidTypeError)
        expect(src.close_count).to eq(1)
      end

      it 'closes the source at most once across explicit close and iteration' do
        src = source
        result = JMESPath.stream('[*].foo', src)
        result.close
        result.to_a
        result.close
        expect(src.close_count).to eq(1)
      end
    end

    describe 'expressions that require materialization' do
      it 'materializes sort() by default' do
        src = source(10)
        shuffled = RecordingSource.new(src.elements.reverse)
        result = JMESPath.stream('sort([*].foo)', shuffled)
        expect(result.to_a).to eq((0...10).to_a)
        expect(shuffled.read_count).to eq(10)
        expect(shuffled.close_count).to eq(1)
      end

      it 'materializes negative indices by default' do
        src = source
        expect(JMESPath.stream('[-1]', src).to_a).to eq([src.elements[-1]])
        expect(src.read_count).to eq(100)
      end

      it 'materializes negative slices by default' do
        src = source
        expected = JMESPath.search('[-3:].foo', src.elements)
        expect(JMESPath.stream('[-3:].foo', src).to_a).to eq(expected)
      end

      it 'yields non-array results as a single element' do
        src = source(10)
        expect(JMESPath.stream('length([*])', src).to_a).to eq([10])
      end

      it 'raises before consuming when materialization is disabled' do
        src = source
        expect do
          JMESPath.stream('sort([*].foo)', src, materialize: false)
        end.to raise_error(Errors::StreamingNotSupportedError)
        expect(src.read_count).to eq(0)
        expect(src.close_count).to eq(0)
      end

      it 'raises for negative indices when materialization is disabled' do
        src = source
        expect do
          JMESPath.stream('[-1]', src, materialize: false)
        end.to raise_error(Errors::StreamingNotSupportedError)
        expect(src.read_count).to eq(0)
      end

      it 'still streams streamable expressions when materialization is disabled' do
        src = source
        result = JMESPath.stream('[?even].foo', src, materialize: false)
        expect(result.take(3)).to eq([0, 2, 4])
        expect(src.read_count).to eq(5)
        expect(src.close_count).to eq(1)
      end
    end
  end
end
