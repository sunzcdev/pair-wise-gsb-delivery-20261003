# frozen_string_literal: true

RSpec.describe Shoryuken::Helpers::WorkerSlots do
  describe '#initialize' do
    it 'rejects a non-positive capacity' do
      expect { described_class.new(0) }.to raise_error(ArgumentError, /positive/)
    end

    it 'starts with every slot available' do
      slots = described_class.new(3)

      expect(slots.capacity).to eq(3)
      expect(slots.available).to eq(3)
      expect(slots.reserved).to eq(0)
      expect(slots.leased).to eq(0)
      expect(slots.occupied).to eq(0)
    end
  end

  describe '#reserve' do
    subject(:slots) { described_class.new(3) }

    it 'reserves the requested number of free slots before a fetch' do
      expect(slots.reserve(3)).to eq(3)
      expect(slots.reserved).to eq(3)
      expect(slots.available).to eq(0)
    end

    it 'never reserves more than what is available' do
      slots.reserve(2)

      expect(slots.reserve(3)).to eq(1)
      expect(slots.reserved).to eq(3)
      expect(slots.available).to eq(0)
    end

    it 'returns zero and reserves nothing when the pool is full' do
      slots.reserve(3)

      expect(slots.reserve(2)).to eq(0)
      expect(slots.reserved).to eq(3)
    end

    it 'rejects a negative request' do
      expect { slots.reserve(-1) }.to raise_error(ArgumentError)
    end

    it 'counts leased slots as unavailable for reservation' do
      slots.reserve(2)
      slots.occupy
      slots.occupy

      expect(slots.leased).to eq(2)
      expect(slots.available).to eq(1)
      expect(slots.reserve(2)).to eq(1)
    end
  end

  describe '#occupy' do
    subject(:slots) { described_class.new(3) }

    it 'turns a reservation into a worker lease' do
      slots.reserve(2)

      expect(slots.occupy).to eq(1)
      expect(slots.reserved).to eq(1)
      expect(slots.leased).to eq(1)
      expect(slots.available).to eq(1)

      slots.occupy
      expect(slots.reserved).to eq(0)
      expect(slots.leased).to eq(2)
    end

    it 'can lease a free slot directly, without a reservation' do
      expect(slots.occupy).to eq(1)
      expect(slots.leased).to eq(1)
      expect(slots.available).to eq(2)
    end

    it 'raises when the pool is full and nothing is reserved' do
      3.times { slots.occupy }

      expect { slots.occupy }.to raise_error(/none reserved and the pool is full/)
    end
  end

  describe '#release_reservation' do
    subject(:slots) { described_class.new(3) }

    it 'returns unfilled reservations so the next fetch can use them' do
      slots.reserve(3)

      expect(slots.release_reservation(2)).to eq(1)
      expect(slots.reserved).to eq(1)
      expect(slots.available).to eq(2)
    end

    it 'raises when releasing more than is reserved' do
      slots.reserve(1)

      expect { slots.release_reservation(2) }.to raise_error(/only 1 are reserved/)
    end
  end

  describe '#release_if_possible' do
    subject(:slots) { described_class.new(2) }

    it 'does not drive the lease count below zero' do
      expect(slots.release_if_possible).to eq(0)

      slots.occupy

      expect(slots.release_if_possible).to eq(0)
      expect(slots.leased).to eq(0)
    end
  end

  describe '#release' do
    subject(:slots) { described_class.new(2) }

    it 'frees a finished worker lease' do
      slots.reserve(2)
      slots.occupy
      slots.occupy

      expect(slots.release).to eq(1)
      expect(slots.leased).to eq(1)
      expect(slots.available).to eq(1)
    end

    it 'raises when no slot is leased' do
      expect { slots.release }.to raise_error(/not leased/)
    end
  end

  describe 'the reservation lifecycle' do
    it 'keeps reserved + leased within capacity and recycles freed slots' do
      slots = described_class.new(3)

      # A fetch reserves all three slots but the server only returns one message.
      slots.reserve(3)
      slots.occupy # the single returned message becomes a worker
      slots.release_reservation(2) # the two unfilled slots go back

      expect(slots.leased).to eq(1)
      expect(slots.available).to eq(2)

      # Another queue can immediately reserve the freed slots.
      expect(slots.reserve(2)).to eq(2)
      expect(slots.occupied).to eq(3)

      # The first worker finishes, opening another slot.
      slots.release
      expect(slots.available).to eq(1)
      expect(slots.occupied).to eq(2)
    end
  end

  describe 'under concurrent access' do
    it 'never over-commits the pool' do
      slots = described_class.new(5)
      errors = []

      threads = Array.new(8) do
        Thread.new do
          200.times do
            # Each thread only touches the slots it reserved itself, so reservations and leases stay balanced per
            # thread while the shared pool is hammered.
            taken = slots.reserve(2)
            next if taken.zero?

            # Lease half of the reservation as workers, give the other half straight back unfilled.
            leased = taken / 2
            unfilled = taken - leased

            leased.times { slots.occupy }
            slots.release_reservation(unfilled)
            sleep(0)
            leased.times { slots.release }
          end
        rescue => e
          errors << e
        end
      end
      threads.each(&:join)

      expect(errors).to be_empty
      expect(slots.occupied).to eq(0)
      expect(slots.available).to eq(5)
    end
  end
end
