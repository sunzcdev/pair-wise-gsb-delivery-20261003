# frozen_string_literal: true

require_relative '../jwa'

module JWT
  module JWK
    # Explicit verification-purpose policy for selecting a key from a JWK Set.
    #
    # A JWK Set may mix signing and encryption keys and reuse the same +kid+
    # for entries of different key types, so matching on +kid+ alone (and
    # picking the first entry) is not sufficient. This policy jointly
    # evaluates the +kty+, +alg+, +use+ and +key_ops+ members of every
    # candidate against the algorithm of the token being verified:
    #
    # * +kty+ must match the key type required by the token algorithm
    #   ("RSA" for RS*/PS*, "EC" for ES*, "oct" for HS*).
    # * +alg+, when present, must equal the token algorithm.
    # * +use+, when present, must be "sig".
    # * +key_ops+, when present, must include "verify".
    #
    # Missing metadata: +alg+, +use+ and +key_ops+ are optional per RFC 7517.
    # An absent member imposes no restriction and the key remains a candidate.
    #
    # Contradictory declarations are never silently ignored: a key declaring
    # +use: "sig"+ together with a +key_ops+ lacking "verify" (or a non-"sig"
    # +use+ together with a +key_ops+ containing "verify"), or an +alg+ member
    # incompatible with the key's +kty+, raises a JWT::JWKError.
    #
    # If more than one key satisfies all checks the selection is ambiguous and
    # a JWT::SignatureError is raised instead of depending on the order of the
    # keys in the set.
    class VerificationPolicy
      SIGNATURE_USE    = 'sig'
      VERIFY_OPERATION = 'verify'

      # Maps a JWA implementation to the JWK "kty" usable for verification.
      KTY_BY_JWA = {
        JWA::Hmac  => 'oct',
        JWA::Rsa   => 'RSA',
        JWA::Ps    => 'RSA',
        JWA::Ecdsa => 'EC'
      }.freeze

      # Selects the single key from +keys+ usable for verifying a token
      # signed with +algorithm+.
      #
      # @param keys [Array<JWT::JWK::KeyBase>] the candidate keys, for example
      #        all keys matching the token's kid.
      # @param algorithm [String] the algorithm of the token being verified.
      # @return [JWT::JWK::KeyBase, nil] the selected key, or nil when no key qualifies.
      # @raise [ArgumentError] when no algorithm is given.
      # @raise [JWT::JWKError] when a candidate carries contradictory metadata.
      # @raise [JWT::SignatureError] when more than one key qualifies.
      def select(keys, algorithm:)
        raise ArgumentError, 'An algorithm is required to apply the verification policy' if algorithm.nil?

        expected_kty = kty_for_algorithm(algorithm)
        candidates = keys.select { |key| candidate?(key, algorithm, expected_kty) }

        return candidates.first if candidates.size <= 1

        raise ::JWT::SignatureError,
              "Multiple keys match kid #{candidates.first[:kid].inspect} for algorithm #{algorithm}"
      end

      private

      def candidate?(key, algorithm, expected_kty)
        validate_consistency!(key)

        return false if expected_kty && key[:kty] != expected_kty
        return false if key[:alg] && key[:alg].to_s != algorithm.to_s
        return false if key[:use] && key[:use].to_s != SIGNATURE_USE
        return false if key[:key_ops] && !key_operations(key).include?(VERIFY_OPERATION)

        true
      end

      def validate_consistency!(key)
        use = key[:use]&.to_s
        if use && key[:key_ops] && (use == SIGNATURE_USE) != key_operations(key).include?(VERIFY_OPERATION)
          raise ::JWT::JWKError,
                "Key #{key[:kid].inspect} declares contradictory 'use' (#{use}) and 'key_ops' (#{key_operations(key).join(', ')})"
        end

        key_alg = key[:alg]
        return unless key_alg

        alg_kty = kty_for_algorithm(key_alg)
        return unless alg_kty && key[:kty] && alg_kty != key[:kty]

        raise ::JWT::JWKError,
              "Key #{key[:kid].inspect} declares 'alg' #{key_alg} incompatible with 'kty' #{key[:kty]}"
      end

      def key_operations(key)
        Array(key[:key_ops]).map(&:to_s)
      end

      def kty_for_algorithm(algorithm)
        jwa = JWA.resolve(algorithm)
        KTY_BY_JWA.each { |klass, kty| return kty if jwa.is_a?(klass) }
        nil
      end
    end
  end
end
