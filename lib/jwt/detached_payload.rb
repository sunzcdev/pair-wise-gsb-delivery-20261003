# frozen_string_literal: true

module JWT
  # Signs and verifies raw bytes transferred over a separate channel with a
  # detached signature.
  #
  # The payload never travels inside the compact serialization. It is used in
  # its raw, unencoded form, explicitly declared with the "b64" protected
  # header and the corresponding "crit" entry (RFC 7797), and detached from
  # the serialization (RFC 7515, Appendix F).
  #
  # This entry point is intentionally separate from JWT.decode: the payload is
  # never JSON parsed, generated or transcoded, so dots, newlines and
  # non-UTF-8 bytes are signed and verified byte for byte.
  #
  # @example
  #   jwt = JWT::DetachedPayload.sign(raw_bytes, key: 'secret', algorithm: 'HS256')
  #   JWT::DetachedPayload.verify(jwt, raw_bytes, key: 'secret', algorithm: 'HS256') # => true
  class DetachedPayload
    class << self
      # Signs raw bytes and returns the compact serialization with a detached payload.
      #
      # @param payload [String] the raw bytes to sign.
      # @param key [String, JWT::JWK::KeyBase] the key to sign with.
      # @param algorithm [String, Object] the algorithm to sign with.
      # @param headers [Hash] additional headers for the token.
      # @return [String] the compact serialization without the payload segment.
      # @raise [JWT::EncodeError] if the payload is not a String.
      def sign(payload, key:, algorithm:, headers: {})
        header = headers.transform_keys(&:to_s)
        crit   = Array(header['crit']) | ['b64']
        token  = Token.new(payload: payload, header: header.merge('b64' => false, 'crit' => crit))
        token.detach_payload!
        token.sign!(algorithm: algorithm, key: key)
        token.jwt
      end

      # Verifies a detached signature against raw bytes received separately.
      #
      # @param jwt [String] the compact serialization with a detached payload.
      # @param payload [String] the raw bytes the signature is expected to cover.
      # @param key [String, Array<String>, JWT::JWK::KeyBase] the key(s) to verify with.
      # @param algorithm [String, Array<String>, Object] the algorithm(s) to verify with.
      # @return [Boolean] true when the signature is valid.
      # @raise [JWT::MalformedTokenError] if the token carries an embedded payload or
      #   does not explicitly declare the unencoded payload mode.
      # @raise [JWT::InvalidCritError] if the "crit" header does not include "b64".
      # @raise [JWT::VerificationError] if the signature does not match the payload.
      def verify(jwt, payload, key:, algorithm:)
        token = EncodedToken.new(jwt)

        unless token.encoded_payload.nil? || token.encoded_payload.empty?
          raise JWT::MalformedTokenError, 'Token carries an embedded payload, expected a detached payload'
        end

        raise JWT::MalformedTokenError, 'The "b64" header must be set to false for detached payloads' unless token.header['b64'] == false

        token.verify_claims!(crit: ['b64'])
        token.encoded_payload = payload.to_s.b
        token.verify_signature!(algorithm: algorithm, key: key)
        true
      end

      # Checks whether a detached signature is valid for the given raw bytes.
      #
      # @param jwt [String] the compact serialization with a detached payload.
      # @param payload [String] the raw bytes the signature is expected to cover.
      # @param key [String, Array<String>, JWT::JWK::KeyBase] the key(s) to verify with.
      # @param algorithm [String, Array<String>, Object] the algorithm(s) to verify with.
      # @return [Boolean] true when the signature is valid, false otherwise.
      def valid?(jwt, payload, key:, algorithm:)
        verify(jwt, payload, key: key, algorithm: algorithm)
      rescue JWT::Error
        false
      end
    end
  end
end
