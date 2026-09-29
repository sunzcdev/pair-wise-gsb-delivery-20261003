# frozen_string_literal: true

RSpec.describe JWT::JWK::KeyFinder do
  let(:kid) { 'shared-kid' }
  let(:rsa_jwk) { JWT::JWK.new(test_pkey('rsa-2048-private.pem'), kid) }
  let(:ec_jwk) { JWT::JWK.new(test_pkey('ec256-private.pem'), kid) }

  let(:rsa_key_params) { rsa_jwk.export }
  let(:ec_key_params) { ec_jwk.export }

  let(:jwks) { { keys: [ec_key_params, rsa_key_params] } }
  let(:finder) { described_class.new(jwks: jwks, **finder_options) }
  let(:finder_options) { {} }

  describe '#key_for' do
    context 'when no policy is given' do
      it 'returns the first key matching the kid, relying on the order of the set' do
        expect(finder.key_for(kid).to_der).to eq(ec_jwk.verify_key.public_to_der)
      end

      context 'and multiple keys with the same kid exist' do
        it 'does not raise an ambiguity error' do
          expect { finder.key_for(kid) }.not_to raise_error
        end
      end
    end

    context 'when a verification policy is given' do
      context 'and the policy requires a kty' do
        let(:finder_options) { { policy: JWT::JWK::VerificationPolicy.new(kty: 'RSA') } }

        it 'selects the key with the matching kty regardless of the order in the set' do
          expect(finder.key_for(kid).to_der).to eq(rsa_jwk.verify_key.public_to_der)
        end
      end

      context 'and the policy requires an algorithm' do
        let(:finder_options) { { policy: JWT::JWK::VerificationPolicy.new(algorithm: 'ES256') } }
        let(:rsa_key_params) { rsa_jwk.export.merge(alg: 'RS256') }
        let(:ec_key_params) { ec_jwk.export.merge(alg: 'ES256') }

        it 'selects the key declaring the algorithm' do
          expect(finder.key_for(kid).to_der).to eq(ec_jwk.verify_key.public_to_der)
        end

        context 'and a key does not declare an alg' do
          let(:ec_key_params) { ec_jwk.export }

          it 'treats the missing alg as unrestricted' do
            expect(finder.key_for(kid).to_der).to eq(ec_jwk.verify_key.public_to_der)
          end
        end
      end

      context 'and keys declare different use values' do
        let(:finder_options) { { policy: JWT::JWK::VerificationPolicy.new } }
        let(:rsa_key_params) { rsa_jwk.export.merge(use: 'enc') }
        let(:ec_key_params) { ec_jwk.export.merge(use: 'sig') }

        it 'rejects keys not declared for signature use' do
          expect(finder.key_for(kid).to_der).to eq(ec_jwk.verify_key.public_to_der)
        end

        context 'and a key does not declare a use' do
          let(:ec_key_params) { ec_jwk.export }

          it 'treats the missing use as unrestricted' do
            expect(finder.key_for(kid).to_der).to eq(ec_jwk.verify_key.public_to_der)
          end
        end
      end

      context 'and keys declare different key_ops values' do
        let(:finder_options) { { policy: JWT::JWK::VerificationPolicy.new } }
        let(:rsa_key_params) { rsa_jwk.export.merge(key_ops: %w[encrypt decrypt]) }
        let(:ec_key_params) { ec_jwk.export.merge(key_ops: %w[verify]) }

        it 'rejects keys without the verify key op' do
          expect(finder.key_for(kid).to_der).to eq(ec_jwk.verify_key.public_to_der)
        end

        context 'and a key does not declare key_ops' do
          let(:ec_key_params) { ec_jwk.export }

          it 'treats the missing key_ops as unrestricted' do
            expect(finder.key_for(kid).to_der).to eq(ec_jwk.verify_key.public_to_der)
          end
        end
      end

      context 'and a key declares contradictory use and key_ops' do
        let(:finder_options) { { policy: JWT::JWK::VerificationPolicy.new } }
        let(:rsa_key_params) { rsa_jwk.export.merge(use: 'sig', key_ops: %w[encrypt]) }
        let(:ec_key_params) { ec_jwk.export.merge(use: 'enc', key_ops: %w[verify]) }

        it 'rejects both keys instead of ignoring one of the declarations' do
          expect { finder.key_for(kid) }.to raise_error(JWT::SignatureError, /Could not find public key for kid/)
        end
      end

      context 'and multiple keys satisfy the policy' do
        let(:finder_options) { { policy: JWT::JWK::VerificationPolicy.new } }

        it 'raises an ambiguity error instead of relying on the order of the set' do
          expect { finder.key_for(kid) }.to raise_error(JWT::SignatureError, /Multiple keys matching kid/)
        end
      end
    end
  end
end
