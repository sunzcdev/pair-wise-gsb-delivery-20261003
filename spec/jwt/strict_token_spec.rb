# frozen_string_literal: true

RSpec.describe 'strict token parsing' do
  let(:secret) { 'secret' }
  let(:header) { { 'alg' => 'HS256' } }
  let(:payload) { { 'data' => 'value' } }
  let(:encoded_token) { build_token(header, payload) }
  attr_accessor :custom_encoded_token

  def encode64(value)
    Base64.urlsafe_encode64(value, padding: false)
  end

  def sign(header_segment, payload_segment)
    encode64(OpenSSL::HMAC.digest('SHA256', secret, "#{header_segment}.#{payload_segment}"))
  end

  def build_token(header_value, payload_value)
    header_segment = encode64(JSON.generate(header_value))
    payload_segment = encode64(JSON.generate(payload_value))
    [header_segment, payload_segment, sign(header_segment, payload_segment)].join('.')
  end

  def sign_strictly
    encoded_token = self.encoded_token

    expect do
      described_class.new(encoded_token, strict: true).verify_signature!(algorithm: 'HS256', key: secret)
    end.not_to raise_error
  end

  def decode_strictly
    expect do
      JWT.decode(encoded_token, secret, true, algorithm: 'HS256', strict: true)
    end.not_to raise_error
  end

  def strict_error
    token_error = nil
    begin
      described_class.new(encoded_token, strict: true)
    rescue JWT::MalformedTokenError => e
      token_error = e
    end

    decode_error = nil
    begin
      JWT.decode(encoded_token, nil, true, algorithm: 'HS256', strict: true)
    rescue JWT::MalformedTokenError => e
      decode_error = e
    end

    [token_error, decode_error]
  end

  shared_examples 'a strict parse rejection' do
    it 'is rejected before key lookup by both entry points' do
      token_error, decode_error = strict_error

      aggregate_failures do
        expect(token_error).to be_a(JWT::MalformedTokenError)
        expect(decode_error).to be_a(JWT::MalformedTokenError)
        expect(decode_error.message).to eq(token_error.message)
        expect(token_error.message).not_to include(encoded_token)
      end
    end
  end

  describe 'JWT::EncodedToken' do
    let(:described_class) { JWT::EncodedToken }

    it 'accepts a canonical token and verifies the original signing input' do
      sign_strictly
    end

    context 'when the token has extra segments' do
      let(:encoded_token) { "#{build_token(header, payload)}.extra" }

      include_examples 'a strict parse rejection'
    end

    context 'when a segment uses base64 padding' do
      let(:header) { { 'alg' => 'HS256', 'ab' => 1 } }

      let(:encoded_token) do
        header_segment = Base64.urlsafe_encode64(JSON.generate(header))
        payload_segment = encode64(JSON.generate(payload))
        [header_segment, payload_segment, sign(header_segment, payload_segment)].join('.')
      end

      include_examples 'a strict parse rejection'
    end

    context 'when a segment uses non-canonical base64url bits' do
      let(:encoded_token) do
        header_segment = encode64(JSON.generate(header))
        payload_segment = encode64(JSON.generate(payload))
        signature = sign(header_segment, payload_segment)
        [header_segment, payload_segment, signature.sub(/.\z/, 'd')].join('.')
      end

      include_examples 'a strict parse rejection'
    end

    context 'when the header duplicates an object key' do
      let(:encoded_token) do
        header_segment = encode64('{"alg":"HS256","alg":"none"}')
        payload_segment = encode64(JSON.generate(payload))
        [header_segment, payload_segment, sign(header_segment, payload_segment)].join('.')
      end

      include_examples 'a strict parse rejection'
    end

    context 'when the payload duplicates a nested object key' do
      let(:encoded_token) do
        header_segment = encode64(JSON.generate(header))
        payload_segment = encode64('{"data":{"a":1,"a":2}}')
        [header_segment, payload_segment, sign(header_segment, payload_segment)].join('.')
      end

      include_examples 'a strict parse rejection'
    end

    context 'when JSON is nested too deeply' do
      let(:encoded_token) do
        nested = 11.times.reduce('1') { |value, _| "{\"a\":#{value}}" }
        header_segment = encode64(JSON.generate(header))
        payload_segment = encode64(nested)
        [header_segment, payload_segment, sign(header_segment, payload_segment)].join('.')
      end

      include_examples 'a strict parse rejection'
    end

    context 'when the decoded payload is too large' do
      let(:payload) { { 'data' => 'a' * (10 * 1024) } }

      include_examples 'a strict parse rejection'
    end

    it 'does not call key_finder when parsing fails' do
      called = false
      token = "#{build_token(header, payload)}.extra"

      expect do
        described_class.new(token, strict: true).valid_signature?(
          algorithm: 'HS256',
          key_finder: ->(_) { called = true; secret }
        )
      end.to raise_error(JWT::MalformedTokenError)

      expect(called).to be(false)
    end

    it 'keeps the received signing input unchanged' do
      token = described_class.new(encoded_token, strict: true)

      expect(token.signing_input).to eq(encoded_token.split('.').first(2).join('.'))
    end

    it 'strictly validates a payload when it is assigned' do
      header = { 'alg' => 'HS256', 'b64' => false, 'crit' => ['b64'] }
      header_segment = encode64(JSON.generate(header))
      raw_payload = JSON.generate(payload)
      placeholder_payload = JSON.generate(placeholder: true)
      signature = encode64(OpenSSL::HMAC.digest('SHA256', secret, "#{header_segment}.#{raw_payload}"))
      token = described_class.new([header_segment, placeholder_payload, signature].join('.'), strict: true)

      token.encoded_payload = raw_payload

      expect(token.unverified_payload).to eq(payload)
      expect(token.verify_signature!(algorithm: 'HS256', key: secret)).to be_nil
    end

    context 'with the old lenient mode' do
      it 'accepts duplicate object keys' do
        header_segment = encode64('{"alg":"HS256","alg":"none"}')
        payload_segment = encode64(JSON.generate(payload))
        token = [header_segment, payload_segment, sign(header_segment, payload_segment)].join('.')

        expect(described_class.new(token).header).to eq('alg' => 'none')
      end
    end
  end

  describe 'JWT.decode' do
    let(:described_class) { JWT::EncodedToken }

    it 'accepts a canonical token' do
      decode_strictly
    end

    context 'when the token has extra segments' do
      let(:encoded_token) { "#{build_token(header, payload)}.extra" }

      include_examples 'a strict parse rejection'
    end

    it 'does not invoke the key callback when parsing fails' do
      called = false

      expect do
        JWT.decode("#{encoded_token}.extra", nil, true, algorithm: 'HS256', strict: true) do
          called = true
          secret
        end
      end.to raise_error(JWT::MalformedTokenError)

      expect(called).to be(false)
    end

    it 'verifies the original signing input' do
      verifier = Class.new do
        include JWT::JWA::SigningAlgorithm

        def initialize(expected_key)
          @alg = 'strict-test'
          @expected_key = expected_key
        end

        attr_reader :received_data, :received_signature

        def verify(data:, signature:, verification_key:)
          @received_data = data
          @received_signature = signature
          verification_key == @expected_key &&
            signature == OpenSSL::HMAC.digest(OpenSSL::Digest::SHA256.new, @expected_key, data)
        end
      end.new(secret)

      header = { 'alg' => verifier.alg }
      header_segment = encode64(JSON.generate(header))
      payload_segment = encode64(JSON.generate(payload))
      signature = encode64(OpenSSL::HMAC.digest('SHA256', secret, "#{header_segment}.#{payload_segment}"))
      self.custom_encoded_token = [header_segment, payload_segment, signature].join('.')

      JWT.decode(custom_encoded_token, secret, true, algorithm: verifier, strict: true)

      expect(verifier.received_data).to eq("#{header_segment}.#{payload_segment}")
      expect(verifier.received_signature).to eq(Base64.urlsafe_decode64(signature))
    end
  end
end
