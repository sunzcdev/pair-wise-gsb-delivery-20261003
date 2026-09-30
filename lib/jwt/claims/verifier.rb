# frozen_string_literal: true

module JWT
  module Claims
    # @api private
    module Verifier
      # Reserved option key carrying the injectable clock. It is not a claim
      # verifier and is consumed before the verifiers are iterated.
      CLOCK_KEY = :clock

      private_constant(:CLOCK_KEY)

      VERIFIERS = {
        exp: ->(options) { Claims::Expiration.new(leeway: options.dig(:exp, :leeway)) },
        nbf: ->(options) { Claims::NotBefore.new(leeway: options.dig(:nbf, :leeway)) },
        iss: ->(options) { Claims::Issuer.new(issuers: options[:iss]) },
        iat: ->(options) { Claims::IssuedAt.new(leeway: options.dig(:iat, :leeway)) },
        chronology: lambda { |options|
          Claims::Chronology.new(
            leeway: options.dig(:chronology, :leeway),
            max_age: options.dig(:chronology, :max_age)
          )
        },
        jti: ->(options) { Claims::JwtId.new(validator: options[:jti]) },
        aud: ->(options) { Claims::Audience.new(expected_audience: options[:aud]) },
        sub: ->(options) { Claims::Subject.new(expected_subject: options[:sub]) },
        crit: ->(options) { Claims::Crit.new(expected_crits: options[:crit]) },
        required: ->(options) { Claims::Required.new(required_claims: options[:required]) },
        numeric: ->(*)        { Claims::Numeric.new }
      }.freeze

      private_constant(:VERIFIERS)

      class << self
        # @api private
        def verify!(context, *options)
          now = observed_time(options)
          iterate_verifiers(*options) do |verifier, verifier_options|
            verify_one!(context, verifier, verifier_options, now: now)
          end
          nil
        end

        # @api private
        def errors(context, *options)
          errors = []
          now = observed_time(options)
          iterate_verifiers(*options) do |verifier, verifier_options|
            verify_one!(context, verifier, verifier_options, now: now)
          rescue ::JWT::ClaimValidationError, ::JWT::MalformedTokenError => e
            # A payload that cannot be decoded has no valid claims either, so the
            # predicate API reports it instead of raising.
            errors << JWT::Claims::Error.new(message: e.message)
          end
          errors
        end

        private

        def observed_time(options)
          clock = options.find { |option| option.is_a?(Hash) && option.key?(CLOCK_KEY) }&.fetch(CLOCK_KEY)
          Clock.resolve(clock || -> { ::Time.now })
        end

        def iterate_verifiers(*options)
          options.each do |element|
            if element.is_a?(Hash)
              element.each_key { |key| yield(key, element) unless key == CLOCK_KEY }
            else
              yield(element, {})
            end
          end
        end

        def verify_one!(context, verifier, options, now:)
          verifier_builder = VERIFIERS.fetch(verifier) { raise ArgumentError, "#{verifier} not a valid claim verifier" }
          verifier_builder.call(options || {}).verify!(context: context, now: now)
        end
      end
    end
  end
end
