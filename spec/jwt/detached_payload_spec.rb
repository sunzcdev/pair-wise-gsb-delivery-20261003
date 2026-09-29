# frozen_string_literal: true

RSpec.describe JWT::DetachedPayload do
  let(:key) { 'secret' }
  # Raw payload bytes containing a dot, a newline and non-UTF-8 bytes
  let(:payload) { "pay.load\nwith \xFF\xFE bytes".b }

  # Test vectors computed independently with OpenSSL (HMAC-SHA256, key "secret")
  let(:encoded_token) { 'eyJhbGciOiJIUzI1NiJ9..HIt3_cMfBEC4IC6wk4zIuuQtDGUwztKc1-zJYH_COVk' }
  let(:unencoded_token) { 'eyJiNjQiOmZhbHNlLCJjcml0IjpbImI2NCJdLCJhbGciOiJIUzI1NiJ9..l9J7DZVuK3-EelTsV-ZcYyvXf4Y8_55FSSTdX7aD3J0' }
  let(:no_crit_token) { 'eyJiNjQiOmZhbHNlLCJhbGciOiJIUzI1NiJ9..lZThURoCRSkF-5a0qBIciL9nFOET2s-4poNOB2WnZG0' }
  let(:embedded_payload_token) { 'eyJhbGciOiJIUzI1NiJ9.cGF5LmxvYWQKd2l0aCD__iBieXRlcw.HIt3_cMfBEC4IC6wk4zIuuQtDGUwztKc1-zJYH_COVk' }

  describe '.encode' do
    it 'returns a detached token with an empty payload segment' do
      token = described_class.encode(payload, key, 'HS256')
      expect(token.split('.')[1]).to eq('')
    end

    it 'signs the base64url encoded raw payload bytes' do
      expect(described_class.encode(payload, key, 'HS256')).to eq(encoded_token)
    end

    it 'does not alter payloads containing dots, newlines or non-UTF-8 bytes' do
      token = described_class.encode(payload, key, 'HS256')
      expect(token).to eq(encoded_token)
    end

    context 'with unencoded payload mode' do
      subject(:token) { described_class.encode(payload, key, 'HS256', {}, unencoded: true) }

      it 'signs the raw payload bytes as-is' do
        expect(token).to eq(unencoded_token)
      end

      it 'declares b64: false and the corresponding crit entry in the protected header' do
        header = JSON.parse(JWT::Base64.url_decode(token.split('.')[0]))
        expect(header).to eq('b64' => false, 'crit' => ['b64'], 'alg' => 'HS256')
      end

      it 'merges a user provided crit header with the b64 entry' do
        token = described_class.encode(payload, key, 'HS256', { crit: ['exp'] }, unencoded: true)
        header = JSON.parse(JWT::Base64.url_decode(token.split('.')[0]))
        expect(header['crit']).to contain_exactly('exp', 'b64')
      end
    end

    context 'when payload is not a String' do
      it 'raises an ArgumentError' do
        expect { described_class.encode({ 'pay' => 'load' }, key, 'HS256') }.to raise_error(ArgumentError, 'Payload must be a String')
      end
    end
  end

  describe '.decode' do
    it 'verifies the signature against the raw payload bytes' do
      payload_out, header = described_class.decode(encoded_token, payload, key, true, algorithm: 'HS256')
      expect(payload_out).to eq(payload)
      expect(header).to eq('alg' => 'HS256')
    end

    it 'verifies an explicitly declared unencoded payload' do
      payload_out, header = described_class.decode(unencoded_token, payload, key, true, algorithm: 'HS256', unencoded: true)
      expect(payload_out).to eq(payload)
      expect(header).to eq('b64' => false, 'crit' => ['b64'], 'alg' => 'HS256')
    end

    context 'when the payload is missing a byte' do
      it 'fails verification' do
        expect { described_class.decode(encoded_token, payload[0..-2], key, true, algorithm: 'HS256') }
          .to raise_error(JWT::VerificationError)
      end

      it 'fails verification in unencoded mode' do
        expect { described_class.decode(unencoded_token, payload[0..-2], key, true, algorithm: 'HS256', unencoded: true) }
          .to raise_error(JWT::VerificationError)
      end
    end

    context 'when the payload has an extra byte' do
      it 'fails verification' do
        expect { described_class.decode(encoded_token, "#{payload}x", key, true, algorithm: 'HS256') }
          .to raise_error(JWT::VerificationError)
      end
    end

    context 'when the token also carries an embedded payload' do
      it 'fails' do
        expect { described_class.decode(embedded_payload_token, payload, key, true, algorithm: 'HS256') }
          .to raise_error(JWT::MalformedTokenError, 'Token already contains a payload')
      end
    end

    context 'when the b64 declaration is not listed in crit' do
      it 'fails' do
        expect { described_class.decode(no_crit_token, payload, key, true, algorithm: 'HS256', unencoded: true) }
          .to raise_error(JWT::InvalidCritError, 'Crit header missing expected values: b64')
      end
    end

    context 'when the token declares an unencoded payload but the option is not given' do
      it 'fails' do
        expect { described_class.decode(unencoded_token, payload, key, true, algorithm: 'HS256') }
          .to raise_error(JWT::MalformedTokenError, 'Token declares an unencoded payload, pass unencoded: true to verify it')
      end
    end

    context 'when the unencoded option is given but the token does not declare it' do
      it 'fails' do
        expect { described_class.decode(encoded_token, payload, key, true, algorithm: 'HS256', unencoded: true) }
          .to raise_error(JWT::MalformedTokenError, 'Token does not declare an unencoded payload (b64: false)')
      end
    end

    context 'when the b64 header value is not false' do
      let(:token) { "#{JWT::Base64.url_encode(JSON.generate('alg' => 'HS256', 'b64' => true))}..signature" }

      it 'fails' do
        expect { described_class.decode(token, payload, key, true, algorithm: 'HS256') }
          .to raise_error(JWT::MalformedTokenError, 'Invalid b64 header value')
      end
    end

    context 'when the key is wrong' do
      it 'fails verification' do
        expect { described_class.decode(encoded_token, payload, 'wrong', true, algorithm: 'HS256') }
          .to raise_error(JWT::VerificationError)
      end
    end

    context 'when the algorithm is not allowed' do
      it 'fails' do
        expect { described_class.decode(encoded_token, payload, key, true, algorithm: 'HS512') }
          .to raise_error(JWT::IncorrectAlgorithm, 'Expected a different algorithm')
      end
    end

    context 'when no algorithm is specified' do
      it 'fails' do
        expect { described_class.decode(encoded_token, payload, key, true) }
          .to raise_error(JWT::IncorrectAlgorithm, 'An algorithm must be specified')
      end
    end

    context 'when verification is disabled' do
      it 'returns the payload and header without verifying' do
        payload_out, header = described_class.decode(encoded_token, payload, nil, false)
        expect(payload_out).to eq(payload)
        expect(header).to eq('alg' => 'HS256')
      end
    end

    context 'when a keyfinder is given' do
      it 'uses the key provided by the keyfinder' do
        payload_out, = described_class.decode(encoded_token, payload, nil, true, algorithm: 'HS256') { |_header| key }
        expect(payload_out).to eq(payload)
      end
    end

    context 'when the algorithm is none' do
      let(:none_token) { 'eyJhbGciOiJub25lIn0..' }

      it 'does not require a key' do
        payload_out, header = described_class.decode(none_token, payload, nil, true, algorithm: 'none')
        expect(payload_out).to eq(payload)
        expect(header).to eq('alg' => 'none')
      end
    end
  end
end
