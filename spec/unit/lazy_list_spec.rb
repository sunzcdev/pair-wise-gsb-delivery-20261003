# frozen_string_literal: true

RSpec.describe(Dry::Monads::LazyList) do
  lazy_list = described_class

  # An infinite enumerator of 0, 1, 2, ... recording every value it
  # is asked to produce, so tests can observe how far a source was read.
  def counted_naturals(reads)
    Enumerator.new do |y|
      n = 0
      loop do
        reads << n
        y << n
        n += 1
      end
    end
  end

  describe "#bind" do
    context "with two infinite sources" do
      let(:x_reads) { [] }
      let(:y_reads) { [] }

      let(:combined) do
        lazy_list.enumerate(counted_naturals(x_reads)).bind do |x|
          lazy_list.enumerate(counted_naturals(y_reads)).fmap { |y| [x, y] }
        end
      end

      it "interleaves branches fairly so every finite combination appears" do
        # A plain concatenating bind would never leave the x = 0 branch.
        # Interleaving reaches x = 3 within the first ten elements.
        expect(combined.take(10)).to eql(
          [
            [0, 0], [1, 0], [0, 1], [2, 0], [0, 2],
            [1, 1], [0, 3], [3, 0], [0, 4], [1, 2]
          ]
        )
      end

      it "reads only a bounded prefix of each source" do
        combined.take(10)

        expect(x_reads).to eql([0, 1, 2, 3])
        expect(y_reads.size).to be(10)
        expect(y_reads.max).to be(4)
      end

      it "does not start background threads" do
        expect { combined.take(10) }.not_to(change { Thread.list.count })
      end
    end

    context "with an empty branch" do
      it "skips empty branches without stalling" do
        result = lazy_list[1, 2, 3].bind { |x|
          x.even? ? lazy_list.empty : lazy_list[x, -x]
        }

        expect(result.take(4)).to eql([1, 3, -1, -3])
      end

      it "keeps producing values from sparse infinite sources" do
        evens = lazy_list.enumerate(counted_naturals([])).bind { |x|
          x.odd? ? lazy_list.empty : lazy_list[x]
        }

        expect(evens.take(3)).to eql([0, 2, 4])
      end

      it "binds over an empty list to an empty list" do
        result = lazy_list.empty.bind { raise "must not be called" }

        expect(result.take(3)).to eql([])
        expect(result).to be_empty
      end
    end

    context "with a branch that raises" do
      let(:result) do
        lazy_list.enumerate(counted_naturals([])).bind { |x|
          raise "boom at #{x}" if x == 2

          lazy_list[x * 10]
        }
      end

      it "yields elements produced before the failing branch" do
        expect(result.take(2)).to eql([0, 10])
      end

      it "raises exactly when the iteration reaches the failing branch" do
        expect { result.take(3) }.to raise_error(RuntimeError, "boom at 2")
      end
    end

    it "accepts a callable instead of a block" do
      expect(lazy_list[1, 2, 3].bind(-> x { [x, x] }).take(6))
        .to eql([1, 2, 1, 3, 2, 3])
    end
  end

  describe "#apply" do
    let(:arg_reads) { [] }

    let(:functions) { lazy_list[-> x { x + 1 }, -> x { x * 10 }] }
    let(:arguments) { lazy_list.enumerate(counted_naturals(arg_reads)) }

    it "applies functions fairly over an infinite argument source" do
      expect(functions.apply(arguments).take(6))
        .to eql([1, 0, 2, 10, 3, 20])
    end

    it "reads only as many arguments as needed" do
      functions.apply(arguments).take(6)

      expect(arg_reads).to eql([0, 1, 2])
    end
  end

  describe "#fmap" do
    it "maps lazily over infinite sources" do
      reads = []
      doubled = lazy_list.enumerate(counted_naturals(reads)).fmap { _1 * 2 }

      expect(doubled.take(3)).to eql([0, 2, 4])
      expect(reads).to eql([0, 1, 2])
    end
  end

  describe "#interleave" do
    it "alternates two finite lists" do
      expect(lazy_list[1, 3, 5].interleave(lazy_list[2, 4]).take(5))
        .to eql([1, 2, 3, 4, 5])
    end

    it "lets a finite list survive an infinite one" do
      mixed = lazy_list.enumerate(counted_naturals([])).interleave(lazy_list[:a, :b])

      expect(mixed.take(5)).to eql([0, :a, 1, :b, 2])
    end
  end

  describe "#+" do
    it "appends in order" do
      expect((lazy_list[1, 2] + lazy_list[3, 4]).take(4)).to eql([1, 2, 3, 4])
    end
  end

  describe ".unfold" do
    it "builds a list lazily from Maybe values" do
      reads = []
      list = lazy_list.unfold(0) { |state|
        reads << state
        state < 100 ? Dry::Monads::Some([state + 1, state * 10]) : Dry::Monads::None()
      }

      expect(list.take(3)).to eql([0, 10, 20])
      expect(reads).to eql([0, 1, 2])
    end
  end

  describe ".coerce" do
    it "coerces nil to an empty list" do
      expect(lazy_list.coerce(nil)).to be_empty
    end

    it "coerces an array" do
      expect(lazy_list.coerce([1, 2]).take(2)).to eql([1, 2])
    end

    it "raises a type error on uncoercible object" do
      expect { lazy_list.coerce(Object.new) }
        .to raise_error(TypeError, /Can't coerce/)
    end
  end

  describe "#head" do
    it "wraps the first element with Some" do
      expect(lazy_list[1, 2, 3].head).to eql(Dry::Monads::Some(1))
    end

    it "returns None for an empty list" do
      expect(lazy_list.empty.head).to eql(Dry::Monads::None())
    end
  end

  describe "#inspect" do
    it "marks lists that were not fully evaluated" do
      list = lazy_list.enumerate(counted_naturals([]))
      list.take(2)

      expect(list.inspect).to eql("LazyList[0, 1, ...]")
    end

    it "shows a fully evaluated list" do
      expect(lazy_list[1, 2].tap { _1.take(3) }.inspect).to eql("LazyList[1, 2]")
    end
  end

  describe "mixin" do
    it "is available via Dry::Monads[:lazy_list]" do
      klass = Class.new { include Dry::Monads[:lazy_list] }

      expect(klass.new.LazyList([1, 2]).take(2)).to eql([1, 2])
    end
  end
end
