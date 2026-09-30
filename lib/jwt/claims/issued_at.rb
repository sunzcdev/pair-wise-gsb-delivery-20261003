# frozen_string_literal: true

module JWT
  module Claims
    # The IssuedAt class is responsible for validating the issued at claim ('iat') in a JWT token.
    class IssuedAt
      # Initializes a new IssuedAt instance.
      #
      # @param leeway [Integer] the drift (in seconds) to allow between the clock of the issuer and the clock of the verifier. Default: 0.
      def initialize(leeway: 0)
        @leeway = leeway || 0
      end

      # Verifies the issued at claim ('iat') in the JWT token.
      #
      # @param context [Object] the context containing the JWT payload.
      # @param now [Time, Numeric, nil] the single observation time of the current verification round.
      # @param _args [Hash] additional arguments (not used).
      # @raise [JWT::InvalidIatError] if the issued at claim is invalid.
      # @return [nil]
      def verify!(context:, now: nil, **_args)
        return unless context.payload.is_a?(Hash)
        return unless context.payload.key?('iat')

        iat = context.payload['iat']
        observed_at = (now || Time.now).to_f
        raise(JWT::InvalidIatError, 'Invalid iat') if !iat.is_a?(::Numeric) || iat.to_f > observed_at + leeway
      end

      private

      attr_reader :leeway
    end
  end
end
