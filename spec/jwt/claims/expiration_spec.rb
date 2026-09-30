# frozen_string_literal: true

RSpec.describe JWT::Claims::Expiration do
  let(:now) { 1_609_459_200 }
  let(:payload) { { 'exp' => now + 5 } }
  let(:leeway) { 0 }

  subject(:verify!) { described_class.new(leeway: leeway).verify!(context: SpecSupport::Token.new(payload: payload), now: now) }

  context 'when token is expired' do
    let(:payload) { { 'exp' => now - 5 } }

    it 'must raise JWT::ExpiredSignature when the token has expired' do
      expect { verify! }.to(raise_error(JWT::ExpiredSignature))
    end
  end

  context 'when token is expired but some leeway is defined' do
    let(:payload) { { 'exp' => now - 5 } }
    let(:leeway) { 10 }

    it 'passes validation' do
      verify!
    end
  end

  context 'when token exp is set to current time' do
    let(:payload) { { 'exp' => now } }

    it 'fails validation' do
      expect { verify! }.to(raise_error(JWT::ExpiredSignature))
    end
  end

  context 'with a fractional NumericDate' do
    context 'when exp is exactly now' do
      let(:payload) { { 'exp' => now } }
      let(:now) { 1_609_459_200.5 }

      it 'fails validation' do
        expect { verify! }.to(raise_error(JWT::ExpiredSignature))
      end
    end

    context 'when exp equals now minus leeway' do
      let(:payload) { { 'exp' => now - 0.25 } }
      let(:now) { 1_609_459_200.5 }
      let(:leeway) { 0.25 }

      it 'fails validation at the expired boundary' do
        expect { verify! }.to(raise_error(JWT::ExpiredSignature))
      end
    end

    context 'when exp is within the leeway' do
      let(:payload) { { 'exp' => now - 0.2 } }
      let(:now) { 1_609_459_200.5 }
      let(:leeway) { 0.25 }

      it 'passes validation' do
        verify!
      end
    end

    context 'when exp is just past the leeway' do
      let(:payload) { { 'exp' => now - 0.26 } }
      let(:now) { 1_609_459_200.5 }
      let(:leeway) { 0.25 }

      it 'fails validation' do
        expect { verify! }.to(raise_error(JWT::ExpiredSignature))
      end
    end
  end

  context 'when an observation time is injected' do
    let(:verifier) { described_class.new(leeway: leeway) }

    it 'uses the injected time instead of reading the system clock' do
      expect { verifier.verify!(context: SpecSupport::Token.new(payload: { 'exp' => 100 }), now: 99) }.not_to raise_error
      expect { verifier.verify!(context: SpecSupport::Token.new(payload: { 'exp' => 100 }), now: 100) }.to raise_error(JWT::ExpiredSignature)
    end
  end

  context 'when token is not a Hash' do
    let(:payload) { 'beautyexperts_nbf_iat' }
    it 'passes validation' do
      verify!
    end
  end
end
