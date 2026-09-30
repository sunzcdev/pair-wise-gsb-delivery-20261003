# frozen_string_literal: true

RSpec.describe JWT::Claims::Expiration do
  let(:payload) { { 'exp' => (Time.now.to_i + 5) } }
  let(:leeway) { 0 }
  let(:time) { nil }

  subject(:verify!) { described_class.new(leeway: leeway).verify!(context: SpecSupport::Token.new(payload: payload), time: time) }

  context 'when token is expired' do
    let(:payload) { { 'exp' => (Time.now.to_i - 5) } }

    it 'must raise JWT::ExpiredSignature when the token has expired' do
      expect { verify! }.to(raise_error(JWT::ExpiredSignature))
    end
  end

  context 'when token is expired but some leeway is defined' do
    let(:payload) { { 'exp' => (Time.now.to_i - 5) } }
    let(:leeway) { 10 }

    it 'passes validation' do
      verify!
    end
  end

  context 'when token exp is set to current time' do
    let(:payload) { { 'exp' => Time.now.to_i } }

    it 'fails validation' do
      expect { verify! }.to(raise_error(JWT::ExpiredSignature))
    end
  end

  context 'when token is not a Hash' do
    let(:payload) { 'beautyexperts_nbf_iat' }
    it 'passes validation' do
      verify!
    end
  end

  context 'with an injected observation time' do
    let(:time) { Time.at(1_609_459_200, 500, :millisecond) }

    context 'when exp is exactly the observation time' do
      let(:payload) { { 'exp' => 1_609_459_200.5 } }

      it 'fails validation because the boundary itself is expired' do
        expect { verify! }.to raise_error(JWT::ExpiredSignature)
      end
    end

    context 'when exp is a fraction after the observation time' do
      let(:payload) { { 'exp' => 1_609_459_200.501 } }

      it 'passes validation' do
        verify!
      end
    end

    context 'when exp sits exactly on the leeway boundary' do
      let(:payload) { { 'exp' => 1_609_459_200.25 } }
      let(:leeway) { 0.25 }

      it 'fails validation because equality is still expired' do
        expect { verify! }.to raise_error(JWT::ExpiredSignature)
      end
    end

    context 'when exp is within the leeway by a fraction' do
      let(:payload) { { 'exp' => 1_609_459_200.251 } }
      let(:leeway) { 0.25 }

      it 'passes validation' do
        verify!
      end
    end
  end
end
