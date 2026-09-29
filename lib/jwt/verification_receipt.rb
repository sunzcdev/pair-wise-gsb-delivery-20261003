# frozen_string_literal: true

module JWT
  # An immutable record of a fully successful token verification.
  #
  # A receipt binds the raw bytes of the verified token, the algorithm that
  # was used and a caller provided policy identifier, and records the moment
  # of verification together with the checks that passed.
  #
  # Receipts can only be issued by the library after a complete successful
  # verification (see JWT::EncodedToken#verify_with_receipt!). They are
  # historical records: a receipt proves that at +verified_at+ the token was
  # verified under +policy+, it does not grant access to the token payload
  # and it says nothing about any other policy.
  #
  # @example
  #   receipt = token.verify_with_receipt!(signature: { algorithm: 'HS256', key: 'secret' },
  #                                        claims: [:exp],
  #                                        policy: :public_api)
  #   receipt.valid_for?(:public_api) # => true
  #   receipt.valid_for?(:admin_api)  # => false
  class VerificationReceipt
    # Returns the raw bytes of the verified token.
    # @return [String] the original encoded JWT.
    attr_reader :token

    # Returns the algorithm the token was verified with.
    # @return [String] the algorithm from the token header.
    attr_reader :algorithm

    # Returns the caller provided policy identifier the verification was performed under.
    # @return [Object] the policy identifier.
    attr_reader :policy

    # Returns the moment the verification succeeded.
    # @return [Time] the verification time.
    attr_reader :verified_at

    # Returns the checks that passed, always including +:signature+.
    # @return [Array<Symbol>] the passed checks.
    attr_reader :checks

    # Issues a receipt for a successfully verified token.
    #
    # @api private
    # @param token [String] the raw encoded token.
    # @param algorithm [String] the algorithm the token was verified with.
    # @param policy [Object] the caller provided policy identifier.
    # @param checks [Array<Symbol>] the checks that passed.
    # @return [JWT::VerificationReceipt] the issued receipt.
    def self.issue(token:, algorithm:, policy:, checks:)
      new(token: token,
          algorithm: algorithm,
          policy: policy,
          checks: checks,
          verified_at: Time.now)
    end

    # @api private
    def initialize(token:, algorithm:, policy:, checks:, verified_at:)
      @token       = String(token).dup.freeze
      @algorithm   = String(algorithm).freeze
      @policy      = policy.is_a?(String) ? policy.dup.freeze : policy
      @checks      = checks.map(&:to_sym).uniq.freeze
      @verified_at = verified_at.freeze
      freeze
    end
    private_class_method :new

    # Returns whether this receipt was issued under the given policy.
    #
    # A receipt only ever proves the policy it was issued for. Verification
    # under another policy requires a new verification and a new receipt.
    #
    # @param other_policy [Object] the policy identifier to check against.
    # @return [Boolean] true if the receipt was issued under +other_policy+.
    def valid_for?(other_policy)
      policy == other_policy
    end

    # Returns whether the given check was part of the successful verification.
    #
    # @param check [Symbol] the check to look for, e.g. +:signature+ or +:exp+.
    # @return [Boolean] true if the check passed during verification.
    def covers?(check)
      checks.include?(check.to_sym)
    end

    # Returns whether this receipt was issued for the given token.
    #
    # @param other [String, JWT::EncodedToken] the token to compare with.
    # @return [Boolean] true if the raw token bytes match.
    def matches?(other)
      token == (other.respond_to?(:jwt) ? other.jwt : other)
    end

    # @return [Hash] the receipt attributes as a Hash.
    def to_h
      {
        token: token,
        algorithm: algorithm,
        policy: policy,
        verified_at: verified_at,
        checks: checks
      }
    end

    # Receipts are value objects: two receipts are equal when all their
    # attributes, including the verification time, are equal.
    def ==(other)
      other.is_a?(VerificationReceipt) && to_h == other.to_h
    end
    alias eql? ==

    # @return [Integer] the hash code of the receipt.
    def hash
      to_h.hash
    end
  end
end
