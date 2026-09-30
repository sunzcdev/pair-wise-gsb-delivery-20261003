# frozen_string_literal: true

RSpec.describe JWT::Claims::TimeConstraints do
  let(:now) { ::Time.at(1_609_459_200.5) }
  let(:context) { SpecSupport::Token.new(payload: payload) }

  subject(:verify!) { described_class.new(**options).verify!(context: context, time: now) }

  let(:options) { {} }

  context 'claim ordering' do
    context 'when iat, nbf and exp are in order' do
      let(:payload) { { 'iat' => 100.0, 'nbf' => 100.5, 'exp' => 200.25 } }

      it 'passes validation' do
        verify!
      end
    end

    context 'when adjacent claims are equal' do
      let(:payload) { { 'iat' => 100, 'nbf' => 100, 'exp' => 100 } }

      it 'passes validation' do
        verify!
      end
    end

    context 'when nbf is before iat' do
      let(:payload) { { 'iat' => 100.5, 'nbf' => 100.25, 'exp' => 200 } }

      it 'fails validation even though no claim is checked against the clock' do
        expect { verify! }.to raise_error(JWT::InvalidTimeConstraintsError, /nbf is before iat/)
      end
    end

    context 'when exp is before nbf' do
      let(:payload) { { 'nbf' => 100.5, 'exp' => 100.25 } }

      it 'fails validation' do
        expect { verify! }.to raise_error(JWT::InvalidTimeConstraintsError, /exp is before nbf/)
      end
    end

    context 'when exp is before iat' do
      let(:payload) { { 'iat' => 100.5, 'exp' => 100.25 } }

      it 'fails validation' do
        expect { verify! }.to raise_error(JWT::InvalidTimeConstraintsError, /exp is before iat/)
      end
    end

    context 'when the claims are missing or non numeric' do
      let(:payload) { { 'iat' => 'not numeric', 'exp' => '200' } }

      it 'skips the comparisons' do
        verify!
      end
    end

    context 'when the payload is not a Hash' do
      let(:context) { SpecSupport::Token.new(payload: 'not-a-hash') }

      it 'passes validation' do
        verify!
      end
    end
  end

  context 'maximum token lifetime' do
    let(:options) { { max_token_lifetime: 100 } }

    context 'when exp - iat is exactly at the maximum' do
      let(:payload) { { 'iat' => 1_609_459_100.5, 'exp' => 1_609_459_200.5 } }

      it 'passes validation' do
        verify!
      end
    end

    context 'when exp - iat exceeds the maximum by a fraction' do
      let(:payload) { { 'iat' => 1_609_459_100.5, 'exp' => 1_609_459_200.75 } }

      it 'fails validation' do
        expect { verify! }.to raise_error(JWT::InvalidTimeConstraintsError, /maximum/)
      end
    end

    context 'when exp - iat is within the maximum' do
      let(:payload) { { 'iat' => 1_609_459_150, 'exp' => 1_609_459_200 } }

      it 'passes validation' do
        verify!
      end
    end

    context 'when iat or exp is missing' do
      let(:payload) { { 'exp' => 1_609_459_200 } }

      it 'does not enforce the lifetime' do
        verify!
      end
    end
  end
end
