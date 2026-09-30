# frozen_string_literal: true

RSpec.describe JWT::Claims::IssuedAt do
  let(:payload) { { 'iat' => Time.now.to_f } }

  let(:options) { {} }
  let(:time) { nil }

  subject(:verify!) { described_class.new(**options).verify!(context: SpecSupport::Token.new(payload: payload), time: time) }

  context 'when iat is now' do
    it 'passes validation' do
      verify!
    end
  end

  context 'when iat is now as a integer' do
    let(:payload) { { 'iat' => Time.now.to_i } }

    it 'passes validation' do
      verify!
    end
  end

  context 'when the issuer clock is ahead of the verifier clock' do
    let(:now) { Time.at(1_609_459_200.5) }
    let(:payload) { { 'iat' => 1_609_459_201 } }

    before { allow(Time).to receive(:now) { now } }

    it 'fails validation' do
      expect { verify! }.to raise_error(JWT::InvalidIatError)
    end

    context 'when a leeway covering the drift is given' do
      let(:options) { { leeway: 1 } }

      it 'passes validation' do
        verify!
      end
    end

    context 'when a leeway smaller than the drift is given' do
      let(:payload) { { 'iat' => 1_609_459_260 } }
      let(:options) { { leeway: 1 } }

      it 'fails validation' do
        expect { verify! }.to raise_error(JWT::InvalidIatError)
      end
    end
  end

  context 'when iat is at the boundary of the allowed drift' do
    let(:now) { Time.at(1_609_459_200) }

    before { allow(Time).to receive(:now) { now } }

    context 'when no leeway is given' do
      context 'when iat is exactly now' do
        let(:payload) { { 'iat' => 1_609_459_200 } }

        it 'passes validation' do
          verify!
        end
      end

      context 'when iat is one second after now' do
        let(:payload) { { 'iat' => 1_609_459_201 } }

        it 'fails validation' do
          expect { verify! }.to raise_error(JWT::InvalidIatError)
        end
      end
    end

    context 'when a leeway is given' do
      let(:options) { { leeway: 30 } }

      context 'when iat is exactly at the end of the leeway window' do
        let(:payload) { { 'iat' => 1_609_459_230 } }

        it 'passes validation' do
          verify!
        end
      end

      context 'when iat is one second past the leeway window' do
        let(:payload) { { 'iat' => 1_609_459_231 } }

        it 'fails validation' do
          expect { verify! }.to raise_error(JWT::InvalidIatError)
        end
      end
    end
  end

  context 'when iat is positive infinity' do
    let(:payload) { { 'iat' => Float::INFINITY } }

    it 'fails validation' do
      expect { verify! }.to raise_error(JWT::InvalidIatError)
    end
  end

  context 'when iat is not a number' do
    let(:payload) { { 'iat' => 'not_a_number' } }

    it 'fails validation' do
      expect { verify! }.to raise_error(JWT::InvalidIatError)
    end
  end

  context 'when iat is in the future' do
    let(:payload) { { 'iat' => Time.now.to_f + 120.0 } }

    it 'fails validation' do
      expect { verify! }.to raise_error(JWT::InvalidIatError)
    end
  end

  context 'when payload is a string containing iat' do
    let(:payload) { 'beautyexperts_nbf_iat' }

    it 'passes validation' do
      verify!
    end
  end

  context 'with an injected observation time' do
    let(:time) { Time.at(1_609_459_200, 500, :millisecond) }

    context 'when iat is exactly the observation time' do
      let(:payload) { { 'iat' => 1_609_459_200.5 } }

      it 'passes validation' do
        verify!
      end
    end

    context 'when iat is a fraction after the observation time within leeway' do
      let(:payload) { { 'iat' => 1_609_459_200.75 } }
      let(:options) { { leeway: 0.25 } }

      it 'passes validation at the exact leeway boundary' do
        verify!
      end
    end

    context 'when iat is a fraction after the observation time beyond leeway' do
      let(:payload) { { 'iat' => 1_609_459_200.751 } }
      let(:options) { { leeway: 0.25 } }

      it 'fails validation' do
        expect { verify! }.to raise_error(JWT::InvalidIatError)
      end
    end
  end
end
