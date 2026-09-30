# frozen_string_literal: true

module JWT
  module Claims
    # Context class to contain the data passed to individual claim validators
    #
    # @api private
    VerificationContext = Struct.new(:payload, keyword_init: true)

    # Verifiers to support the ::JWT.decode method
    #
    # @api private
    module DecodeVerifier
      VERIFIERS = {
        verify_expiration: ->(options) { Claims::Expiration.new(leeway: options[:exp_leeway] || options[:leeway]) },
        verify_not_before: ->(options) { Claims::NotBefore.new(leeway: options[:nbf_leeway] || options[:leeway]) },
        verify_iss: ->(options) { options[:iss] && Claims::Issuer.new(issuers: options[:iss]) },
        verify_iat: ->(options) { Claims::IssuedAt.new(leeway: options[:verify_iat].is_a?(Hash) ? options[:verify_iat][:leeway] : nil) },
        verify_time_constraints: ->(options) do
          config = options[:verify_time_constraints]
          from_config = config.is_a?(::Hash) ? config[:max_token_lifetime] : nil
          max_token_lifetime = from_config || options[:max_token_lifetime]
          Claims::TimeConstraints.new(max_token_lifetime: max_token_lifetime)
        end,
        verify_jti: ->(options) { Claims::JwtId.new(validator: options[:verify_jti]) },
        verify_aud: ->(options) { options[:aud] && Claims::Audience.new(expected_audience: options[:aud]) },
        verify_sub: ->(options) { options[:sub] && Claims::Subject.new(expected_subject: options[:sub]) },
        required_claims: ->(options) { Claims::Required.new(required_claims: options[:required_claims]) }
      }.freeze

      private_constant(:VERIFIERS)

      class << self
        # @api private
        def verify!(payload, options)
          time = Claims::Clock.resolve(options[:clock])
          VERIFIERS.each do |key, verifier_builder|
            next unless enabled?(key, options)

            verifier_builder&.call(options)&.verify!(context: VerificationContext.new(payload: payload), time: time)
          end
          nil
        end

        def enabled?(key, options)
          return true if key == :verify_time_constraints && options[:max_token_lifetime]

          options[key] || options[key.to_s]
        end
      end
    end
  end
end
