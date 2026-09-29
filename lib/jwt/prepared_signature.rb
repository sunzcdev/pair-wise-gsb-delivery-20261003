# frozen_string_literal: true

module JWT
  # Represents a token prepared for two-step ("external") signing, for cases
  # where the private key lives outside the application (HSM, KMS, a separate
  # signing service).
  #
  # Step one freezes the token and exposes the exact bytes to sign along with
  # the algorithm requirements. Step two takes the externally produced
  # signature back and completes the token:
  #
  #   token = JWT::Token.new(payload: { 'pay' => 'load' })
  #   prepared = token.prepare_signature(algorithm: 'ES256')
  #
  #   prepared.signing_input # the exact bytes to sign
  #   prepared.algorithm     # "ES256"
  #   prepared.digest        # "SHA256", the digest the signer must apply first
  #
  #   # ... the signing input is signed elsewhere ...
  #
  #   jwt = prepared.complete(external_signature, format: :der)
  #
  # The prepared request is bound to the frozen signing input of its own
  # token. A signature produced for another request can technically be
  # passed to {#complete}, but the resulting token will not verify, as the
  # signature does not match this request's signing input.
  class PreparedSignature
    # Returns the JWS algorithm name the external signer must use.
    #
    # @return [String] the algorithm name (e.g. "ES256").
    attr_reader :algorithm

    # Returns the exact bytes the external signer must sign.
    #
    # @return [String] the signing input ("base64url(header).base64url(payload)").
    attr_reader :signing_input

    # @api private
    def initialize(token, algorithm)
      @token = token
      @jwa = JWA.resolve(algorithm)
      raise ::JWT::EncodeError, "Unsupported signing algorithm: #{algorithm.inspect}" if @jwa.equal?(JWA::Unsupported)

      @algorithm = @jwa.alg
      token.header.merge!(@jwa.header) { |_key, old, _new| old }
      @signing_input = token.signing_input
      token.header.freeze
      token.payload.freeze
      @completed = false
    end

    # Returns the name of the digest the external signer must apply to the
    # signing input before signing.
    #
    # @return [String, nil] the OpenSSL digest name (e.g. "SHA256"), or nil
    #   when the algorithm does not prescribe one.
    def digest
      @jwa.digest
    end

    # Completes the token with the externally produced signature.
    #
    # @param signature [String] the signature produced by the external signer
    #   over {#signing_input}.
    # @param format [Symbol] the representation of the given signature.
    #   +:raw+ means the exact JWS signature bytes and is accepted by every
    #   algorithm. ECDSA algorithms additionally accept +:der+ for an ASN.1
    #   DER encoded signature, which is converted to the raw R||S form.
    # @return [String] the completed JWT token.
    # @raise [JWT::EncodeError] if the signature has an invalid format, or if
    #   this request was already completed.
    def complete(signature, format: :raw)
      raise ::JWT::EncodeError, 'Signature already applied' if @completed

      @token.apply_signature(@jwa.normalize_external_signature(signature, format: format))
      @completed = true
      @token.jwt
    end
  end
end
