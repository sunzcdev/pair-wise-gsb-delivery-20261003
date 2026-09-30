# frozen_string_literal: true

RSpec.describe 'injectable verification clock' do
  let(:base_time) { Time.at(1_609_459_200, 500, :millisecond) }

  # A clock that moves forward every single time it is observed. If exp, nbf
  # and iat were to read the clock independently they would see different
  # times and validation would become order dependent.
  let(:clock) do
    observed = base_time
    -> { observed += 1 }
  end

  describe JWT::Claims do
    let(:payload) do
      {
        'iat' => 1_609_459_200.5,
        'nbf' => 1_609_459_200.5,
        'exp' => 1_609_459_260.5
      }
    end

    it 'observes the clock exactly once for the whole verification round' do
      calls = 0
      counting_clock = -> { calls += 1; base_time }

      described_class.verify_payload!(payload, :exp, :nbf, :iat, clock: counting_clock)

      expect(calls).to eq(1)
    end

    it 'validates all time based claims against the same observation' do
      expect do
        described_class.verify_payload!(payload, :exp, :nbf, :iat, clock: clock)
      end.not_to raise_error
    end

    it 'evaluates boundary values against the fixed observation using fractional NumericDates' do
      boundary_payload = {
        'iat' => 1_609_459_200.5,
        'nbf' => 1_609_459_200.5,
        'exp' => 1_609_459_200.501
      }

      expect do
        described_class.verify_payload!(
          boundary_payload,
          :exp, :nbf, :iat,
          clock: base_time
        )
      end.not_to raise_error
    end

    it 'treats an exp equal to the observation time as expired even with fractional NumericDates' do
      boundary_payload = { 'exp' => 1_609_459_200.5 }

      expect do
        described_class.verify_payload!(boundary_payload, :exp, clock: base_time)
      end.to raise_error(JWT::ExpiredSignature)
    end

    it 'applies leeways only against the clock, not to the relations between claims' do
      reversed_payload = payload.merge('nbf' => 1_609_459_199.5)

      expect do
        described_class.verify_payload!(
          reversed_payload,
          :exp, :nbf, :iat,
          exp: { leeway: 1000 },
          nbf: { leeway: 1000 },
          iat: { leeway: 1000 },
          time_constraints: true,
          clock: base_time
        )
      end.to raise_error(JWT::InvalidTimeConstraintsError, /nbf is before iat/)
    end

    it 'reports ordering errors without raising through the predicate API' do
      reversed_payload = payload.merge('exp' => 1_609_459_199.5)

      expect(
        described_class.valid_payload?(
          reversed_payload,
          time_constraints: { max_token_lifetime: 1000 },
          clock: base_time
        )
      ).to be(false)
    end

    it 'enforces the maximum token lifetime at the exact boundary' do
      lifetime_payload = { 'iat' => 1_609_459_200.5, 'exp' => 1_609_459_260.5 }

      expect do
        described_class.verify_payload!(
          lifetime_payload,
          time_constraints: { max_token_lifetime: 60 },
          clock: base_time
        )
      end.not_to raise_error

      expect do
        described_class.verify_payload!(
          lifetime_payload,
          time_constraints: { max_token_lifetime: 59.999 },
          clock: base_time
        )
      end.to raise_error(JWT::InvalidTimeConstraintsError, /maximum/)
    end
  end

  describe JWT do
    let(:payload) do
      {
        'iat' => 1_609_459_200.5,
        'nbf' => 1_609_459_200.5,
        'exp' => 1_609_459_260.5
      }
    end
    let(:token) { JWT.encode(payload, 'secret') }

    it 'accepts the clock through the decode options and reads it once' do
      calls = 0
      counting_clock = -> { calls += 1; base_time }

      JWT.decode(
        token,
        'secret',
        true,
        {
          clock: counting_clock,
          verify_iat: true,
          verify_time_constraints: { max_token_lifetime: 60 }
        }
      )

      expect(calls).to eq(1)
    end

    it 'accepts a bare max_token_lifetime option' do
      expect do
        JWT.decode(
          token,
          'secret',
          true,
          clock: base_time,
          verify_iat: true,
          max_token_lifetime: 60
        )
      end.not_to raise_error

      expect do
        JWT.decode(
          token,
          'secret',
          true,
          clock: base_time,
          verify_iat: true,
          max_token_lifetime: 59.999
        )
      end.to raise_error(JWT::InvalidTimeConstraintsError, /maximum/)
    end

    it 'uses the configured global clock' do
      JWT.configure do |config|
        config.decode.clock = clock
        config.decode.verify_iat = true
      end

      expect { JWT.decode(token, 'secret') }.not_to raise_error
    end
  end

  describe JWT::EncodedToken do
    let(:payload) do
      {
        'iat' => 1_609_459_200.5,
        'nbf' => 1_609_459_200.5,
        'exp' => 1_609_459_260.5
      }
    end
    let(:encoded_token) { JWT::EncodedToken.new(JWT.encode(payload, 'secret')) }

    it 'verifies a token against a jumping clock read once' do
      expect(
        encoded_token.valid?(
          signature: { algorithm: 'HS256', key: 'secret' },
          claims: [:exp, :nbf, :iat, { time_constraints: { max_token_lifetime: 60 } }],
          clock: clock
        )
      ).to be(true)
    end
  end
end
