# frozen_string_literal: true

RSpec.describe JWT::Claims::Chronology do
  let(:payload) do
    {
      'iat' => now - 10,
      'nbf' => now - 5,
      'exp' => now + 60
    }
  end
  let(:now) { 1_609_459_200 }

  subject(:verify!) { described_class.new(**options).verify!(context: SpecSupport::Token.new(payload: payload), now: now) }

  describe 'claim order' do
    context 'when the claims are in chronological order' do
      let(:options) { {} }

      it 'passes validation' do
        verify!
      end
    end

    context 'when iat, nbf and exp are identical' do
      let(:payload) { { 'iat' => now, 'nbf' => now, 'exp' => now } }
      let(:options) { {} }

      it 'passes validation' do
        verify!
      end
    end

    context 'when nbf is earlier than iat within the leeway' do
      let(:payload) { { 'iat' => now, 'nbf' => now - 1, 'exp' => now + 60 } }
      let(:options) { { leeway: 1 } }

      it 'passes validation at the leeway boundary' do
        verify!
      end
    end

    context 'when nbf is earlier than iat beyond the leeway' do
      let(:payload) { { 'iat' => now, 'nbf' => now - 1.5, 'exp' => now + 60 } }
      let(:options) { { leeway: 1 } }

      it 'fails validation' do
        expect { verify! }.to raise_error(JWT::InvalidChronologyError, /'iat' .* is after 'nbf'/)
      end
    end

    context 'when nbf is after exp without leeway' do
      let(:payload) { { 'iat' => now - 10, 'nbf' => now + 0.5, 'exp' => now + 0.25 } }
      let(:options) { {} }

      it 'fails validation even when both timestamps are in the future' do
        expect { verify! }.to raise_error(JWT::InvalidChronologyError, /'nbf' .* is after 'exp'/)
      end
    end

    context 'when exp is before iat' do
      let(:payload) { { 'iat' => now, 'exp' => now - 1 } }
      let(:options) { {} }

      it 'fails validation regardless of the observation time' do
        expect { verify! }.to raise_error(JWT::InvalidChronologyError, /'iat' .* is after 'exp'/)
      end
    end

    context 'when only some of the claims are present' do
      let(:payload) { { 'iat' => now + 10, 'exp' => now + 20 } }
      let(:options) { {} }

      it 'only checks the present pairs' do
        verify!
      end
    end

    context 'with fractional NumericDate values' do
      let(:payload) { { 'iat' => now - 10.25, 'nbf' => now - 10.5, 'exp' => now + 0.5 } }

      context 'when the inversion is smaller than the leeway' do
        let(:options) { { leeway: 0.25 } }

        it 'passes validation at the fractional boundary' do
          verify!
        end
      end

      context 'when the inversion is larger than the leeway' do
        let(:options) { { leeway: 0.2 } }

        it 'fails validation' do
          expect { verify! }.to raise_error(JWT::InvalidChronologyError)
        end
      end
    end
  end

  describe 'maximum token age' do
    let(:options) { { max_age: 10 } }

    context 'when the token age is exactly the maximum age' do
      let(:payload) { { 'iat' => now - 10 } }

      it 'passes validation at the boundary' do
        verify!
      end
    end

    context 'when the token is older than the maximum age' do
      let(:payload) { { 'iat' => now - 10.01 } }

      it 'fails validation' do
        expect { verify! }.to raise_error(JWT::InvalidMaxAgeError, /maximum allowed age/)
      end
    end

    context 'when a leeway is given' do
      let(:options) { { max_age: 10, leeway: 0.5 } }

      context 'when the token age equals max age plus leeway' do
        let(:payload) { { 'iat' => now - 10.5 } }

        it 'passes validation at the boundary' do
          verify!
        end
      end

      context 'when the token exceeds max age plus leeway' do
        let(:payload) { { 'iat' => now - 10.75 } }

        it 'fails validation' do
          expect { verify! }.to raise_error(JWT::InvalidMaxAgeError)
        end
      end
    end

    context 'when the payload has no iat' do
      let(:payload) { { 'exp' => now + 10 } }

      it 'skips the age check' do
        verify!
      end
    end

    context 'when now is given as a Time' do
      let(:observed) { Time.at(now.to_f) }

      subject(:verify!) { described_class.new(**options).verify!(context: SpecSupport::Token.new(payload: { 'iat' => now - 10 }), now: observed) }

      it 'passes validation' do
        verify!
      end
    end
  end
end

