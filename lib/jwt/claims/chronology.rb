# frozen_string_literal: true

module JWT
  module Claims
    # Validates the relation between the time based claims 'iat', 'nbf' and
    # 'exp'. A token must have been issued before (or at the same time as) it
    # becomes valid or expires, and it must become valid before (or at the same
    # time as) it expires.
    #
    # An optional maximum token age can be enforced against the 'iat' claim.
    #
    # The leeway only covers clock-skew sized inversions: an earlier claim is
    # accepted as long as it is not more than +leeway+ seconds after the later
    # claim. It can never make a genuinely reversed order valid.
    class Chronology
      # Pairs of claims that must be in chronological order, earliest first.
      ORDER = [
        %w[iat nbf],
        %w[iat exp],
        %w[nbf exp]
      ].freeze

      private_constant(:ORDER)

      # Initializes a new Chronology instance.
      #
      # @param leeway [Integer] the drift (in seconds) tolerated between the time claims. Default: 0.
      # @param max_age [Integer, nil] the maximum accepted token age in seconds, measured from 'iat' to the observation time.
      def initialize(leeway: nil, max_age: nil)
        @leeway = leeway || 0
        @max_age = max_age
      end

      # Verifies the order of the 'iat', 'nbf' and 'exp' claims and, when
      # configured, the maximum age of the token.
      #
      # @param context [Object] the context containing the JWT payload.
      # @param now [Time, Numeric, nil] the single observation time of the current verification round.
      # @param _args [Hash] additional arguments (not used).
      # @raise [JWT::InvalidChronologyError] if the time claims are in an invalid order.
      # @raise [JWT::InvalidMaxAgeError] if the token is older than the configured maximum age.
      # @return [nil]
      def verify!(context:, now: nil, **_args)
        return unless context.payload.is_a?(Hash)

        validate_claim_order(context.payload)
        validate_max_age(context.payload, now)
        nil
      end

      private

      attr_reader :leeway, :max_age

      def validate_claim_order(payload)
        ORDER.each do |earlier, later|
          next unless time_claim?(payload, earlier) && time_claim?(payload, later)

          if payload[earlier].to_f > payload[later].to_f + leeway
            raise JWT::InvalidChronologyError,
                  "Invalid claim order: '#{earlier}' (#{payload[earlier]}) is after '#{later}' (#{payload[later]}) by more than the allowed leeway of #{leeway} seconds"
          end
        end
      end

      def validate_max_age(payload, now)
        return if max_age.nil?
        return unless time_claim?(payload, 'iat')

        observed_at = (now || ::Time.now).to_f
        return if observed_at - payload['iat'].to_f <= max_age.to_f + leeway

        raise JWT::InvalidMaxAgeError,
              "Token is older than the maximum allowed age of #{max_age} seconds"
      end

      def time_claim?(payload, claim)
        payload[claim].is_a?(::Numeric)
      end
    end
  end
end
