# frozen_string_literal: true

RSpec.describe JWT::Claims::Clock do
  describe '.resolve' do
    it 'calls the clock exactly once' do
      calls = 0
      clock = -> do
        calls += 1
        ::Time.at(1_609_459_200)
      end

      expect(described_class.resolve(clock)).to eq(::Time.at(1_609_459_200))
      expect(calls).to eq(1)
    end

    it 'returns a given Time as is' do
      time = ::Time.at(1_609_459_200.5)
      expect(described_class.resolve(time)).to eq(time)
    end

    it 'supports a fractional NumericDate' do
      expect(described_class.resolve(1_609_459_200.25)).to eq(::Time.at(1_609_459_200.25))
    end

    it 'falls back to the default clock when nothing is given' do
      time = ::Time.at(1_609_459_200)
      allow(::Time).to receive(:now).and_return(time)

      expect(described_class.resolve(nil)).to eq(time)
    end
  end
end
