# frozen_string_literal: true

module JWT
  # Generates and verifies detached signatures over external raw payload bytes
  # (see RFC 7515 Appendix F and RFC 7797).
  #
  # The payload is treated as opaque bytes: it is never JSON processed and never
  # transcoded, so payloads containing dots, newlines or non-UTF-8 bytes are
  # signed and verified exactly as given.
  #
  # Generating a detached signature:
  #
  #   jwt = JWT::DetachedPayload.encode(raw_bytes, 'secret', 'HS256')
  #
  # Verifying a detached signature:
  #
  #   payload, header = JWT::DetachedPayload.decode(jwt, raw_bytes, 'secret', true, algorithm: 'HS256')
  #
  # Explicitly declared unencoded payload mode (RFC 7797, "b64": false protected
  # header accompanied by the "crit" declaration):
  #
  #   jwt = JWT::DetachedPayload.encode(raw_bytes, 'secret', 'HS256', {}, unencoded: true)
  #   payload, header = JWT::DetachedPayload.decode(jwt, raw_bytes, 'secret', true, algorithm: 'HS256', unencoded: true)
  module DetachedPayload
    # @private
    class RawToken < ::JWT::Token
      def initialize(payload:, header:, unencoded:)
        super(payload: payload, header: header)
        @unencoded = unencoded
        detach_payload!
      end

      def encoded_payload
        @encoded_payload ||= @unencoded ? payload : ::JWT::Base64.url_encode(payload)
      end
    end

    private_constant(:RawToken)

    module_function

    # Generates a detached signature for the given raw payload bytes.
    #
    # @param payload [String] the raw payload bytes to sign. Treated as opaque
    #   bytes, never JSON processed or transcoded.
    # @param key [String] the key used to sign the payload.
    # @param algorithm [String] the algorithm used to sign the payload.
    # @param header_fields [Hash] additional headers to include in the token.
    # @param unencoded [Boolean] when true, the payload is signed as-is and the
    #   token explicitly declares the unencoded payload mode via the "b64": false
    #   protected header and the corresponding "crit" entry.
    # @return [String] the token with a detached payload ("header..signature").
    def encode(payload, key, algorithm = 'HS256', header_fields = {}, unencoded: false)
      raise ArgumentError, 'Payload must be a String' unless payload.is_a?(String)

      headers = header_fields.transform_keys(&:to_s)
      if unencoded
        headers['b64'] = false
        headers['crit'] = (Array(headers['crit']) | ['b64'])
      end

      token = RawToken.new(payload: payload, header: headers, unencoded: unencoded)
      token.sign!(algorithm: algorithm, key: key)
      token.jwt
    end

    # Verifies a detached signature against the given raw payload bytes.
    #
    # @param jwt [String] the token with a detached payload ("header..signature").
    # @param payload [String] the raw payload bytes the signature is expected to cover.
    # @param key [String] the key used to verify the signature.
    # @param verify [Boolean] whether to verify the signature.
    # @param options [Hash] additional options for verification.
    # @option options [String, Array<String>] :algorithm the allowed algorithm(s).
    # @option options [Boolean] :unencoded must be true when the token declares the
    #   unencoded payload mode ("b64": false), and must not be set otherwise.
    # @return [Array(String, Hash)] the verified raw payload and the token header.
    # @raise [JWT::MalformedTokenError] if the token carries an embedded payload, has
    #   an invalid structure or its unencoded payload declaration does not match the
    #   given options.
    # @raise [JWT::InvalidCritError] if the "crit" header does not cover the "b64" declaration.
    # @raise [JWT::VerificationError] if the signature does not match the payload.
    def decode(jwt, payload, key = nil, verify = true, options = {}, &keyfinder)
      raise JWT::MalformedTokenError, 'Nil JSON web token' unless jwt

      token = EncodedToken.new(jwt)
      validate_segment_count!(token, verify)
      validate_detached!(token)

      unencoded = unencoded_payload?(token.header, options)
      token.encoded_payload = unencoded ? payload : ::JWT::Base64.url_encode(payload)

      verify_signature!(token, payload, key, options, &keyfinder) if verify

      [payload, token.header]
    end

    def validate_segment_count!(token, verify)
      segment_count = token.jwt.count('.') + 1
      return if segment_count == 3
      return if !verify && segment_count == 2

      raise JWT::MalformedTokenError, 'Not enough or too many segments'
    end
    private_class_method :validate_segment_count!

    def validate_detached!(token)
      return if token.encoded_payload.nil? || token.encoded_payload.empty?

      raise JWT::MalformedTokenError, 'Token already contains a payload'
    end
    private_class_method :validate_detached!

    def unencoded_payload?(header, options)
      raise JWT::MalformedTokenError, 'Token header not a JSON object' unless header.is_a?(Hash)

      b64 = header['b64']
      raise JWT::MalformedTokenError, 'Invalid b64 header value' unless b64.nil? || b64 == false

      declared = b64 == false
      verify_crit!(header) if declared

      expected = options[:unencoded] || options['unencoded'] ? true : false
      if declared != expected
        message = declared ? 'Token declares an unencoded payload, pass unencoded: true to verify it' :
                             'Token does not declare an unencoded payload (b64: false)'
        raise JWT::MalformedTokenError, message
      end

      declared
    end
    private_class_method :unencoded_payload?

    def verify_crit!(header)
      crit = header['crit']
      return if crit.is_a?(Array) && crit.include?('b64')

      raise JWT::InvalidCritError, 'Crit header missing expected values: b64'
    end
    private_class_method :verify_crit!

    def verify_signature!(token, payload, key, options, &keyfinder)
      header = token.header
      raise JWT::IncorrectAlgorithm, 'Token is missing alg header' unless header['alg']

      algorithms = allowed_algorithms(options)
      raise JWT::IncorrectAlgorithm, 'An algorithm must be specified' if algorithms.empty?

      valid_algorithms = algorithms.select { |alg| alg.valid_alg?(header['alg']) }
      raise JWT::IncorrectAlgorithm, 'Expected a different algorithm' if valid_algorithms.empty?

      return if header['alg'] == 'none'

      key = find_key(key, token, payload, &keyfinder)
      raise JWT::SignatureError, 'No verification key available' unless key

      token.verify_signature!(algorithm: valid_algorithms, key: key)
    end
    private_class_method :verify_signature!

    def allowed_algorithms(options)
      alg_key = Decode::ALGORITHM_KEYS.find { |key| options[key] }
      Array(options[alg_key]).map { |alg| JWA.resolve(alg) }
    end
    private_class_method :allowed_algorithms

    def find_key(key, token, payload, &keyfinder)
      return key unless keyfinder

      keyfinder.arity == 2 ? keyfinder.call(token.header, payload) : keyfinder.call(token.header)
    end
    private_class_method :find_key
  end
end
