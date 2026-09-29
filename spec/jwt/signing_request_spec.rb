# frozen_string_literal: true

RSpec.describe JWT::SigningRequest do
  let(:payload) { { 'pay' => 'load' } }
  let(:header) { {} }

  # Simulates an external signer holding the key outside the application.
  # Only local OpenSSL is used, no cloud service clients involved.
  def external_sign(request, key)
    case request.algorithm
    when 'HS256'
      OpenSSL::HMAC.digest(request.digest, key, request.signing_input)
    when 'RS256'
      key.sign(OpenSSL::Digest.new(request.digest), request.signing_input)
    when 'PS256'
      key.sign_pss(request.digest, request.signing_input, salt_length: :digest, mgf1_hash: request.digest)
    when /\AES/
      der_to_raw(key.dsa_sign_asn1(OpenSSL::Digest.new(request.digest).digest(request.signing_input)), request.signature_length / 2)
    end
  end

  def der_to_raw(der, byte_size)
    OpenSSL::ASN1.decode(der).value.map { |value| value.value.to_s(2).rjust(byte_size, "\x00") }.join
  end

  describe '#initialize' do
    it 'exposes the signing input and the algorithm requirements' do
      request = described_class.new(payload: payload, header: header, algorithm: 'ES256')

      expected_input = JWT::Token.new(payload: payload, header: { 'alg' => 'ES256' }).signing_input
      expect(request.signing_input).to eq(expected_input)
      expect(request.algorithm).to eq('ES256')
      expect(request.digest).to eq('SHA256')
      expect(request.signature_format).to eq(:ecdsa_raw)
      expect(request.signature_length).to eq(64)
    end

    it 'includes the alg header in the signing input' do
      request = described_class.new(payload: payload, header: header, algorithm: 'HS256')

      encoded_header = request.signing_input.split('.').first
      expect(JWT::JSON.parse(JWT::Base64.url_decode(encoded_header))).to eq('alg' => 'HS256')
    end

    it 'freezes the given header and payload' do
      nested = { 'a' => 1 }
      payload = { 'pay' => 'load', 'nested' => nested, 'list' => [1, 2] }
      header = { 'kid' => '1' }

      described_class.new(payload: payload, header: header, algorithm: 'HS256')

      expect { payload['pay'] = 'changed' }.to raise_error(FrozenError)
      expect { nested['a'] = 2 }.to raise_error(FrozenError)
      expect { payload['list'] << 3 }.to raise_error(FrozenError)
      expect { header['kid'] = '2' }.to raise_error(FrozenError)
    end

    it 'freezes the signing input' do
      payload = { 'pay' => 'load' }
      request = described_class.new(payload: payload, header: header, algorithm: 'HS256')

      expect(request.signing_input).to be_frozen
    end

    it 'raises when the algorithm is not supported' do
      expect { described_class.new(payload: payload, algorithm: 'FOOBAR') }.to raise_error(JWT::EncodeError, 'Unsupported signing algorithm')
    end

    it 'raises for the none algorithm' do
      expect { described_class.new(payload: payload, algorithm: 'none') }.to raise_error(JWT::EncodeError, "The 'none' algorithm cannot be signed externally")
    end

    it 'verifies numeric claims at preparation time' do
      expect { described_class.new(payload: { 'exp' => 'not-a-number' }, algorithm: 'HS256') }.to raise_error(JWT::InvalidPayload)
    end
  end

  describe '#complete' do
    context 'with HS256' do
      let(:key) { 'secret' }

      it 'round-trips through the external signer' do
        request = described_class.new(payload: payload, header: header, algorithm: 'HS256')
        token = request.complete(external_sign(request, key))

        expect(JWT::EncodedToken.new(token.jwt).valid_signature?(algorithm: 'HS256', key: key)).to be(true)
      end

      it 'verifies the signature when a key is given' do
        request = described_class.new(payload: payload, header: header, algorithm: 'HS256')
        token = request.complete(external_sign(request, key), key: key)

        expect(token.jwt).to eq(JWT.encode(payload, key, 'HS256'))
      end
    end

    context 'with RS256' do
      let(:private_key) { test_pkey('rsa-2048-private.pem') }
      let(:public_key) { test_pkey('rsa-2048-public.pem') }

      it 'round-trips through the external signer' do
        request = described_class.new(payload: payload, header: header, algorithm: 'RS256')
        token = request.complete(external_sign(request, private_key), key: public_key)

        expect(JWT.decode(token.jwt, public_key, true, algorithm: 'RS256').first).to eq(payload)
      end
    end

    context 'with PS256' do
      let(:private_key) { test_pkey('rsa-2048-private.pem') }
      let(:public_key) { test_pkey('rsa-2048-public.pem') }

      it 'round-trips through the external signer' do
        request = described_class.new(payload: payload, header: header, algorithm: 'PS256')
        token = request.complete(external_sign(request, private_key), key: public_key)

        expect(JWT.decode(token.jwt, public_key, true, algorithm: 'PS256').first).to eq(payload)
      end
    end

    context 'with ES256' do
      let(:private_key) { test_pkey('ec256-private.pem') }
      let(:public_key) { test_pkey('ec256-public.pem') }

      it 'round-trips a raw R || S signature through the external signer' do
        request = described_class.new(payload: payload, header: header, algorithm: 'ES256')
        signature = external_sign(request, private_key)

        expect(signature.bytesize).to eq(64)

        token = request.complete(signature, key: public_key)
        expect(JWT.decode(token.jwt, public_key, true, algorithm: 'ES256').first).to eq(payload)
      end

      it 'rejects the ASN.1/DER representation produced by OpenSSL' do
        request = described_class.new(payload: payload, header: header, algorithm: 'ES256')
        der_signature = private_key.dsa_sign_asn1(OpenSSL::Digest::SHA256.digest(request.signing_input))

        expect { request.complete(der_signature) }.to raise_error(JWT::EncodeError, /Malformed ES256 signature: expected 64 bytes/)
      end
    end

    context 'when the request is already completed' do
      it 'raises' do
        request = described_class.new(payload: payload, header: header, algorithm: 'HS256')
        request.complete(external_sign(request, 'secret'))

        expect { request.complete(external_sign(request, 'secret')) }.to raise_error(JWT::EncodeError, 'Signing request already completed')
      end
    end

    context 'when the signature has the wrong format' do
      it 'rejects non-String signatures' do
        request = described_class.new(payload: payload, header: header, algorithm: 'HS256')

        expect { request.complete(nil) }.to raise_error(JWT::EncodeError, 'Signature must be a String')
      end

      it 'rejects empty signatures' do
        request = described_class.new(payload: payload, header: header, algorithm: 'HS256')

        expect { request.complete('') }.to raise_error(JWT::EncodeError, 'Signature must not be empty')
      end

      it 'rejects truncated ECDSA signatures' do
        request = described_class.new(payload: payload, header: header, algorithm: 'ES256')

        expect { request.complete('x' * 63) }.to raise_error(JWT::EncodeError, /Malformed ES256 signature/)
      end

      it 'does not consume the request' do
        request = described_class.new(payload: payload, header: header, algorithm: 'HS256')

        expect { request.complete('') }.to raise_error(JWT::EncodeError)
        expect(request.complete(external_sign(request, 'secret')).jwt).to eq(JWT.encode(payload, 'secret', 'HS256'))
      end
    end

    context 'when the signature belongs to another request' do
      let(:private_key) { test_pkey('ec256-private.pem') }
      let(:public_key) { test_pkey('ec256-public.pem') }

      let(:request_a) { described_class.new(payload: { 'pay' => 'a' }, algorithm: 'ES256') }
      let(:request_b) { described_class.new(payload: { 'pay' => 'b' }, algorithm: 'ES256') }

      it 'rejects the cross-backfilled signature when a key is given' do
        signature_for_a = external_sign(request_a, private_key)

        expect { request_b.complete(signature_for_a, key: public_key) }.to raise_error(JWT::VerificationError, 'Signature does not match the prepared signing input')
      end

      it 'produces a token that fails verification when no key is given' do
        signature_for_a = external_sign(request_a, private_key)
        token = request_b.complete(signature_for_a)

        expect { JWT.decode(token.jwt, public_key, true, algorithm: 'ES256') }.to raise_error(JWT::VerificationError)
      end

      it 'rejects a signature made for tampered data' do
        request = described_class.new(payload: payload, header: header, algorithm: 'ES256')
        tampered_signature = der_to_raw(private_key.dsa_sign_asn1(OpenSSL::Digest::SHA256.digest(request.signing_input + 'tampered')), 32)

        expect { request.complete(tampered_signature, key: public_key) }.to raise_error(JWT::VerificationError)
      end
    end
  end

  describe 'JWT.prepare_signature' do
    it 'returns a signing request' do
      request = JWT.prepare_signature(payload, algorithm: 'HS256', header: { 'kid' => '1' })

      expect(request).to be_a(described_class)
      expect(request.algorithm).to eq('HS256')
    end
  end
end
