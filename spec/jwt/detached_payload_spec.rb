# frozen_string_literal: true

RSpec.describe JWT::DetachedPayload do
  let(:key) { 'secret' }
  let(:algorithm) { 'HS256' }
  # Raw bytes containing a dot, newlines and non-UTF-8 bytes.
  let(:payload) { "raw.pay\nload\r\nwith\xFFbinary\x00bytes".b }
  let(:jwt) { 'eyJiNjQiOmZhbHNlLCJjcml0IjpbImI2NCJdLCJhbGciOiJIUzI1NiJ9..2CQTGbqGwoX1m0jVIC9uC6o8Z7O32NKv24pKrQ6ngy0' }

  describe '.sign' do
    subject(:signed_jwt) { described_class.sign(payload, key: key, algorithm: algorithm) }

    it 'matches the expected test vector' do
      expect(signed_jwt).to eq(jwt)
    end

    it 'returns a compact serialization with a detached payload' do
      expect(signed_jwt.split('.')[1]).to eq('')
    end

    it 'declares the unencoded payload mode in the protected header' do
      header = JWT::JSON.parse(JWT::Base64.url_decode(signed_jwt.split('.')[0]))
      expect(header).to eq('b64' => false, 'crit' => ['b64'], 'alg' => 'HS256')
    end

    it 'produces a signature verifiable against the raw bytes' do
      expect(described_class.verify(signed_jwt, payload, key: key, algorithm: algorithm)).to be(true)
    end

    context 'when additional headers are given' do
      subject(:signed_jwt) { described_class.sign(payload, key: key, algorithm: algorithm, headers: { kid: 'key-1', crit: %w[exp] }) }

      it 'merges them with the b64 declaration' do
        header = JWT::JSON.parse(JWT::Base64.url_decode(signed_jwt.split('.')[0]))
        expect(header).to include('kid' => 'key-1', 'b64' => false)
        expect(header['crit']).to contain_exactly('exp', 'b64')
      end
    end

    context 'when payload is not a String' do
      it 'raises an EncodeError' do
        expect { described_class.sign({ 'pay' => 'load' }, key: key, algorithm: algorithm) }.to raise_error(JWT::EncodeError, 'Payload must be a String when the "b64" header is false')
      end
    end
  end

  describe '.verify' do
    it 'returns true for the exact payload' do
      expect(described_class.verify(jwt, payload, key: key, algorithm: algorithm)).to be(true)
    end

    it 'does not modify the given payload' do
      expect { described_class.verify(jwt, payload, key: key, algorithm: algorithm) }.not_to(change { payload })
    end

    context 'when the payload is missing a byte' do
      it 'raises a VerificationError' do
        expect { described_class.verify(jwt, payload[0..-2], key: key, algorithm: algorithm) }.to raise_error(JWT::VerificationError, 'Signature verification failed')
      end
    end

    context 'when the payload has an extra byte' do
      it 'raises a VerificationError' do
        expect { described_class.verify(jwt, "#{payload}x", key: key, algorithm: algorithm) }.to raise_error(JWT::VerificationError, 'Signature verification failed')
      end
    end

    context 'when the key is wrong' do
      it 'raises a VerificationError' do
        expect { described_class.verify(jwt, payload, key: 'wrong', algorithm: algorithm) }.to raise_error(JWT::VerificationError, 'Signature verification failed')
      end
    end

    context 'when the token also carries an embedded payload' do
      let(:embedded_jwt) { JWT::Token.new(payload: { 'pay' => 'load' }).tap { |token| token.sign!(algorithm: 'HS256', key: key) }.jwt }

      it 'raises a MalformedTokenError' do
        expect { described_class.verify(embedded_jwt, payload, key: key, algorithm: algorithm) }.to raise_error(JWT::MalformedTokenError, 'Token carries an embedded payload, expected a detached payload')
      end
    end

    context 'when the b64 header is not declared' do
      let(:jwt) do
        JWT::Token.new(payload: { 'pay' => 'load' }).tap do |token|
          token.detach_payload!
          token.sign!(algorithm: 'HS256', key: key)
        end.jwt
      end

      it 'raises a MalformedTokenError' do
        expect { described_class.verify(jwt, payload, key: key, algorithm: algorithm) }.to raise_error(JWT::MalformedTokenError, 'The "b64" header must be set to false for detached payloads')
      end
    end

    context 'when the crit header does not include b64' do
      let(:encoded_header) { JWT::Base64.url_encode(JWT::JSON.generate('alg' => 'HS256', 'b64' => false)) }
      let(:jwt) do
        signature = JWT::Base64.url_encode(OpenSSL::HMAC.digest('SHA256', key, [encoded_header, payload].join('.')))
        [encoded_header, '', signature].join('.')
      end

      it 'raises an InvalidCritError' do
        expect { described_class.verify(jwt, payload, key: key, algorithm: algorithm) }.to raise_error(JWT::InvalidCritError, 'Crit header missing')
      end
    end
  end

  describe '.valid?' do
    it 'returns true for a valid signature' do
      expect(described_class.valid?(jwt, payload, key: key, algorithm: algorithm)).to be(true)
    end

    it 'returns false when the payload does not match' do
      expect(described_class.valid?(jwt, payload[0..-2], key: key, algorithm: algorithm)).to be(false)
    end

    it 'returns false when the token carries an embedded payload' do
      embedded_jwt = JWT::Token.new(payload: { 'pay' => 'load' }).tap { |token| token.sign!(algorithm: 'HS256', key: key) }.jwt
      expect(described_class.valid?(embedded_jwt, payload, key: key, algorithm: algorithm)).to be(false)
    end
  end
end
