# frozen_string_literal: true

module JWT
  module JWK
    # Explicit policy describing which JWKs are acceptable candidates for
    # signature verification.
    #
    # A key is a candidate only when all of its declared metadata agrees with
    # the verification purpose:
    #
    # * +kty+ - when the policy declares a key type, the key must carry the
    #   same +kty+. Keys without a +kty+ are rejected in that case.
    # * +alg+ - when the policy declares an algorithm and the key carries an
    #   +alg+ member, the two must be equal. Keys without +alg+ are accepted,
    #   as the member is optional in RFC 7517.
    # * +use+ - when the key carries a +use+ member it must be +"sig"+.
    #   Keys without +use+ are accepted, as the member is optional in RFC 7517.
    # * +key_ops+ - when the key carries a +key_ops+ member it must include
    #   +"verify"+. Keys without +key_ops+ are accepted, as the member is
    #   optional in RFC 7517.
    #
    # Contradictory declarations are never ignored: every declared member is
    # checked, so a key declaring for example +use: "sig"+ together with
    # +key_ops: ["encrypt"]+ is rejected.
    class VerificationPolicy
      # The "use" member value required for signature verification.
      SIGNATURE_USE = 'sig'
      # The "key_ops" member value required for signature verification.
      VERIFY_KEY_OP = 'verify'

      attr_reader :kty, :algorithm

      # @param kty [String, Symbol, nil] the key type ("kty") the key must
      #   declare, or nil to accept any key type.
      # @param algorithm [String, Symbol, nil] the algorithm the key must be
      #   usable with, or nil to accept any algorithm.
      def initialize(kty: nil, algorithm: nil)
        @kty = kty&.to_s
        @algorithm = algorithm&.to_s
      end

      # Returns the keys from the given enumerable that satisfy the policy.
      # @param keys [Enumerable<JWT::JWK::KeyBase>] the keys to filter.
      # @return [Array<JWT::JWK::KeyBase>] the candidate keys.
      def candidates(keys)
        keys.select { |key| match?(key) }
      end

      # @param key [JWT::JWK::KeyBase] the key to check.
      # @return [Boolean] whether the key satisfies the policy.
      def match?(key)
        kty_matches?(key) && alg_matches?(key) && use_matches?(key) && key_ops_match?(key)
      end

      private

      def kty_matches?(key)
        return true unless kty

        key[:kty] == kty
      end

      def alg_matches?(key)
        return true unless algorithm
        return true unless key[:alg]

        key[:alg] == algorithm
      end

      def use_matches?(key)
        return true unless key[:use]

        key[:use] == SIGNATURE_USE
      end

      def key_ops_match?(key)
        return true unless key[:key_ops]

        Array(key[:key_ops]).include?(VERIFY_KEY_OP)
      end
    end
  end
end
