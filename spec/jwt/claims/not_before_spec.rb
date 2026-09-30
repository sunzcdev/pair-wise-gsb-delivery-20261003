# frozen_string_literal: true

RSpec.describe JWT::Claims::NotBefore do
  let(:now) { 1_609_459_200 }
  let(:payload) { { 'nbf' => now + 5 } }

  describe '#verify!' do
    context 'when nbf is in the future' do
      it 'raises JWT::ImmatureSignature' do
        expect { described_class.new(leeway: 0).verify!(context: SpecSupport::Token.new(payload: payload), now: now) }.to raise_error(JWT::ImmatureSignature)
      end
    end

    context 'when nbf is in the past' do
      let(:payload) { { 'nbf' => now - 5 } }

      it 'does not raise error' do
        expect { described_class.new(leeway: 0).verify!(context: SpecSupport::Token.new(payload: payload), now: now) }.not_to raise_error
      end
    end

    context 'when leeway is given' do
      it 'does not raise error' do
        expect { described_class.new(leeway: 10).verify!(context: SpecSupport::Token.new(payload: payload), now: now) }.not_to raise_error
      end
    end

    context 'when nbf is exactly now' do
      let(:payload) { { 'nbf' => now } }

      it 'passes validation at the boundary' do
        expect { described_class.new(leeway: 0).verify!(context: SpecSupport::Token.new(payload: payload), now: now) }.not_to raise_error
      end
    end

    context 'with a fractional NumericDate' do
      let(:now) { 1_609_459_200.5 }

      context 'when nbf equals now plus leeway' do
        let(:payload) { { 'nbf' => now + 0.25 } }

        it 'passes validation at the leeway boundary' do
          expect { described_class.new(leeway: 0.25).verify!(context: SpecSupport::Token.new(payload: payload), now: now) }.not_to raise_error
        end
      end

      context 'when nbf is just past the leeway' do
        let(:payload) { { 'nbf' => now + 0.3 } }

        it 'fails validation' do
          expect { described_class.new(leeway: 0.25).verify!(context: SpecSupport::Token.new(payload: payload), now: now) }.to raise_error(JWT::ImmatureSignature)
        end
      end
    end
  end
end
