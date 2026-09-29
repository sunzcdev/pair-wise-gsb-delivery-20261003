# frozen_string_literal: true

module JWT
  # An immutable receipt proving that an {EncodedToken} passed a complete
  # verification (signature and claims) under a caller identified policy.
  #
  # Receipts are issued exclusively by {EncodedToken#verify_with_receipt!}
  # after a fully successful verification. There is intentionally no public
  # constructor and no API to mint a receipt from an unverified payload.
  #
  # Semantics:
  # - Repeated verification performs the full verification again and issues a
  #   new receipt with a fresh +verified_at+; previously issued receipts are
  #   never mutated or reused.
  # - A failed verification raises and issues no receipt, leaving the token's
  #   payload access rules untouched.
  # - A receipt is bound to the exact token bytes and policy it was issued
  #   for. It proves nothing about any other policy (see {#covers?}) and
  #   nothing about a token whose bytes have since changed (see {#for_token?}).
  class VerificationReceipt
    # Returns the raw token bytes the receipt was issued for.
    # @return [String] the encoded JWT token.
    attr_reader :jwt

    # Returns the algorithm the token was verified with.
    # @return [String] the algorithm from the token header.
    attr_reader :algorithm

    # Returns the caller provided policy identifier the verification was performed under.
    # @return [Object] the policy identifier.
    attr_reader :policy

    # Returns the moment the verification completed.
    # @return [Time] the verification time.
    attr_reader :verified_at

    # Returns the checks that passed, always including +:signature+.
    # @return [Array<Symbol>] the verified checks.
    attr_reader :checks

    # @api private
    def initialize(jwt:, algorithm:, policy:, verified_at:, checks:)
      @jwt         = jwt.dup.freeze
      @algorithm   = algorithm&.dup&.freeze
      @policy      = policy.dup.freeze
      @verified_at = verified_at.dup.freeze
      @checks      = checks.map(&:to_sym).freeze

      freeze
    end

    private_class_method :new

    # Returns whether this receipt was issued under the given policy.
    #
    # A receipt issued for one policy never proves that another policy has
    # passed, even when both policies verified the same token bytes.
    #
    # @param policy [Object] the policy identifier to check against.
    # @return [Boolean] whether the receipt covers the given policy.
    def covers?(policy)
      self.policy == policy
    end

    # Returns whether this receipt was issued for the given token's current bytes.
    #
    # Compares against the token's current segments, so a token mutated via
    # {EncodedToken#encoded_payload=} after the receipt was issued no longer
    # matches.
    #
    # @param token [EncodedToken] the token to compare against.
    # @return [Boolean] whether the receipt matches the token bytes.
    def for_token?(token)
      jwt == [token.encoded_header, token.encoded_payload, token.encoded_signature].join('.')
    end

    # Receipts are value objects: two receipts are equal when every bound
    # attribute matches, including the policy and the verification time.
    def ==(other)
      other.is_a?(VerificationReceipt) &&
        jwt == other.jwt &&
        algorithm == other.algorithm &&
        policy == other.policy &&
        verified_at == other.verified_at &&
        checks == other.checks
    end
    alias eql? ==

    def hash
      [self.class, jwt, algorithm, policy, verified_at, checks].hash
    end
  end
end
