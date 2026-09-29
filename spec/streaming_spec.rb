# frozen_string_literal: true
require 'spec_helper'

describe 'JMESPath.stream' do
  # A read-only pull source that records how many elements were read
  # and how many times it was closed.
  class CountingSource
    attr_reader :reads, :closes

    def initialize(elements)
      @elements = elements
      @reads = 0
      @closes = 0
      @index = 0
    end

    def next
      raise StopIteration if @index >= @elements.size

      @reads += 1
      element = @elements[@index]
      @index += 1
      element
    end

    def close
      @closes += 1
    end
  end

  # A read-only iterable source that records reads and closes.
  class EachSource
    attr_reader :reads, :closes

    def initialize(elements)
      @elements = elements
      @reads = 0
      @closes = 0
    end

    def each
      @elements.each do |element|
        @reads += 1
        yield element
      end
    end

    def close
      @closes += 1
    end
  end

  let(:data) { (1..100).map { |n| { 'n' => n } } }

  describe 'streaming filters and projections' do
    it 'does not traverse the tail when the consumer stops early' do
      source = CountingSource.new(data)
      stream = JMESPath.stream('[?n > `0`].n', source)
      expect(stream.first(3)).to eq([1, 2, 3])
      expect(source.reads).to eq(3)
    end

    it 'closes the source exactly once on early exit' do
      source = CountingSource.new(data)
      stream = JMESPath.stream('[*].n', source)
      stream.first(3)
      expect(source.closes).to eq(1)
      stream.close
      expect(source.closes).to eq(1)
    end

    it 'produces the same results as search when fully consumed' do
      expression = '[?n >= `50`].{value: n, big: n > `75`}'
      source = CountingSource.new(data)
      expect(JMESPath.stream(expression, source).to_a)
        .to eq(JMESPath.search(expression, data))
      expect(source.reads).to eq(100)
      expect(source.closes).to eq(1)
    end

    it 'streams flatten projections' do
      values = [[1, 2], [3], 4, [5, 6]]
      source = CountingSource.new(values)
      expect(JMESPath.stream('[]', source).to_a)
        .to eq(JMESPath.search('[]', values))
    end

    it 'streams nested projections' do
      values = [{ 'a' => [1, 2] }, { 'a' => [3] }]
      source = CountingSource.new(values)
      expect(JMESPath.stream('[*].a[]', source).to_a)
        .to eq(JMESPath.search('[*].a[]', values))
    end

    it 'streams non-negative slices without traversing the tail' do
      source = CountingSource.new(data)
      stream = JMESPath.stream('[1:10].n', source)
      expect(stream.first(3)).to eq([2, 3, 4])
      expect(source.reads).to eq(4)
      expect(source.closes).to eq(1)
    end

    it 'streams chains of projections' do
      values = [{ 'a' => [{ 'b' => 1 }, { 'b' => 2 }] },
                { 'a' => [{ 'b' => 3 }] }]
      source = CountingSource.new(values)
      expect(JMESPath.stream('[*].a[*].b | [?@ > `1`]', source).to_a)
        .to eq(JMESPath.search('[*].a[*].b | [?@ > `1`]', values))
    end

    it 'supports each-based sources' do
      source = EachSource.new(data)
      stream = JMESPath.stream('[?n <= `3`].n', source)
      expect(stream.to_a).to eq([1, 2, 3])
      expect(source.closes).to eq(1)
    end

    it 'closes the source exactly once when evaluation raises' do
      values = [{ 'n' => 1 }, { 'n' => 'two' }, { 'n' => 3 }]
      source = CountingSource.new(values)
      stream = JMESPath.stream('[*].abs(@)', source)
      expect { stream.to_a }.to raise_error(JMESPath::Errors::InvalidTypeError)
      expect(source.closes).to eq(1)
    end
  end

  describe 'nodes requiring materialization' do
    it 'rejects before consuming the source when materialization is disabled' do
      ['sort(@)', 'reverse(@)', 'length(@)', '[-1]', '[-2:]', 'sum(@)'].each do |expression|
        source = CountingSource.new([3, 1, 2])
        expect { JMESPath.stream(expression, source, allow_materialization: false) }
          .to raise_error(JMESPath::Errors::MaterializationError)
        expect(source.reads).to eq(0)
        expect(source.closes).to eq(0)
      end
    end

    it 'materializes the source when allowed' do
      source = CountingSource.new([3, 1, 2])
      expect(JMESPath.stream('sort(@)', source).to_a).to eq([1, 2, 3])
      expect(source.reads).to eq(3)
      expect(source.closes).to eq(1)
    end

    it 'materializes negative indices when allowed' do
      values = [{ 'n' => 1 }, { 'n' => 2 }]
      source = CountingSource.new(values)
      expect(JMESPath.stream('[-1].n', source).to_a).to eq([2])
    end

    it 'yields scalar results once' do
      source = CountingSource.new([3, 1, 2])
      expect(JMESPath.stream('length(@)', source).to_a).to eq([3])
    end

    it 'supports :allow_materialization on the runtime' do
      runtime = JMESPath::Runtime.new(allow_materialization: false)
      source = CountingSource.new([3, 1, 2])
      expect { runtime.stream('sort(@)', source) }
        .to raise_error(JMESPath::Errors::MaterializationError)
      expect(runtime.stream('[?@ > `1`]', CountingSource.new([3, 1, 2])).to_a)
        .to eq([3, 2])
    end
  end
end
