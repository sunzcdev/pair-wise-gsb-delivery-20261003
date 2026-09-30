# frozen_string_literal: true

RSpec.describe 'strict token parsing' do
  let(:valid_token) { JWT.encode({ 'pay' => 'load' }, 'secret', 'HS256') }

  def encode64(data)
    Base64.urlsafe_encode64(data, padding: false)
  end

  def token_with(header: nil, payload: nil, signature: 'sig')
    header_segment = header || encode64('{"alg":"HS256"}')
    payload_segment = payload || encode64('{"pay":"load"}')

    [header_segment, payload_segment, signature].join('.')
  end

  def expect_strict_rejection(input, error_class, message)
    keyfinder_called = false

    expect do
      JWT::EncodedToken.new(input, strict: true)
    end.to raise_error do |error|
      expect(error).to be_instance_of(error_class)
      expect(error.message).to eq(message)
      expect(error.message).not_to include(input)
    end

    expect do
      JWT.decode(input, nil, true, algorithms: ['HS256'], strict: true) do
        keyfinder_called = true
        'secret'
      end
    end.to raise_error do |error|
      expect(error).to be_instance_of(error_class)
      expect(error.message).to eq(message)
      expect(error.message).not_to include(input)
    end

    expect(keyfinder_called).to be(false)
  end

  it 'decodes a canonical token without changing the signing input' do
    encoded_token = JWT::EncodedToken.new(valid_token, strict: true)

    original_header, original_payload = valid_token.split('.')
    expect(encoded_token.signing_input).to eq("#{original_header}.#{original_payload}")

    expect(
      JWT.decode(
        valid_token,
        nil,
        true,
        { algorithms: ['HS256'], strict: true }
      ) { 'secret' }
    ).to eq([{ 'pay' => 'load' }, { 'alg' => 'HS256' }])
  end

  it 'rejects extra segments before both parsing and key lookup' do
    expect_strict_rejection(
      "#{valid_token}.extra",
      JWT::MalformedTokenError,
      'Not enough or too many segments'
    )
  end

  it 'rejects non-canonical Base64url segments' do
    expect_strict_rejection(
      token_with(header: "#{encode64('{"alg":"HS256"}')}="),
      JWT::Base64DecodeError,
      'Invalid base64 encoding'
    )

    expect_strict_rejection(
      token_with(header: "#{encode64('{"alg":"HS256"}')}\n"),
      JWT::Base64DecodeError,
      'Invalid base64 encoding'
    )

    expect_strict_rejection(
      token_with(signature: "#{encode64('sig')}="),
      JWT::Base64DecodeError,
      'Invalid base64 encoding'
    )
  end

  it 'rejects duplicate object keys in the header and payload' do
    expect_strict_rejection(
      token_with(header: encode64('{"alg":"HS256","alg":"none"}')),
      JWT::MalformedTokenError,
      'Duplicate key in JSON segment'
    )

    expect_strict_rejection(
      token_with(payload: encode64('{"a":1,"a":2}')),
      JWT::MalformedTokenError,
      'Duplicate key in JSON segment'
    )

    expect_strict_rejection(
      token_with(payload: encode64('{"a":{"b":1,"b":2}}')),
      JWT::MalformedTokenError,
      'Duplicate key in JSON segment'
    )
  end

  it 'rejects JSON nested deeper than the configured limit' do
    deeply_nested_json = '[' * 101 + '1' + ']' * 101

    expect_strict_rejection(
      token_with(payload: encode64(deeply_nested_json)),
      JWT::MalformedTokenError,
      'JSON nesting is too deep'
    )
  end

  it 'rejects decoded segments larger than the configured limit' do
    stub_const('JWT::StrictTokenParser::MAX_DECODED_SEGMENT_BYTESIZE', 10)

    expect_strict_rejection(
      token_with(payload: encode64('{"pay":"payload"}')),
      JWT::MalformedTokenError,
      'Decoded segment is too large'
    )
  end

  it 'does not include the complete token in rejection messages' do
    input = token_with(payload: encode64('{"a":1,"a":2}'))

    expect do
      JWT::EncodedToken.new(input, strict: true)
    end.to raise_error(JWT::MalformedTokenError) do |error|
      expect(error.message).not_to include(input)
    end
  end

  it 'rejects before normalizing a non-canonical segment used for signature verification' do
    canonical_header = encode64('{"alg":"HS256"}')
    padded_header = "#{canonical_header}="
    payload = encode64('{"pay":"load"}')
    normalized_signature = encode64(
      OpenSSL::HMAC.digest('SHA256', 'secret', "#{canonical_header}.#{payload}")
    )
    input = [padded_header, payload, normalized_signature].join('.')

    expect_strict_rejection(
      input,
      JWT::Base64DecodeError,
      'Invalid base64 encoding'
    )
  end

  it 'keeps the legacy parsing mode permissive' do
    input = 'e30=.eyJwYXkiOiJsb2FkIn0.'

    expect { JWT::EncodedToken.new(input, strict: true) }
      .to raise_error(JWT::Base64DecodeError, 'Invalid base64 encoding')

    expect(JWT.decode(input, nil, false).first).to eq({ 'pay' => 'load' })
    expect(JWT::EncodedToken.new(input).header).to eq({})

    duplicate_key_input = token_with(payload: encode64('{"a":1,"a":2}'))
    expect(JWT.decode(duplicate_key_input, nil, false).first).to eq({ 'a' => 2 })
  end

  it 'keeps the existing critical header validation for unencoded payloads' do
    header = encode64('{"alg":"none","b64":false,"crit":["b64"]}')
    payload = '{"pay":"load"}'
    input = [header, payload, ''].join('.')

    expect(JWT.decode(input, nil, true, algorithms: ['none'], strict: true))
      .to eq([{ 'pay' => 'load' }, { 'alg' => 'none', 'b64' => false, 'crit' => ['b64'] }])
  end
end
