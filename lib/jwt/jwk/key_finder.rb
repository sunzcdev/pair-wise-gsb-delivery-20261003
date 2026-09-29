# frozen_string_literal: true

module JWT
  module JWK
    # JSON Web Key keyfinder
    # To find the key for a given kid
    #
    # The KeyFinder never performs network requests itself, fetching the keys
    # (over HTTP or otherwise) is the responsibility of the application
    # provided loader.
    #
    # Instances are thread safe and are meant to be shared when JWKS refresh
    # coordination is desired. Concurrent key misses for the same key id are
    # coordinated on the instance level: only one refresh (a loader call with
    # +kid_not_found: true+) is performed at a time per key id, other threads
    # wait for the ongoing refresh to finish instead of triggering their own.
    #
    # Unknown key ids are remembered in a bounded negative cache so that
    # repeated misses do not hammer the loader. When a refresh fails the
    # previously fetched keys are kept, and a refresh issued earlier can never
    # overwrite keys applied by a newer successful refresh.
    class KeyFinder
      # Default time-to-live in seconds for a negative cache entry of an unknown kid
      DEFAULT_NEGATIVE_CACHE_TTL = 30
      # Default maximum number of unknown kids kept in the negative cache
      DEFAULT_NEGATIVE_CACHE_CAPACITY = 128

      # Initializes a new KeyFinder instance.
      # @param [Hash] options the options to create a KeyFinder with
      # @option options [Proc, JWT::JWK::Set] :jwks the jwks or a loader proc
      # @option options [Boolean] :allow_nil_kid whether to allow nil kid
      # @option options [Array] :key_fields the fields to use for key matching,
      #                         the order of the fields are used to determine
      #                         the priority of the keys.
      # @option options [Numeric] :negative_cache_ttl how long (in seconds) an
      #                         unknown kid is remembered before the loader is
      #                         asked to refresh again.
      # @option options [Integer] :negative_cache_capacity maximum number of
      #                         unknown kids remembered at once.
      def initialize(options)
        @allow_nil_kid = options[:allow_nil_kid]
        jwks_or_loader = options[:jwks]

        @jwks_loader = if jwks_or_loader.respond_to?(:call)
                         jwks_or_loader
                       else
                         ->(_options) { jwks_or_loader }
                       end

        @key_fields = options[:key_fields] || %i[kid]

        @negative_cache_ttl = options.fetch(:negative_cache_ttl, DEFAULT_NEGATIVE_CACHE_TTL)
        @negative_cache_capacity = options.fetch(:negative_cache_capacity, DEFAULT_NEGATIVE_CACHE_CAPACITY)

        @mutex = Mutex.new
        @refresh_condition = ConditionVariable.new
        @jwks = nil
        @refreshes_in_flight = {}
        @negative_cache = {}
        @refresh_generation = 0
        @applied_generation = 0
      end

      # Returns the verification key for the given kid
      # @param [String] kid the key id
      def key_for(kid, key_field = :kid)
        raise ::JWT::MalformedTokenError, "Invalid type for #{key_field} header parameter" unless kid.nil? || kid.is_a?(String)

        jwk = resolve_key(kid, key_field)

        raise ::JWT::SignatureError, 'No keys found in jwks' unless jwks_any?
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
        jwk = find_key(kid, key_field, key_matcher)
        return jwk if jwk

        # Second try, invalidate for backwards compatibility
        refresh_and_find(kid, key_field, key_matcher)
      end

      def find_key(kid, key_field, key_matcher)
        @mutex.synchronize do
          @jwks ||= JWT::JWK::Set.new(@jwks_loader.call(key_field => kid))
          @jwks.find { |key| key_matcher.call(key) }
        end
      end

      def refresh_and_find(kid, key_field, key_matcher)
        generation = nil

        @mutex.synchronize do
          # Wait for an eventual ongoing refresh for the same kid to finish,
          # re-checking the keys and the negative cache after each wakeup
          loop do
            jwk = @jwks.find { |key| key_matcher.call(key) }
            return jwk if jwk
            return nil if negatively_cached?(kid)
            break unless @refreshes_in_flight[kid]

            @refresh_condition.wait(@mutex)
          end

          @refreshes_in_flight[kid] = true
          generation = (@refresh_generation += 1)
        end

        new_jwks = fetch_jwks(kid, key_field)

        @mutex.synchronize do
          if new_jwks
            # A refresh issued earlier must not overwrite keys applied by a
            # newer successful refresh
            if generation > @applied_generation
              @jwks = new_jwks
              @applied_generation = generation
              @negative_cache.delete(kid)
            end
            remember_unknown_kid(kid) unless @jwks.find { |key| key_matcher.call(key) }
          else
            # The refresh failed, keep the previously fetched keys and avoid
            # hammering the loader for the same unknown kid
            remember_unknown_kid(kid)
          end

          @refreshes_in_flight.delete(kid)
          @refresh_condition.broadcast

          @jwks.find { |key| key_matcher.call(key) }
        end
      end

      def fetch_jwks(kid, key_field)
        JWT::JWK::Set.new(@jwks_loader.call(invalidate: true, kid_not_found: true, key_field => kid))
      rescue StandardError
        nil
      end

      def negatively_cached?(kid)
        expires_at = @negative_cache[kid]
        return false unless expires_at

        if monotonic_now >= expires_at
          @negative_cache.delete(kid)
          false
        else
          true
        end
      end

      def remember_unknown_kid(kid)
        @negative_cache.delete(kid)
        @negative_cache[kid] = monotonic_now + @negative_cache_ttl
        @negative_cache.shift while @negative_cache.size > @negative_cache_capacity
      end

      def jwks_any?
        @mutex.synchronize { @jwks&.any? || false }
      end

      def monotonic_now
        Process.clock_gettime(Process::CLOCK_MONOTONIC)
      end
    end
  end
end
