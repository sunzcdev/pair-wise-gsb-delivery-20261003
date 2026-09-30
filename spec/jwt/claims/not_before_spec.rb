# frozen_string_literal: true

RSpec.describe JWT::Claims::NotBefore do
  let(:payload) { { 'nbf' => (Time.now.to_i + 5) } }
  let(:time) { nil }

  describe '#verify!' do
    context 'when nbf is in the future' do
      it 'raises JWT::ImmatureSignature' do
        expect { described_class.new(leeway: 0).verify!(context: SpecSupport::Token.new(payload: payload), time: time) }.to raise_error(JWT::ImmatureSignature)
      end
    end

    context 'when nbf is in the past' do
      let(:payload) { { 'nbf' => (Time.now.to_i - 5) } }

      it 'does not raise error' do
        expect { described_class.new(leeway: 0).verify!(context: SpecSupport::Token.new(payload: payload), time: time) }.not_to raise_error
      end
    end

    context 'when leeway is given' do
      it 'does not raise error' do
        expect { described_class.new(leeway: 10).verify!(context: SpecSupport::Token.new(payload: payload), time: time) }.not_to raise_error
      end
    end

    context 'with an injected observation time' do
      let(:time) { Time.at(1_609_459_200, 500, :millisecond) }

      context 'when nbf is exactly the observation time' do
        let(:payload) { { 'nbf' => 1_609_459_200.5 } }

        it 'does not raise error' do
          expect { described_class.new(leeway: 0).verify!(context: SpecSupport::Token.new(payload: payload), time: time) }.not_to raise_error
        end
      end

      context 'when nbf is a fraction in the future within leeway' do
        let(:payload) { { 'nbf' => 1_609_459_200.75 } }

        it 'does not raise error at the exact leeway boundary' do
          expect { described_class.new(leeway: 0.25).verify!(context: SpecSupport::Token.new(payload: payload), time: time) }.not_to raise_error
        end
      end

      context 'when nbf is a fraction in the future beyond leeway' do
        let(:payload) { { 'nbf' => 1_609_459_200.751 } }

        it 'raises JWT::ImmatureSignature' do
          expect { described_class.new(leeway: 0.25).verify!(context: SpecSupport::Token.new(payload: payload), time: time) }.to raise_error(JWT::ImmatureSignature)
        end
      end
    end
  end
end
