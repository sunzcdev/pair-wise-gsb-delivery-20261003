# frozen_string_literal: true

module JWT
  module Claims
    # Validates the relations between the time based claims 'iat', 'nbf' and
    # 'exp' and enforces an optional maximum token lifetime.
    #
    # The relations describe the claims as observed by the issuer, so leeway
    # values must never participate in their comparison: leeway only models
    # clock skew against the verifier and must not make a token whose claim
    # order is reversed look valid.
    class TimeConstraints
      # @param max_token_lifetime [Numeric, nil] the maximum allowed value of
      #   +exp - iat+ in seconds. +nil+ disables the check.
      def initialize(max_token_lifetime: nil)
        @max_token_lifetime = max_token_lifetime
      end

      # @param context [Object] the context containing the JWT payload.
      # @raise [JWT::InvalidTimeConstraintsError] if the claim order is
      #   reversed or the token exceeds its maximum lifetime.
      # @return [nil]
      def verify!(context:, **_args)
        payload = context.payload
        return unless payload.is_a?(Hash)

        iat = numeric_value(payload, 'iat')
        nbf = numeric_value(payload, 'nbf')
        exp = numeric_value(payload, 'exp')

        verify_order!(iat, nbf, exp)
        verify_max_token_lifetime!(iat, exp)
        nil
      end

      private

      attr_reader :max_token_lifetime

      def numeric_value(payload, claim)
        value = payload[claim]
        return nil unless value.is_a?(::Numeric)

        value.to_f
      end

      def verify_order!(iat, nbf, exp)
        raise JWT::InvalidTimeConstraintsError, 'Invalid iat/nbf: nbf is before iat' if iat && nbf && nbf < iat
        raise JWT::InvalidTimeConstraintsError, 'Invalid nbf/exp: exp is before nbf' if nbf && exp && exp < nbf
        raise JWT::InvalidTimeConstraintsError, 'Invalid iat/exp: exp is before iat' if iat && exp && exp < iat
      end

      def verify_max_token_lifetime!(iat, exp)
        return if max_token_lifetime.nil?
        return unless iat && exp

        return if (exp - iat) <= max_token_lifetime.to_f

        raise JWT::InvalidTimeConstraintsError,
              "Invalid exp/iat: token lifetime exceeds the maximum of #{max_token_lifetime} seconds"
      end
    end
  end
end
