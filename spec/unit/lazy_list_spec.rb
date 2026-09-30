# frozen_string_literal: true

RSpec.describe(Dry::Monads::LazyList) do
  lazy_list = described_class
  subject { lazy_list[1, 2, 3] }

  it_behaves_like "a monad"

  def counted_enumerator
    reads = 0

    enumerator = Enumerator.new do |yielder|
      value = 0

      loop do
        reads += 1
        yielder << value
        value += 1
      end
    end

    [enumerator, -> { reads }]
  end

  describe "construction" do
    it "coerces arrays without reading them" do
      expect(lazy_list[1, 2, 3].first(2)).to eql([1, 2])
    end

    it "coerces nil to an empty list" do
      expect(lazy_list.coerce(nil)).to be_empty
    end

    it "coerces an enumerator" do
      expect(lazy_list.coerce([1, 2].each).first(2)).to eql([1, 2])
    end

    it "wraps one value with pure" do
      expect(lazy_list.pure(1).to_a).to eql([1])
    end

    it "does not read an enumerator when built" do
      reads = 0
      enumerator = Enumerator.new do |yielder|
        reads += 1
        yielder << 1
      end

      lazy_list.new(enumerator)

      expect(reads).to be_zero
    end

    it "raises a type error on an uncoercible value" do
      expect {
        lazy_list.coerce(Object.new)
      }.to raise_error(TypeError, /Can't coerce.*LazyList/)
    end
  end

  describe "#bind" do
    it "fairly enumerates combinations from two infinite sources" do
      xs, xs_reads = counted_enumerator
      ys, ys_reads = counted_enumerator
      xs = lazy_list.new(xs)

      combinations = xs.bind do |x|
        lazy_list.new(ys).fmap { |y| [x, y] }
      end

      expect(combinations.first(10)).to eql(
        [
          [0, 0],
          [0, 1], [1, 0],
          [0, 2], [1, 1], [2, 0],
          [0, 3], [1, 2], [2, 1], [3, 0]
        ]
      )
      expect(xs_reads.()).to be(4)
      expect(ys_reads.()).to be(4)
    end

    it "skips empty branches without blocking later branches" do
      xs = [0, 1, 2].each
      ys = [10, 20].each
      xs = lazy_list.new(xs)
      ys = lazy_list.new(ys)

      combinations = xs.bind do |x|
        if x.eql?(1)
          lazy_list[]
        else
          ys.fmap { |y| [x, y] }
        end
      end

      expect(combinations.to_a).to eql(
        [
          [0, 10],
          [0, 20], [2, 10],
          [2, 20]
        ]
      )
    end

    it "raises a branch construction error when that branch is opened" do
      xs, xs_reads = counted_enumerator
      ys, ys_reads = counted_enumerator
      xs = lazy_list.new(xs)
      ys = lazy_list.new(ys)
      error = RuntimeError.new("branch boom")

      combinations = xs.bind do |x|
        case x
        when 0
          ys.fmap { |y| [x, y] }
        when 1
          lazy_list[]
        else
          raise error
        end
      end
      enumerator = combinations.each

      expect([enumerator.next, enumerator.next]).to eql([[0, 0], [0, 1]])
      expect { enumerator.next }.to raise_error(error)
      expect(xs_reads.()).to be(3)
      expect(ys_reads.()).to be(2)
    end

    it "raises a failing branch error at its scheduled iteration" do
      xs, xs_reads = counted_enumerator
      ys, ys_reads = counted_enumerator
      xs = lazy_list.new(xs)
      ys = lazy_list.new(ys)
      error = RuntimeError.new("boom")

      combinations = xs.bind do |x|
        case x
        when 0
          ys.fmap { |y| [x, y] }
        when 1
          lazy_list[]
        else
          lazy_list.new(
            Enumerator.new do |yielder|
              yielder << [x, 0]
              raise error
            end
          )
        end
      end
      enumerator = combinations.each

      values = 5.times.map { enumerator.next }

      expect(values).to eql(
        [[0, 0], [0, 1], [0, 2], [2, 0], [0, 3]]
      )
      expect { 6.times { enumerator.next } }.to raise_error(error)
      expect(xs_reads.()).to be(4)
      expect(ys_reads.()).to be(4)
    end

    it "does not start a thread" do
      xs, = counted_enumerator
      ys, = counted_enumerator
      xs = lazy_list.new(xs)
      ys = lazy_list.new(ys)
      combinations = xs.bind do |x|
        ys.fmap { |y| [x, y] }
      end

      expect(Thread).not_to receive(:new)

      combinations.first(10)
    end
  end

  describe "#apply" do
    it "fairly applies infinite functions to infinite values" do
      xs, xs_reads = counted_enumerator
      ys, ys_reads = counted_enumerator
      functions = lazy_list.new(xs).fmap { |x| ->(y) { x + y } }
      values = lazy_list.new(ys)

      expect(functions.apply(values).first(10)).to eql(
        [0, 1, 1, 2, 2, 2, 3, 3, 3, 3]
      )
      expect(xs_reads.()).to be(4)
      expect(ys_reads.()).to be(4)
    end

    it "accepts a coerced value list" do
      functions = lazy_list.pure(->(value) { value * 2 })

      expect(functions.apply([1, 2]).to_a).to eql([2, 4])
    end
  end

  describe "enumeration" do
    it "supports filter, map, and concatenation lazily" do
      source, reads = counted_enumerator
      values = lazy_list.new(source)

      result = values
        .filter(&:even?)
        .map { |value| value * 10 }
        .+(lazy_list[100])

      expect(result.first(3)).to eql([0, 20, 40])
      expect(reads.()).to be(5)
    end

    it "keeps values available for repeated enumeration" do
      values = lazy_list[1, 2, 3].fmap(&:succ)

      expect(values.first(2)).to eql([2, 3])
      expect(values.to_a).to eql([2, 3, 4])
    end

    it "does not consume more values than requested from a chain" do
      source, reads = counted_enumerator

      result = lazy_list.new(source).fmap(&:succ)

      expect(result.first(3)).to eql([1, 2, 3])
      expect(reads.()).to be(3)
    end
  end

  describe described_class::Mixin do
    subject(:context) { Class.new { include Dry::Monads::LazyList::Mixin }.new }

    it "adds a lazy list constructor" do
      expect(lazy_list::Mixin::L).to be(lazy_list)
      expect(lazy_list::Mixin::LazyList).to be(lazy_list)
      expect(context.LazyList([1, 2]).to_a).to eql([1, 2])
    end
  end
end
