# frozen_string_literal: true

module JWT
  module JWK
    # JSON Web Key keyfinder
    # To find the key for a given kid
    class KeyFinder
      # Initializes a new KeyFinder instance.
      # @param [Hash] options the options to create a KeyFinder with
      # @option options [Proc, JWT::JWK::Set] :jwks the jwks or a loader proc
      # @option options [Boolean] :allow_nil_kid whether to allow nil kid
      # @option options [Array] :key_fields the fields to use for key matching,
      #                         the order of the fields are used to determine
      #                         the priority of the keys.
      # @option options [Boolean, JWT::JWK::VerificationPolicy] :verification_policy
      #                         an explicit verification purpose policy used to
      #                         select among multiple keys matching the key
      #                         field. When enabled, the kty, alg, use and
      #                         key_ops members are jointly checked against the
      #                         token algorithm. When not given, the first key
      #                         matching the key field is returned, preserving
      #                         the historical behaviour.
      def initialize(options)
        @allow_nil_kid = options[:allow_nil_kid]
        @verification_policy = resolve_verification_policy(options[:verification_policy])
        jwks_or_loader = options[:jwks]

        @jwks_loader = if jwks_or_loader.respond_to?(:call)
                         jwks_or_loader
                       else
                         ->(_options) { jwks_or_loader }
                       end

        @key_fields = options[:key_fields] || %i[kid]
      end

      # Returns the verification key for the given kid
      # @param [String] kid the key id
      # @param [String] algorithm the algorithm of the token being verified,
      #                  required when a verification policy is enabled
      def key_for(kid, key_field = :kid, algorithm: nil)
        raise ::JWT::MalformedTokenError, "Invalid type for #{key_field} header parameter" unless kid.nil? || kid.is_a?(String)
        raise ArgumentError, 'An algorithm is required when a verification policy is enabled' if @verification_policy && algorithm.nil?

        jwk = resolve_key(kid, key_field, algorithm)

        raise ::JWT::SignatureError, 'No keys found in jwks' unless @jwks.any?
        raise ::JWT::SignatureError, "Could not find public key for kid #{kid}" unless jwk

        jwk.verify_key
      end

      # Returns the key for the given token
      # @param [JWT::EncodedToken] token the token
      def call(token)
        @key_fields.each do |key_field|
          field_value = token.header[key_field.to_s]

          return key_for(field_value, key_field, algorithm: token.header['alg']) if field_value
        end

        raise ::JWT::SignatureError, 'No key id (kid) or x5t found from token headers' unless @allow_nil_kid

        kid = token.header['kid']
        key_for(kid, algorithm: token.header['alg'])
      end

      private

      def resolve_verification_policy(option)
        case option
        when nil   then nil
        when true  then VerificationPolicy.new
        else option
        end
      end

      def resolve_key(kid, key_field, algorithm)
        key_matcher = ->(key) { (kid.nil? && @allow_nil_kid) || key[key_field] == kid }

        # First try without invalidation to facilitate application caching
        @jwks ||= JWT::JWK::Set.new(@jwks_loader.call(key_field => kid))
        jwk = find_key(key_matcher, algorithm)

        return jwk if jwk

        # Second try, invalidate for backwards compatibility
        @jwks = JWT::JWK::Set.new(@jwks_loader.call(invalidate: true, kid_not_found: true, key_field => kid))
        find_key(key_matcher, algorithm)
      end

      def find_key(key_matcher, algorithm)
        matches = @jwks.select { |key| key_matcher.call(key) }
        return matches.first unless @verification_policy
        return nil if matches.empty?

        @verification_policy.select(matches, algorithm: algorithm)
      end
    end
  end
end
