# frozen_string_literal: true

RSpec.describe JWT::PreparedSignature do
  let(:payload) { { 'pay' => 'load' } }
  let(:header) { {} }
  let(:token) { JWT::Token.new(payload: payload, header: header) }

  # Local OpenSSL standing in for an external signing party holding the key.
  let(:external_signer) do
    ->(data) { OpenSSL::HMAC.digest(OpenSSL::Digest::SHA256.new, hmac_key, data) }
  end
  let(:hmac_key) { 'secret' }

  subject(:prepared) { token.prepare_signature(algorithm: 'HS256') }

  describe '#prepare_signature' do
    it 'exposes the signing input and algorithm requirements' do
      expect(prepared.signing_input).to eq(token.signing_input)
      expect(prepared.algorithm).to eq('HS256')
      expect(prepared.digest).to eq('SHA256')
    end

    it 'merges the alg header into the token' do
      prepared
      expect(token.header).to eq('alg' => 'HS256')
    end

    it 'freezes the header and payload' do
      prepared
      expect(token.header).to be_frozen
      expect(token.payload).to be_frozen
      expect { payload['pay'] = 'changed' }.to raise_error(FrozenError)
      expect { header['custom'] = 'value' }.not_to(change { prepared.signing_input })
    end

    context 'when token is already signed' do
      before { token.sign!(algorithm: 'HS256', key: 'secret') }

      it 'raises' do
        expect { token.prepare_signature(algorithm: 'HS256') }.to raise_error(JWT::EncodeError, 'Token already signed')
      end
    end

    context 'when token is already prepared' do
      before { prepared }

      it 'raises' do
        expect { token.prepare_signature(algorithm: 'HS256') }.to raise_error(JWT::EncodeError, 'Token is already prepared for external signing')
      end

      it 'no longer allows signing with a key' do
        expect { token.sign!(algorithm: 'HS256', key: 'secret') }.to raise_error(JWT::EncodeError, 'Token is prepared for external signing')
      end
    end

    context 'when algorithm is not supported' do
      it 'raises' do
        expect { token.prepare_signature(algorithm: 'NOPE') }.to raise_error(JWT::EncodeError, 'Unsupported signing algorithm: "NOPE"')
      end
    end
  end

  describe '#complete' do
    context 'with HS256' do
      it 'completes a verifiable token' do
        jwt = prepared.complete(external_signer.call(prepared.signing_input))

        expect(JWT::EncodedToken.new(jwt).valid_signature?(algorithm: 'HS256', key: hmac_key)).to be(true)
        expect(JWT.decode(jwt, hmac_key, true, algorithm: 'HS256')).to eq([{ 'pay' => 'load' }, { 'alg' => 'HS256' }])
      end

      it 'returns the same string as Token#jwt' do
        jwt = prepared.complete(external_signer.call(prepared.signing_input))

        expect(jwt).to eq(token.jwt)
      end
    end

    context 'with RS256' do
      let(:rsa_key) { test_pkey('rsa-2048-private.pem') }
      let(:prepared) { token.prepare_signature(algorithm: 'RS256') }

      it 'exposes the digest requirement' do
        expect(prepared.digest).to eq('SHA256')
      end

      it 'completes a verifiable token' do
        signature = rsa_key.sign(OpenSSL::Digest.new(prepared.digest), prepared.signing_input)
        jwt = prepared.complete(signature)

        expect(JWT::EncodedToken.new(jwt).valid_signature?(algorithm: 'RS256', key: rsa_key.public_key)).to be(true)
      end
    end

    context 'with ES256' do
      let(:ec_key) { test_pkey('ec256-private.pem') }
      let(:prepared) { token.prepare_signature(algorithm: 'ES256') }
      let(:der_signature) { ec_key.dsa_sign_asn1(OpenSSL::Digest.new(prepared.digest).digest(prepared.signing_input)) }

      it 'exposes the digest requirement' do
        expect(prepared.digest).to eq('SHA256')
      end

      it 'accepts a DER encoded signature' do
        jwt = prepared.complete(der_signature, format: :der)

        expect(JWT::EncodedToken.new(jwt).valid_signature?(algorithm: 'ES256', key: ec_key.public_key)).to be(true)
      end

      it 'accepts a raw R||S signature' do
        r, s = OpenSSL::ASN1.decode(der_signature).value.map { |v| v.value.to_s(2).rjust(32, "\x00") }
        jwt = prepared.complete(r + s, format: :raw)

        expect(JWT::EncodedToken.new(jwt).valid_signature?(algorithm: 'ES256', key: ec_key.public_key)).to be(true)
      end

      it 'defaults to the raw format' do
        expect { prepared.complete(der_signature) }.to raise_error(JWT::EncodeError, /Invalid ECDSA signature length/)
      end
    end

    context 'when a signature from another prepared request is used' do
      let(:other_token) { JWT::Token.new(payload: { 'pay' => 'other load' }) }
      let(:other_prepared) { other_token.prepare_signature(algorithm: 'HS256') }

      it 'produces a token that does not verify' do
        jwt = prepared.complete(external_signer.call(other_prepared.signing_input))

        encoded = JWT::EncodedToken.new(jwt)
        expect(encoded.valid_signature?(algorithm: 'HS256', key: hmac_key)).to be(false)
        expect { encoded.verify_signature!(algorithm: 'HS256', key: hmac_key) }.to raise_error(JWT::VerificationError)
      end
    end

    context 'when completed twice' do
      it 'raises' do
        prepared.complete(external_signer.call(prepared.signing_input))

        expect { prepared.complete(external_signer.call(prepared.signing_input)) }.to raise_error(JWT::EncodeError, 'Signature already applied')
      end
    end

    context 'when signature format is invalid' do
      let(:prepared) { token.prepare_signature(algorithm: 'ES256') }

      it 'rejects a raw ECDSA signature of wrong length' do
        expect { prepared.complete('too-short', format: :raw) }.to raise_error(JWT::EncodeError, 'Invalid ECDSA signature length for ES256: expected 64 bytes, got 9')
      end

      it 'rejects a malformed DER signature' do
        expect { prepared.complete('not-der', format: :der) }.to raise_error(JWT::EncodeError, /Invalid DER-encoded ECDSA signature/)
      end

      it 'rejects a DER structure without two integers' do
        der = OpenSSL::ASN1::Sequence.new([OpenSSL::ASN1::Integer.new(1)]).to_der

        expect { prepared.complete(der, format: :der) }.to raise_error(JWT::EncodeError, /expected a SEQUENCE of two INTEGERs/)
      end

      it 'rejects an unknown format' do
        expect { prepared.complete('x' * 64, format: :pem) }.to raise_error(JWT::EncodeError, 'Unsupported signature format :pem for ES256. Expected :raw or :der')
      end

      it 'rejects a non-string signature' do
        expect { prepared.complete(nil) }.to raise_error(JWT::EncodeError, 'Signature must be a String')
      end
    end

    context 'when a non-ECDSA algorithm gets a non-raw format' do
      it 'raises' do
        expect { prepared.complete('signature', format: :der) }.to raise_error(JWT::EncodeError, 'Unsupported signature format :der for HS256. Only :raw is supported')
      end
    end
  end
end
