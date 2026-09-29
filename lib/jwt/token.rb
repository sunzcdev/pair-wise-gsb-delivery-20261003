# frozen_string_literal: true

module JWT
  # Represents a JWT token
  #
  # Basic token signed using the HS256 algorithm:
  #
  #   token = JWT::Token.new(payload: {pay: 'load'})
  #   token.sign!(algorithm: 'HS256', key: 'secret')
  #   token.jwt # => eyJhb....
  #
  # Custom headers will be combined with generated headers:
  #   token = JWT::Token.new(payload: {pay: 'load'}, header: {custom: "value"})
  #   token.sign!(algorithm: 'HS256', key: 'secret')
  #   token.header # => {"custom"=>"value", "alg"=>"HS256"}
  #
  class Token
    # Initializes a new Token instance.
    #
    # @param header [Hash] the header of the JWT token.
    # @param payload [Hash] the payload of the JWT token.
    def initialize(payload:, header: {})
      @header  = header&.transform_keys(&:to_s)
      @payload = payload
    end

    # Returns the decoded signature of the JWT token.
    #
    # @return [String] the decoded signature of the JWT token.
    def signature
      @signature ||= ::JWT::Base64.url_decode(encoded_signature || '')
    end

    # Returns the encoded signature of the JWT token.
    #
    # @return [String] the encoded signature of the JWT token.
    def encoded_signature
      @encoded_signature ||= ::JWT::Base64.url_encode(signature)
    end

    # Returns the decoded header of the JWT token.
    #
    # @return [Hash] the header of the JWT token.
    attr_reader :header

    # Returns the encoded header of the JWT token.
    #
    # @return [String] the encoded header of the JWT token.
    def encoded_header
      @encoded_header ||= ::JWT::Base64.url_encode(JWT::JSON.generate(header))
    end

    # Returns the payload of the JWT token.
    #
    # @return [Hash] the payload of the JWT token.
    attr_reader :payload

    # Returns the encoded payload of the JWT token.
    #
    # @return [String] the encoded payload of the JWT token.
    def encoded_payload
      @encoded_payload ||= ::JWT::Base64.url_encode(JWT::JSON.generate(payload))
    end

    # Returns the signing input of the JWT token.
    #
    # @return [String] the signing input of the JWT token.
    def signing_input
      @signing_input ||= [encoded_header, encoded_payload].join('.')
    end

    # Returns the JWT token as a string.
    #
    # @return [String] the JWT token as a string.
    # @raise [JWT::EncodeError] if the token is not signed or other encoding issues
    def jwt
      @jwt ||= (@signature && [encoded_header, @detached_payload ? '' : encoded_payload, encoded_signature].join('.')) || raise(::JWT::EncodeError, 'Token is not signed')
    end

    # Detaches the payload according to https://datatracker.ietf.org/doc/html/rfc7515#appendix-F
    #
    def detach_payload!
      @detached_payload = true
      @jwt = nil

      nil
    end

    # Signs the JWT token.
    #
    # @param key [String, JWT::JWK::KeyBase] the key to use for signing.
    # @param algorithm [String, Object] the algorithm to use for signing.
    # @return [void]
    # @raise [JWT::EncodeError] if the token is already signed or other problems when signing
    def sign!(key:, algorithm:)
      raise ::JWT::EncodeError, 'Token already signed' if @signature
      raise ::JWT::EncodeError, 'Token is prepared for external signing' if @prepared_signature

      JWA.create_signer(algorithm: algorithm, key: key).tap do |signer|
        header.merge!(signer.jwa.header) { |_key, old, _new| old }
        @signature = signer.sign(data: signing_input)
      end

      nil
    end

    # Prepares the token for two-step ("external") signing, for cases where
    # the private key lives outside the application.
    #
    # The token header and payload are frozen and the returned
    # {PreparedSignature} exposes the exact bytes to sign along with the
    # algorithm requirements. The externally produced signature is then
    # passed to {PreparedSignature#complete} to finish the token.
    #
    #   prepared = token.prepare_signature(algorithm: 'ES256')
    #   jwt = prepared.complete(external_signer.sign(prepared.signing_input), format: :der)
    #
    # @param algorithm [String, Object] the algorithm to use for signing.
    # @return [JWT::PreparedSignature] the prepared signing request.
    # @raise [JWT::EncodeError] if the token is already signed or prepared.
    def prepare_signature(algorithm:)
      raise ::JWT::EncodeError, 'Token already signed' if @signature
      raise ::JWT::EncodeError, 'Token is already prepared for external signing' if @prepared_signature

      @prepared_signature = PreparedSignature.new(self, algorithm)
    end

    # Attaches an externally produced signature to the token.
    #
    # @api private
    # @param signature [String] the signature over the token signing input.
    # @return [void]
    # @raise [JWT::EncodeError] if the token is already signed.
    def apply_signature(signature)
      raise ::JWT::EncodeError, 'Token already signed' if @signature

      @signature = signature

      nil
    end

    # Verifies the claims of the token.
    # @param options [Array<Symbol>, Hash] the claims to verify.
    # @raise [JWT::ClaimValidationError] if the claims are invalid.
    def verify_claims!(*options)
      Claims::Verifier.verify!(self, *options)
    end

    # Returns the errors of the claims of the token.
    # @param options [Array<Symbol>, Hash] the claims to verify.
    # @return [Array<Symbol>] the errors of the claims.
    def claim_errors(*options)
      Claims::Verifier.errors(self, *options)
    end

    # Returns whether the claims of the token are valid.
    # @param options [Array<Symbol>, Hash] the claims to verify.
    # @return [Boolean] whether the claims are valid.
    def valid_claims?(*options)
      claim_errors(*options).empty?
    end

    # Returns the JWT token as a string.
    #
    # @return [String] the JWT token as a string.
    alias to_s jwt
  end
end
