# frozen_string_literal: true

module JWT
  module JWK
    # JSON Web Key keyfinder
    # To find the key for a given kid
    class KeyFinder
      # Coordinates one in-flight JWKS refresh. Concurrent lookups for the
      # same unknown kid wait for the shared refresh instead of each
      # triggering the JWKS loader.
      class Refresh
        def initialize(generation)
          @generation = generation
          @condition = ConditionVariable.new
          @done = false
          @error = nil
        end

        attr_reader :generation, :error

        def wait(mutex)
          @condition.wait(mutex) until @done
        end

        def complete!(error = nil)
          @error = error
          @done = true
          @condition.broadcast
        end
      end

      private_constant :Refresh

      # Default time in seconds an unknown kid is remembered to avoid
      # hammering the JWKS loader.
      DEFAULT_UNKNOWN_KID_CACHE_TTL = 60

      # Default maximum number of unknown kids remembered at a time.
      DEFAULT_UNKNOWN_KID_CACHE_SIZE = 128

      # Initializes a new KeyFinder instance.
      # @param [Hash] options the options to create a KeyFinder with
      # @option options [Proc, JWT::JWK::Set] :jwks the jwks or a loader proc
      # @option options [Boolean] :allow_nil_kid whether to allow nil kid
      # @option options [Array] :key_fields the fields to use for key matching,
      #                         the order of the fields are used to determine
      #                         the priority of the keys.
      # @option options [Numeric] :unknown_kid_cache_ttl how long (in seconds)
      #                         an unknown kid is remembered before the JWKS
      #                         loader is asked again. Zero disables the cache.
      # @option options [Integer] :unknown_kid_cache_size the maximum number of
      #                         unknown kids remembered at a time.
      def initialize(options)
        @allow_nil_kid = options[:allow_nil_kid]
        jwks_or_loader = options[:jwks]

        @jwks_loader = if jwks_or_loader.respond_to?(:call)
                         jwks_or_loader
                       else
                         ->(_options) { jwks_or_loader }
                       end

        @key_fields = options[:key_fields] || %i[kid]
        @unknown_kid_cache_ttl = options.fetch(:unknown_kid_cache_ttl, DEFAULT_UNKNOWN_KID_CACHE_TTL)
        @unknown_kid_cache_size = options.fetch(:unknown_kid_cache_size, DEFAULT_UNKNOWN_KID_CACHE_SIZE)

        @lock = Mutex.new
        @jwks = nil
        @generation = 0
        @applied_generation = 0
        @refreshes = {}
        @unknown_kids = {}
      end

      # Returns the verification key for the given kid
      # @param [String] kid the key id
      def key_for(kid, key_field = :kid)
        raise ::JWT::MalformedTokenError, "Invalid type for #{key_field} header parameter" unless kid.nil? || kid.is_a?(String)

        jwk = resolve_key(kid, key_field)

        raise ::JWT::SignatureError, 'No keys found in jwks' unless current_jwks.any?
        raise ::JWT::SignatureError, "Could not find public key for kid #{kid}" unless jwk

        jwk.verify_key
      end

      # Returns the key for the given token
      # @param [JWT::EncodedToken] token the token
      def call(token)
        @key_fields.each do |key_field|
          field_value = token.header[key_field.to_s]

          return key_for(field_value, key_field) if field_value
        end

        raise ::JWT::SignatureError, 'No key id (kid) or x5t found from token headers' unless @allow_nil_kid

        kid = token.header['kid']
        key_for(kid)
      end

      private

      def resolve_key(kid, key_field)
        key_matcher = ->(key) { (kid.nil? && @allow_nil_kid) || key[key_field] == kid }

        # First try without invalidation to facilitate application caching
        jwk = initial_jwks(key_field, kid).find { |key| key_matcher.call(key) }
        return jwk if jwk

        # Unknown kids are remembered for a while to avoid hammering the loader
        return nil if @lock.synchronize { unknown_kid?(kid) }

        # Second try, invalidate for backwards compatibility
        refresh_jwks(kid, key_field, key_matcher)
      end

      def initial_jwks(key_field, kid)
        @lock.synchronize { @jwks ||= JWT::JWK::Set.new(@jwks_loader.call(key_field => kid)) }
      end

      def current_jwks
        @lock.synchronize { @jwks }
      end

      def refresh_jwks(kid, key_field, key_matcher)
        action, payload = @lock.synchronize { refresh_action(kid, key_matcher) }

        case action
        when :found   then payload
        when :unknown then nil
        when :wait    then await_refresh(payload, key_matcher)
        else               perform_refresh(payload, kid, key_field, key_matcher)
        end
      end

      def refresh_action(kid, key_matcher)
        jwk = @jwks.find { |key| key_matcher.call(key) }
        return [:found, jwk] if jwk
        return [:unknown, nil] if unknown_kid?(kid)

        attempt = @refreshes[kid]
        return [:wait, attempt] if attempt

        attempt = Refresh.new(@generation += 1)
        @refreshes[kid] = attempt
        [:refresh, attempt]
      end

      def perform_refresh(attempt, kid, key_field, key_matcher)
        new_jwks = load_refreshed_jwks(attempt, kid, key_field)

        @lock.synchronize do
          # A refresh issued earlier must not overwrite a newer successful result
          if attempt.generation > @applied_generation
            @jwks = new_jwks
            @applied_generation = attempt.generation
          end

          jwk = @jwks.find { |key| key_matcher.call(key) }
          remember_unknown_kid(kid) unless jwk
          complete_refresh(kid, attempt)
          jwk
        end
      end

      def load_refreshed_jwks(attempt, kid, key_field)
        JWT::JWK::Set.new(@jwks_loader.call(invalidate: true, kid_not_found: true, key_field => kid))
      rescue StandardError => e
        # A failed refresh keeps the previously loaded keys
        @lock.synchronize { complete_refresh(kid, attempt, e) }
        raise
      end

      def await_refresh(attempt, key_matcher)
        @lock.synchronize do
          attempt.wait(@lock)
          raise attempt.error if attempt.error

          @jwks.find { |key| key_matcher.call(key) }
        end
      end

      def complete_refresh(kid, attempt, error = nil)
        @refreshes.delete(kid)
        attempt.complete!(error)
      end

      def unknown_kid?(kid)
        seen_at = @unknown_kids[kid]
        return false unless seen_at

        return true if monotonic_now - seen_at < @unknown_kid_cache_ttl

        @unknown_kids.delete(kid)
        false
      end

      def remember_unknown_kid(kid)
        return if @unknown_kid_cache_size < 1

        @unknown_kids.delete(kid)
        @unknown_kids[kid] = monotonic_now
        @unknown_kids.shift while @unknown_kids.size > @unknown_kid_cache_size
      end

      def monotonic_now
        Process.clock_gettime(Process::CLOCK_MONOTONIC)
      end
    end
  end
end
