# frozen_string_literal: true

module JWT
  # A prepared two-step signing request for tokens signed outside the
  # application, for example by an HSM, a KMS or another service holding
  # the private key.
  #
  # Preparing a request freezes the header and payload and exposes the
  # exact bytes to sign together with the algorithm requirements. The
  # externally computed signature is then handed back via {#complete} to
  # produce the token:
  #
  #   request = JWT::SigningRequest.new(payload: { 'pay' => 'load' }, algorithm: 'ES256')
  #   signature = external_signer.sign(request.signing_input) # raw R || S for ECDSA
  #   token = request.complete(signature, key: public_key)
  #   token.jwt # => "eyJhb..."
  #
  # The signature is bound to the frozen signing input of this request:
  # handing back a signature made for another request (or for tampered
  # data) is rejected when a verification key is given to {#complete}.
  class SigningRequest
    # Initializes a new signing request and freezes the header and payload.
    #
    # @param payload [Hash] the payload of the JWT token.
    # @param header [Hash] the header of the JWT token.
    # @param algorithm [String, Object] the algorithm the external signer will use.
    # @raise [JWT::EncodeError] if the algorithm is unsupported or not signable.
    def initialize(payload:, header: {}, algorithm:)
      @jwa = JWA.resolve(algorithm)
      raise ::JWT::EncodeError, 'Unsupported signing algorithm' if @jwa.alg.nil?
      raise ::JWT::EncodeError, "The 'none' algorithm cannot be signed externally" if @jwa.valid_alg?('none')

      deep_freeze(header) if header
      deep_freeze(payload)

      @token = Token.new(payload: payload, header: header)
      @token.verify_claims!(:numeric)
      @token.header.merge!(@jwa.header) { |_key, old, _new| old }
      @signing_input = @token.signing_input.freeze
      @completed = false
    end

    # Returns the bytes the external signer must sign.
    #
    # @return [String] the signing input of the token.
    attr_reader :signing_input

    # Returns the name of the algorithm, e.g. "ES256".
    #
    # @return [String] the algorithm name.
    def algorithm
      @jwa.alg
    end

    # Returns the name of the digest the signing input must be hashed with,
    # or nil when the algorithm does not use a digest.
    #
    # @return [String, nil] the digest name, e.g. "SHA256".
    def digest
      @jwa.digest
    end

    # Returns the representation the external signer must hand the signature
    # back in. :raw means the native signature bytes, :ecdsa_raw means the
    # JWS raw R || S fixed-width concatenation (not ASN.1/DER).
    #
    # @return [Symbol] the expected signature format.
    def signature_format
      @jwa.signature_format
    end

    # Returns the expected byte length of the signature, or nil when the
    # algorithm does not prescribe a fixed length.
    #
    # @return [Integer, nil] the expected signature length in bytes.
    def signature_length
      @jwa.signature_length
    end

    # Completes the request with the externally computed signature and
    # returns the signed token.
    #
    # @param signature [String] the signature for the signing input.
    # @param key [Object, nil] optional key to verify the signature against
    #   the prepared signing input before completing.
    # @return [JWT::Token] the signed token.
    # @raise [JWT::EncodeError] if the request is already completed or the
    #   signature does not match the expected format.
    # @raise [JWT::VerificationError] if a key is given and the signature
    #   does not match the prepared signing input.
    def complete(signature, key: nil)
      raise ::JWT::EncodeError, 'Signing request already completed' if @completed

      validate_signature_format!(signature)
      verify_signature!(signature, key) if key

      @completed = true
      @token.attach_signature(signature)
      @token
    end

    private

    def validate_signature_format!(signature)
      raise ::JWT::EncodeError, 'Signature must be a String' unless signature.is_a?(String)
      raise ::JWT::EncodeError, 'Signature must not be empty' if signature.empty?

      expected = @jwa.signature_length
      return unless expected && signature.bytesize != expected

      raise ::JWT::EncodeError, "Malformed #{algorithm} signature: expected #{expected} bytes (raw R || S, each #{expected / 2} bytes), got #{signature.bytesize}"
    end

    def verify_signature!(signature, key)
      return if @jwa.verify(data: @signing_input, signature: signature, verification_key: key)

      raise ::JWT::VerificationError, 'Signature does not match the prepared signing input'
    end

    def deep_freeze(object)
      case object
      when Hash
        object.each do |key, value|
          deep_freeze(key)
          deep_freeze(value)
        end
      when Array
        object.each { |element| deep_freeze(element) }
      end
      object.freeze
    end
  end
end
