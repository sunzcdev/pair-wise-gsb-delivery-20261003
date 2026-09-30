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
        verify_chronology: lambda { |options|
          Claims::Chronology.new(
            leeway: options[:chronology_leeway] || options[:leeway],
            max_age: options[:max_token_age]
          )
        },
        verify_jti: ->(options) { Claims::JwtId.new(validator: options[:verify_jti]) },
        verify_aud: ->(options) { options[:aud] && Claims::Audience.new(expected_audience: options[:aud]) },
        verify_sub: ->(options) { options[:sub] && Claims::Subject.new(expected_subject: options[:sub]) },
        required_claims: ->(options) { Claims::Required.new(required_claims: options[:required_claims]) }
      }.freeze

      private_constant(:VERIFIERS)

      class << self
        # @api private
        def verify!(payload, options)
          now = Clock.resolve(options[:clock] || -> { ::Time.now })
          VERIFIERS.each do |key, verifier_builder|
            next unless verifier_enabled?(key, options)

            verifier_builder&.call(options)&.verify!(context: VerificationContext.new(payload: payload), now: now)
          end
          nil
        end

        private

        def verifier_enabled?(key, options)
          return true if key == :verify_chronology && options[:max_token_age]

          options[key] || options[key.to_s]
        end
      end
    end
  end
end
