# frozen_string_literal: true

module JWT
  module Configuration
    # The DecodeConfiguration class holds the configuration settings for decoding JWT tokens.
    class DecodeConfiguration
      # @!attribute [rw] verify_expiration
      #   @return [Boolean] whether to verify the expiration claim.
      # @!attribute [rw] verify_not_before
      #   @return [Boolean] whether to verify the not before claim.
      # @!attribute [rw] verify_iss
      #   @return [Boolean] whether to verify the issuer claim.
      # @!attribute [rw] verify_iat
      #   @return [Boolean, Hash] whether to verify the issued at claim. A hash can be given to configure the claim, currently only `leeway` is supported.
      # @!attribute [rw] verify_time_constraints
      #   @return [Boolean, Hash] whether to verify the relations between the iat, nbf and exp claims. A hash with `max_token_lifetime` (in seconds) can be given to also enforce a maximum token lifetime.
      # @!attribute [rw] verify_jti
      #   @return [Boolean] whether to verify the JWT ID claim.
      # @!attribute [rw] verify_aud
      #   @return [Boolean] whether to verify the audience claim.
      # @!attribute [rw] verify_sub
      #   @return [Boolean] whether to verify the subject claim.
      # @!attribute [rw] leeway
      #   @return [Integer] the leeway in seconds for time-based claims.
      # @!attribute [rw] max_token_lifetime
      #   @return [Numeric, nil] the maximum allowed token lifetime in seconds (`exp - iat`). Setting this enables the time constraints verification.
      # @!attribute [rw] clock
      #   @return [#call, Time, Numeric, nil] the clock used for time-based claim verification. It is observed once per verification round.
      # @!attribute [rw] algorithms
      #   @return [Array<String>] the list of acceptable algorithms.
      # @!attribute [rw] required_claims
      #   @return [Array<String>] the list of required claims.
      # @!attribute [rw] enforce_hmac_key_length
      #   @return [Boolean] whether to enforce minimum HMAC key lengths. false disables validation (default).

      attr_accessor :verify_expiration,
                    :verify_not_before,
                    :verify_iss,
                    :verify_iat,
                    :verify_time_constraints,
                    :verify_jti,
                    :verify_aud,
                    :verify_sub,
                    :leeway,
                    :max_token_lifetime,
                    :clock,
                    :algorithms,
                    :required_claims,
                    :enforce_hmac_key_length

      # Initializes a new DecodeConfiguration instance with default settings.
      def initialize
        @verify_expiration = true
        @verify_not_before = true
        @verify_iss = false
        @verify_iat = false
        @verify_time_constraints = false
        @verify_jti = false
        @verify_aud = false
        @verify_sub = false
        @leeway = 0
        @max_token_lifetime = nil
        @clock = nil
        @algorithms = ['HS256']
        @required_claims = []
        @enforce_hmac_key_length = false
      end

      # @api private
      def to_h
        {
          verify_expiration: verify_expiration,
          verify_not_before: verify_not_before,
          verify_iss: verify_iss,
          verify_iat: verify_iat,
          verify_time_constraints: verify_time_constraints,
          verify_jti: verify_jti,
          verify_aud: verify_aud,
          verify_sub: verify_sub,
          leeway: leeway,
          max_token_lifetime: max_token_lifetime,
          clock: clock,
          algorithms: algorithms,
          required_claims: required_claims,
          enforce_hmac_key_length: enforce_hmac_key_length
        }
      end
    end
  end
end
