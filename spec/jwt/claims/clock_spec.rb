# frozen_string_literal: true

RSpec.describe JWT::Claims::Clock do
  describe '.resolve' do
    it 'returns a Time unchanged' do
      time = Time.at(1_609_459_200)
      expect(described_class.resolve(time)).to eq(time)
    end

    it 'converts an integer NumericDate to a Time' do
      expect(described_class.resolve(1_609_459_200)).to eq(Time.at(1_609_459_200))
    end

    it 'converts a fractional NumericDate to a Time preserving the fraction' do
      expect(described_class.resolve(1_609_459_200.5).to_f).to eq(1_609_459_200.5)
    end

    it 'calls a callable clock once and resolves its result' do
      clock = SpecSupport::JumpingClock.new(start: 1_609_459_200.0, step: 0.25)
      expect(described_class.resolve(clock).to_f).to eq(1_609_459_200.0)
      expect(clock.call_count).to eq(1)
    end

    it 'raises an ArgumentError for an unusable clock' do
      expect { described_class.resolve('not a clock') }.to raise_error(ArgumentError, /clock must be a Time/)
    end

    it 'raises an ArgumentError when a callable clock returns an unusable value' do
      expect { described_class.resolve(-> { 'not a time' }) }.to raise_error(ArgumentError, /clock must be a Time/)
    end
  end
end
