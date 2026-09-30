# frozen_string_literal: true

module JWT
  module Claims
    # The Expiration class is responsible for validating the expiration claim ('exp') in a JWT token.
    class Expiration
      # Initializes a new Expiration instance.
      #
      # @param leeway [Integer] the amount of leeway (in seconds) to allow when validating the expiration time. Default: 0.
      def initialize(leeway:)
        @leeway = leeway || 0
      end

      # Verifies the expiration claim ('exp') in the JWT token.
      #
      # @param context [Object] the context containing the JWT payload.
      # @param time [Time] the fixed observation time for the verification round.
      # @raise [JWT::ExpiredSignature] if the token has expired.
      # @return [nil]
      def verify!(context:, time: nil, **_args)
        return unless context.payload.is_a?(Hash)
        return unless context.payload.key?('exp')

        observed_at = (time || ::Time.now).to_f
        # RFC 7519: the token must not be accepted on or after 'exp', so the
        # boundary itself is expired. Leeway extends the accepted window.
        raise JWT::ExpiredSignature, 'Signature has expired' if context.payload['exp'].to_f <= (observed_at - leeway)
      end

      private

      attr_reader :leeway
    end
  end
end
