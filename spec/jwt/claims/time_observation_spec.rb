# frozen_string_literal: true

RSpec.describe 'time based claim verification' do
  let(:base_time) { 1_609_459_200.0 }
  let(:clock) { SpecSupport::JumpingClock.new(start: base_time, step: 100) }

  describe 'a single observation time per verification round' do
    context 'with the standalone Claims API' do
      let(:payload) { { 'iat' => base_time, 'nbf' => base_time, 'exp' => base_time + 1 } }

      it 'calls the clock once even when every time claim is verified' do
        expect do
          JWT::Claims.verify_payload!(payload, :exp, :nbf, :iat, :chronology, clock: clock)
        end.not_to raise_error
        expect(clock.call_count).to eq(1)
      end

      context 'with a clock that moves into the past on every observation' do
        let(:clock) { SpecSupport::JumpingClock.new(start: base_time, step: -100) }

        it 'uses the first observation for exp and does not let later reads revive the token' do
          # With a single observation at base_time the token has just expired
          # (exp == now). Reading the clock again for exp would move the clock
          # backwards and wrongly accept it.
          expired_payload = payload.merge('exp' => base_time)
          expect do
            JWT::Claims.verify_payload!(expired_payload, :exp, :nbf, :iat, clock: clock)
          end.to raise_error(JWT::ExpiredSignature)
          expect(clock.call_count).to eq(1)
        end
      end

      context 'with a clock that moves into the future on every observation' do
        let(:clock) { SpecSupport::JumpingClock.new(start: base_time, step: 100) }

        it 'uses the first observation for nbf and iat instead of accepting drift caused by re-reads' do
          future_payload = payload.merge('iat' => base_time + 0.5, 'nbf' => base_time + 0.5)
          errors = JWT::Claims.payload_errors(future_payload, :exp, :nbf, :iat, clock: clock)
          expect(errors.map { |error| error.message }).to contain_exactly(
            'Signature nbf has not been reached',
            'Invalid iat'
          )
          expect(clock.call_count).to eq(1)
        end
      end
    end

    context 'with JWT.decode' do
      let(:payload) do
        {
          'iat' => base_time,
          'nbf' => base_time,
          'exp' => base_time + 10
        }
      end
      let(:token) { JWT.encode(payload, 'secret', 'HS256') }

      it 'shares one observation between the decode claim verifiers' do
        expect do
          JWT.decode(token, 'secret', true,
                     algorithm: 'HS256',
                     verify_iat: true,
                     verify_chronology: true,
                     clock: clock)
        end.not_to raise_error
        expect(clock.call_count).to eq(1)
      end

      it 'does not let a forward jumping clock make a future nbf valid' do
        future_payload = payload.merge('nbf' => base_time + 50)
        future_token = JWT.encode(future_payload, 'secret', 'HS256')

        expect do
          JWT.decode(future_token, 'secret', true, algorithm: 'HS256', clock: clock)
        end.to raise_error(JWT::ImmatureSignature)
        expect(clock.call_count).to eq(1)
      end
    end

    context 'with JWT::EncodedToken' do
      let(:payload) do
        {
          'iat' => base_time,
          'nbf' => base_time,
          'exp' => base_time + 10
        }
      end
      let(:encoded_token) do
        token = JWT::Token.new(payload: payload)
        token.sign!(algorithm: 'HS256', key: 'secret')
        JWT::EncodedToken.new(token.jwt)
      end

      before { encoded_token.verify_signature!(algorithm: 'HS256', key: 'secret') }

      it 'shares one observation across the requested time claims' do
        encoded_token.verify_claims!(:exp, :nbf, :iat, :chronology, clock: clock)
        expect(clock.call_count).to eq(1)
      end

      it 'collects the time claim errors against the same observation' do
        expired_payload = payload.merge('exp' => base_time)
        token = JWT::Token.new(payload: expired_payload)
        token.sign!(algorithm: 'HS256', key: 'secret')
        expired_token = JWT::EncodedToken.new(token.jwt)
        expired_token.verify_signature!(algorithm: 'HS256', key: 'secret')

        errors = expired_token.claim_errors(:exp, :nbf, :iat, clock: clock)
        expect(errors.map(&:message)).to eq(['Signature has expired'])
        expect(clock.call_count).to eq(1)
      end
    end
  end

  describe 'globally configured clock' do
    after { JWT.configuration.reset! }

    it 'is used by JWT.decode unless a per-call clock overrides it' do
      expired_token = JWT.encode({ 'exp' => base_time + 5 }, 'secret', 'HS256')

      JWT.configure { |config| config.decode.clock = -> { base_time + 10 } }
      expect { JWT.decode(expired_token, 'secret', true, algorithm: 'HS256') }.to raise_error(JWT::ExpiredSignature)

      expect do
        JWT.decode(expired_token, 'secret', true, algorithm: 'HS256', clock: -> { base_time })
      end.not_to raise_error
    end
  end

  describe 'maximum token lifetime' do
    let(:payload) { { 'iat' => base_time - 30, 'exp' => base_time + 30 } }

    context 'with the standalone Claims API' do
      it 'accepts a token at exactly the maximum age' do
        boundary_payload = { 'iat' => base_time - 10, 'exp' => base_time + 10 }
        JWT::Claims.verify_payload!(boundary_payload, chronology: { max_age: 10 }, clock: -> { base_time })
      end

      it 'rejects a token older than the maximum age' do
        expect do
          JWT::Claims.verify_payload!(payload, chronology: { max_age: 20 }, clock: -> { base_time })
        end.to raise_error(JWT::InvalidMaxAgeError, /maximum allowed age of 20/)
      end
    end

    context 'with JWT.decode' do
      let(:token) { JWT.encode(payload, 'secret', 'HS256') }

      it 'enforces max_token_age against the injected clock' do
        expect do
          JWT.decode(token, 'secret', true,
                     algorithm: 'HS256',
                     max_token_age: 20,
                     clock: -> { base_time })
        end.to raise_error(JWT::InvalidMaxAgeError)
      end

      it 'accepts a token within max_token_age and applies the leeway at the boundary' do
        boundary_token = JWT.encode({ 'iat' => base_time - 10.25, 'exp' => base_time + 10 }, 'secret', 'HS256')

        expect do
          JWT.decode(boundary_token, 'secret', true,
                     algorithm: 'HS256',
                     max_token_age: 10,
                     chronology_leeway: 0.25,
                     clock: -> { base_time })
        end.not_to raise_error
      end
    end
  end

  describe 'iat, nbf and exp relation constraints' do
    context 'with JWT.decode' do
      it 'rejects an exp before iat regardless of the leeway sized inversion' do
        token = JWT.encode({ 'iat' => base_time, 'exp' => base_time - 1 }, 'secret', 'HS256')

        expect do
          JWT.decode(token, 'secret', true,
                     algorithm: 'HS256',
                     verify_chronology: true,
                     leeway: 0.5,
                     clock: -> { base_time - 10 })
        end.to raise_error(JWT::InvalidChronologyError, /'iat' .* is after 'exp'/)
      end

      it 'tolerates an inversion covered by the chronology leeway' do
        token = JWT.encode({ 'iat' => base_time, 'nbf' => base_time - 0.25, 'exp' => base_time + 10 }, 'secret', 'HS256')

        expect do
          JWT.decode(token, 'secret', true,
                     algorithm: 'HS256',
                     verify_chronology: true,
                     chronology_leeway: 0.25,
                     clock: -> { base_time })
        end.not_to raise_error
      end
    end
  end
end

