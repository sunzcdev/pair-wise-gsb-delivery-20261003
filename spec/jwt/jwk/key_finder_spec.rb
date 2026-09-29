# frozen_string_literal: true

RSpec.describe JWT::JWK::KeyFinder do
  let(:rsa_key) { test_pkey('rsa-2048-private.pem') }
  let(:other_rsa_key) { OpenSSL::PKey.read(File.read(File.join(__dir__, '..', '..', 'fixtures', 'keys', 'rsa-2048-wrong-public.pem'))) }
  let(:ec_key) { test_pkey('ec256-private.pem') }

  let(:kid) { 'shared-kid' }
  let(:rsa_jwk) { JWT::JWK.new(rsa_key, rsa_params) }
  let(:ec_jwk) { JWT::JWK.new(ec_key, ec_params) }
  let(:rsa_params) { { kid: kid } }
  let(:ec_params) { { kid: kid } }
  let(:jwks) { { keys: [rsa_jwk.export, ec_jwk.export] } }

  let(:finder) { described_class.new(jwks: jwks, verification_policy: verification_policy_option) }
  let(:verification_policy_option) { true }

  describe 'legacy behaviour without a verification policy' do
    let(:verification_policy_option) { nil }

    it 'returns the first key matching the kid, ignoring kty and order dependence' do
      expect(JWT::JWK.new(finder.key_for(kid)).members).to eq(rsa_jwk.members)
    end

    context 'when the EC key is listed first' do
      let(:jwks) { { keys: [ec_jwk.export, rsa_jwk.export] } }

      it 'returns the EC key' do
        expect(JWT::JWK.new(finder.key_for(kid)).members).to eq(ec_jwk.members)
      end
    end

    context 'when the first key is declared for encryption use' do
      let(:rsa_params) { { kid: kid, use: 'enc' } }

      it 'still returns the first key' do
        expect(JWT::JWK.new(finder.key_for(kid)).members).to eq(rsa_jwk.members)
      end
    end
  end

  describe 'verification policy' do
    it 'selects the RSA key for an RSA algorithm' do
      expect(JWT::JWK.new(finder.key_for(kid, algorithm: 'RS256')).members).to eq(rsa_jwk.members)
    end

    it 'selects the EC key for an EC algorithm' do
      expect(JWT::JWK.new(finder.key_for(kid, algorithm: 'ES256')).members).to eq(ec_jwk.members)
    end

    context 'when the EC key is listed first' do
      let(:jwks) { { keys: [ec_jwk.export, rsa_jwk.export] } }

      it 'still selects the RSA key for an RSA algorithm' do
        expect(JWT::JWK.new(finder.key_for(kid, algorithm: 'RS256')).members).to eq(rsa_jwk.members)
      end

      it 'still selects the EC key for an EC algorithm' do
        expect(JWT::JWK.new(finder.key_for(kid, algorithm: 'ES256')).members).to eq(ec_jwk.members)
      end
    end

    it 'requires an algorithm' do
      expect { finder.key_for(kid) }.to raise_error(ArgumentError, /algorithm is required/i)
    end

    context 'when keys declare a use' do
      let(:rsa_params) { { kid: kid, use: 'enc' } }
      let(:jwks) { { keys: [rsa_jwk.export, sig_rsa_jwk.export] } }
      let(:sig_rsa_jwk) { JWT::JWK.new(other_rsa_key, kid: kid, use: 'sig') }

      it 'skips keys declared for encryption' do
        expect(JWT::JWK.new(finder.key_for(kid, algorithm: 'RS256')).members).to eq(sig_rsa_jwk.members)
      end
    end

    context 'when keys declare key_ops' do
      let(:rsa_params) { { kid: kid, key_ops: %w[encrypt] } }
      let(:jwks) { { keys: [rsa_jwk.export, verify_rsa_jwk.export] } }
      let(:verify_rsa_jwk) { JWT::JWK.new(other_rsa_key, kid: kid, key_ops: %w[verify]) }

      it 'skips keys without the verify operation' do
        expect(JWT::JWK.new(finder.key_for(kid, algorithm: 'RS256')).members).to eq(verify_rsa_jwk.members)
      end
    end

    context 'when no key qualifies' do
      let(:rsa_params) { { kid: kid, use: 'enc' } }
      let(:ec_params) { { kid: kid, use: 'enc' } }

      it 'raises a SignatureError' do
        expect { finder.key_for(kid, algorithm: 'RS256') }.to raise_error(JWT::SignatureError, "Could not find public key for kid #{kid}")
      end
    end

    context 'when the alg member does not match the token algorithm' do
      let(:rsa_params) { { kid: kid, alg: 'RS512' } }
      let(:jwks) { { keys: [rsa_jwk.export] } }

      it 'does not consider the key a candidate' do
        expect { finder.key_for(kid, algorithm: 'RS256') }.to raise_error(JWT::SignatureError, "Could not find public key for kid #{kid}")
      end

      it 'considers the key a candidate for the declared algorithm' do
        expect(JWT::JWK.new(finder.key_for(kid, algorithm: 'RS512')).members).to eq(rsa_jwk.members)
      end
    end

    context 'when metadata is missing' do
      let(:rsa_params) { { kid: kid } }
      let(:jwks) { { keys: [rsa_jwk.export] } }

      it 'treats the key as a candidate' do
        expect(JWT::JWK.new(finder.key_for(kid, algorithm: 'RS256')).members).to eq(rsa_jwk.members)
      end
    end

    context 'when multiple keys remain after filtering' do
      let(:jwks) { { keys: [rsa_jwk.export, JWT::JWK.new(other_rsa_key, kid: kid).export] } }

      it 'raises an ambiguity error instead of relying on the key order' do
        expect { finder.key_for(kid, algorithm: 'RS256') }.to raise_error(JWT::SignatureError, /Multiple keys match kid/)
      end
    end

    context 'when use and key_ops contradict each other' do
      let(:rsa_params) { { kid: kid, use: 'sig', key_ops: %w[encrypt] } }
      let(:jwks) { { keys: [rsa_jwk.export] } }

      it 'raises a JWKError' do
        expect { finder.key_for(kid, algorithm: 'RS256') }.to raise_error(JWT::JWKError, /contradictory/)
      end
    end

    context 'when an encryption use is combined with a verify key_op' do
      let(:rsa_params) { { kid: kid, use: 'enc', key_ops: %w[verify] } }
      let(:jwks) { { keys: [rsa_jwk.export] } }

      it 'raises a JWKError' do
        expect { finder.key_for(kid, algorithm: 'RS256') }.to raise_error(JWT::JWKError, /contradictory/)
      end
    end

    context 'when the alg member contradicts the kty' do
      let(:rsa_params) { { kid: kid, alg: 'ES256' } }
      let(:jwks) { { keys: [rsa_jwk.export] } }

      it 'raises a JWKError' do
        expect { finder.key_for(kid, algorithm: 'ES256') }.to raise_error(JWT::JWKError, /incompatible/)
      end
    end

    context 'when the policy is given as an instance' do
      let(:verification_policy_option) { JWT::JWK::VerificationPolicy.new }

      it 'applies the policy' do
        expect(JWT::JWK.new(finder.key_for(kid, algorithm: 'ES256')).members).to eq(ec_jwk.members)
      end
    end
  end

  describe 'JWT.decode integration' do
    let(:token_payload) { { 'data' => 'something' } }
    let(:rsa_token) { JWT.encode(token_payload, rsa_key, 'RS256', kid: kid) }
    let(:ec_token) { JWT.encode(token_payload, ec_key, 'ES256', kid: kid) }

    context 'when the verification policy is enabled' do
      it 'decodes a token signed with either key sharing the kid' do
        payload, = JWT.decode(rsa_token, nil, true, algorithms: %w[RS256 ES256], jwks: jwks, verification_policy: true)
        expect(payload).to eq(token_payload)

        payload, = JWT.decode(ec_token, nil, true, algorithms: %w[RS256 ES256], jwks: jwks, verification_policy: true)
        expect(payload).to eq(token_payload)
      end
    end

    context 'when the verification policy is not enabled' do
      it 'keeps the legacy first-match behaviour' do
        payload, = JWT.decode(rsa_token, nil, true, algorithms: %w[RS256 ES256], jwks: jwks)
        expect(payload).to eq(token_payload)

        expect do
          JWT.decode(ec_token, nil, true, algorithms: %w[RS256 ES256], jwks: jwks)
        end.to raise_error(JWT::VerificationKeyError)
      end
    end
  end
end
