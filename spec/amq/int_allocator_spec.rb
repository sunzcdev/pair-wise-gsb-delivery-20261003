require "amq/int_allocator"
require "amq/bit_set"

RSpec.describe AMQ::IntAllocator do

  #
  # Environment
  #

  subject do
    described_class.new(1, 5)
  end

  # Collects the whole allocation set, not just the order, so tests compare against
  # the real range instead of an incidental sequence.
  def full_allocation_set(allocator)
    allocated = []
    exhausted = allocator.exhaustion_value

    while true
      reservation = allocator.allocate
      break if reservation == exhausted || reservation.nil?
      allocated << reservation.to_i
      break if allocated.length > allocator.capacity + 1
    end

    allocated
  end

  #
  # Examples
  #

  describe "#initialize" do
    it "raises ArgumentError when hi <= lo" do
      expect { described_class.new(5, 5) }.to raise_error(ArgumentError)
      expect { described_class.new(10, 5) }.to raise_error(ArgumentError)
    end

    it "raises ArgumentError for an empty inclusive range" do
      expect { described_class.new(5, 4, inclusive: true) }.to raise_error(ArgumentError)
      expect { described_class.new(1, 0, inclusive: true) }.to raise_error(ArgumentError)
    end

    it "raises TypeError for non-integer boundaries" do
      expect { described_class.new("1", 5) }.to raise_error(TypeError)
      expect { described_class.new(1, 5.0, inclusive: true) }.to raise_error(TypeError)
    end
  end

  describe "#number_of_bits" do
    it "returns number of bits available for allocation" do
      expect(subject.number_of_bits).to eq(4)
    end

    it "counts both boundaries in the inclusive mode" do
      expect(described_class.inclusive(1, 5).number_of_bits).to eq(5)
      expect(described_class.inclusive(7, 7).number_of_bits).to eq(1)
      expect(described_class.inclusive(-2, 2).number_of_bits).to eq(5)
    end

    it "is also exposed as #capacity" do
      expect(described_class.inclusive(0, 13).capacity).to eq(14)
    end
  end


  describe "#hi" do
    it "returns upper bound of the allocation range" do
      expect(subject.hi).to eq(5)
    end
  end

  describe "#lo" do
    it "returns lower bound of the allocation range" do
      expect(subject.lo).to eq(1)
    end
  end


  describe "#allocate" do
    context "when integer in the range is available" do
      it "returns allocated integer" do
        expect(subject.allocate).to eq(1)
        expect(subject.allocate).to eq(2)
        expect(subject.allocate).to eq(3)
        expect(subject.allocate).to eq(4)

        expect(subject.allocate).to eq(-1)
      end
    end

    context "when integer in the range IS NOT available" do
      it "returns -1" do
        4.times { subject.allocate }

        expect(subject.allocate).to eq(-1)
        expect(subject.allocate).to eq(-1)
        expect(subject.allocate).to eq(-1)
        expect(subject.allocate).to eq(-1)
      end
    end

    it "returns a value that behaves like the integer it represents" do
      reservation = described_class.inclusive(0, 3).allocate

      expect(reservation.to_i).to eq(0)
      expect(reservation).to eq(0)
      expect(0).to eq(reservation)
      expect([reservation]).to eq([0])
      expect(reservation + 1).to eq(1)
      expect(1 + reservation).to eq(1)
      expect("#{reservation}").to eq("0")
      expect([reservation, described_class.inclusive(0, 3).allocate].minmax.map(&:to_i)).to eq([0, 0])
    end
  end

  describe "closed interval mode" do
    it "allocates the whole lo..hi set when lo is below 1" do
      allocator = described_class.inclusive(0, 4)

      expect(full_allocation_set(allocator)).to eq([0, 1, 2, 3, 4])
      expect(allocator.allocate).to be_nil
    end

    it "allocates the whole lo..hi set when lo is well above 1" do
      allocator = described_class.inclusive(100, 103)

      expect(full_allocation_set(allocator)).to eq([100, 101, 102, 103])
      expect(allocator.allocate).to be_nil
    end

    it "allocates negative boundaries, including -1 itself" do
      allocator = described_class.inclusive(-3, -1)

      expect(allocator.exhaustion_value).to be_nil
      expect(full_allocation_set(allocator)).to eq([-3, -2, -1])
      expect(allocator.allocate).to be_nil
      expect(allocator.allocated?(-1)).to be_truthy
    end

    it "keeps -1 as the exhaustion marker of the historical mode" do
      expect(described_class.new(1, 3).exhaustion_value).to eq(-1)
    end

    it "supports a single value range" do
      allocator = described_class.inclusive(7, 7)

      expect(allocator.capacity).to eq(1)
      expect(allocator.allocate).to eq(7)
      expect(allocator.allocate).to be_nil
      expect(allocator.free_count).to eq(0)

      allocator.free(7)
      expect(allocator.allocate).to eq(7)
    end

    it "keeps hi and lo readable and reports the mode" do
      allocator = described_class.inclusive(-1, 1)

      expect(allocator.lo).to eq(-1)
      expect(allocator.hi).to eq(1)
      expect(allocator.inclusive?).to be_truthy
      expect(subject.inclusive?).to be_falsey
    end

    it "does not hand out the exclusive boundary of the historical mode" do
      allocator = described_class.new(1, 5)

      expect(full_allocation_set(allocator)).to eq([1, 2, 3, 4])
      expect(full_allocation_set(allocator)).not_to include(5)
    end
  end


  describe "#allocate_batch" do
    it "returns distinct IDs inside the range" do
      allocator = described_class.inclusive(10, 14)
      batch     = allocator.allocate_batch(3)

      expect(batch.length).to eq(3)
      expect(batch.map(&:to_i).uniq.length).to eq(3)
      batch.each do |reservation|
        expect((10..14)).to cover(reservation.to_i)
      end
    end

    it "takes the whole remaining capacity" do
      allocator = described_class.inclusive(2, 4)

      expect(allocator.allocate_batch(3).map(&:to_i)).to eq([2, 3, 4])
      expect(allocator.free_count).to eq(0)
      expect(allocator.allocate).to be_nil
    end

    it "leaves the bitmap untouched when capacity is insufficient" do
      allocator = described_class.inclusive(1, 4)
      allocator.allocate_batch(2)

      before = allocator.allocated_count
      expect(allocator.allocate_batch(3)).to be_nil
      expect(allocator.allocated_count).to eq(before)
      expect(allocator.free_count).to eq(2)

      expect(allocator.allocate_batch(2).map(&:to_i)).to eq([3, 4])
      expect(allocator.allocate_batch(1)).to be_nil
      expect(allocator.allocate_batch(0)).to eq([])
    end

    it "returns nil on an exhausted allocator" do
      allocator = described_class.inclusive(5, 5)
      allocator.allocate

      expect(allocator.allocate_batch(1)).to be_nil
    end

    it "wraps to earlier free positions when the cursor is past them" do
      allocator = described_class.inclusive(1, 6)
      (1..5).each { allocator.allocate }
      allocator.free(2)
      allocator.free(4)

      expect(allocator.allocate_batch(2).map(&:to_i).sort).to eq([2, 4])
    end

    it "rejects a negative batch size" do
      expect { subject.allocate_batch(-1) }.to raise_error(ArgumentError)
    end

    it "never returns duplicates even when mixed with single allocations" do
      allocator = described_class.inclusive(0, 9)
      single    = allocator.allocate.to_i
      batch     = allocator.allocate_batch(9).map(&:to_i)

      expect(([single] + batch).sort).to eq((0..9).to_a)
    end
  end


  describe "#free" do
    context "when the integer WAS allocated" do
      it "returns frees that integer" do
        4.times { subject.allocate }
        expect(subject.allocate).to eq(-1)

        subject.free(1)
        expect(subject.allocate).to eq(1)
        expect(subject.allocate).to eq(-1)
        subject.free(2)
        expect(subject.allocate).to eq(2)
        expect(subject.allocate).to eq(-1)
        subject.free(3)
        expect(subject.allocate).to eq(3)
        expect(subject.allocate).to eq(-1)
      end
    end

    context "when the integer WAS NOT allocated" do
      it "has no effect" do
        32.times { subject.free(1) }
        expect(subject.allocate).to eq(1)
      end
    end

    it "ignores out of range and duplicate releases without corrupting the bitmap" do
      allocator = described_class.inclusive(3, 6)
      allocator.allocate_batch(4)

      [2, 7, 0, -1, 100, nil, "3", 3, 3].each { |value| allocator.free(value) }

      expect(allocator.allocated_count).to eq(3)
      expect(allocator.allocated?(4)).to be_truthy
      expect(allocator.allocated?(5)).to be_truthy
      expect(allocator.allocated?(6)).to be_truthy
      expect(allocator.allocated?(3)).to be_falsey
      expect(allocator.allocate.to_i).to eq(3)
    end

    it "releases a whole batch with #free_batch" do
      allocator = described_class.inclusive(1, 4)
      batch     = allocator.allocate_batch(4)

      allocator.free_batch(batch)

      expect(allocator.allocated_count).to eq(0)
      expect(allocator.allocate_batch(4).map(&:to_i)).to eq([1, 2, 3, 4])
    end
  end


  describe "#allocated?" do
    context "when given position WAS allocated" do
      it "returns true" do
        3.times { subject.allocate }

        expect(subject.allocated?(1)).to be_truthy
        expect(subject.allocated?(2)).to be_truthy
        expect(subject.allocated?(3)).to be_truthy
      end
    end

    context "when given position WAS NOT allocated" do
      it "returns false" do
        2.times { subject.allocate }

        expect(subject.allocated?(3)).to be_falsey
        expect(subject.allocated?(4)).to be_falsey
      end
    end

    it "returns false outside the range instead of touching the bitmap" do
      allocator = described_class.inclusive(5, 6)

      expect(allocator.allocated?(4)).to be_falsey
      expect(allocator.allocated?(7)).to be_falsey
      expect(allocator.allocated?(nil)).to be_falsey
    end
  end

  describe "#release" do
    it "is an alias for #free" do
      subject.allocate
      expect(subject.allocated?(1)).to be_truthy
      subject.release(1)
      expect(subject.allocated?(1)).to be_falsey
    end
  end

  describe "#reset" do
    it "releases all allocations" do
      4.times { subject.allocate }
      expect(subject.allocate).to eq(-1)

      subject.reset
      expect(subject.allocate).to eq(1)
    end

    it "prevents a stale reservation from releasing the same ID of the next generation" do
      allocator = described_class.inclusive(1, 3)
      stale     = allocator.allocate

      allocator.reset
      fresh = allocator.allocate

      expect(stale.to_i).to eq(fresh.to_i)

      allocator.free(stale)

      expect(allocator.allocated?(fresh)).to be_truthy
      expect(allocator.allocated?(fresh.to_i)).to be_truthy
      expect(allocator.allocated_count).to eq(1)
      expect(allocator.free_count).to eq(2)
      expect(allocator.allocate_batch(2).map(&:to_i)).to eq([2, 3])
      expect(allocator.allocate).to be_nil

      allocator.free(fresh)
      expect(allocator.allocated_count).to eq(2)
      allocator.reset
      expect(allocator.allocated_count).to eq(0)
    end

    it "prevents stale batch reservations from releasing the next generation" do
      allocator = described_class.inclusive(5, 8)
      stale     = allocator.allocate_batch(2)

      allocator.reset
      fresh = allocator.allocate_batch(2)

      expect(stale.map(&:to_i)).to eq(fresh.map(&:to_i))

      allocator.free_batch(stale)
      expect(allocator.allocated_count).to eq(2)

      allocator.free_batch(fresh)
      expect(allocator.allocated_count).to eq(0)
    end

    it "does not let a reservation from another allocator affect the bitmap" do
      other    = described_class.inclusive(1, 2)
      foreign  = other.allocate

      subject.free(foreign)
      expect(subject.allocated_count).to eq(0)
      expect(subject.allocated?(foreign)).to be_falsey
    end

    it "restores the full free set and ignores stale allocated? queries" do
      allocator = described_class.inclusive(0, 2)
      stale     = allocator.allocate_batch(3)

      allocator.reset

      expect(allocator.free_count).to eq(3)
      expect(allocator.allocate_batch(3).map(&:to_i)).to eq([0, 1, 2])
      expect(stale).to all(satisfy { |reservation| !allocator.allocated?(reservation) })
    end
  end

  describe "dense allocate and free cycles" do
    it "does not rescan the range from the first bit on every allocation" do
      allocator = described_class.inclusive(0, 65_535)
      allocator.allocate_batch(65_536)
      allocator.free(65_535)

      words = allocator.instance_variable_get(:@free_set).instance_variable_get(:@words)
      reads = 0
      counter = Class.new(Array) do
        define_method(:[]) do |index|
          reads += 1
          super(index)
        end
      end
      allocator.instance_variable_get(:@free_set).instance_variable_set(:@words, counter.new(words))

      expect(allocator.allocate).to eq(65_535)
      expect(reads).to be <= 4
    end

    it "keeps handing out distinct values across repeated free and allocate rounds" do
      allocator = described_class.inclusive(1, 64)
      live      = {}

      1_000.times do
        if live.length >= 40 || (live.any? && rand < 0.5)
          value = live.keys.sample
          allocator.free(value)
          live.delete(value)
        else
          reservation = allocator.allocate
          expect(reservation).not_to eq(-1)
          expect((1..64)).to cover(reservation.to_i)
          expect(live).not_to have_key(reservation.to_i)
          live[reservation.to_i] = true
        end
      end

      expect(allocator.allocated_count).to eq(live.length)
      expect(allocator.free_count).to eq(64 - live.length)

      live.each_key { |value| expect(allocator.allocated?(value)).to be_truthy }
      allocator.reset
      expect(allocator.free_count).to eq(64)
    end

    it "exhausts and recovers the whole set repeatedly" do
      allocator = described_class.inclusive(-2, 5)
      expected  = (-2..5).to_a

      20.times do
        values = full_allocation_set(allocator)

        expect(values.sort).to eq(expected)
        allocator.free_batch(values)
        expect(allocator.free_count).to eq(expected.length)
      end
    end
  end
end



