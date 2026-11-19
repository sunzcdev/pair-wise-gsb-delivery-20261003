# frozen_string_literal: true

module Net
  class HTTP
    # Raised when a credential set cannot be published to a
    # Net::HTTP::ProxyCredentialStore.  The store keeps the previously
    # published snapshot, so a failed rotation never leaves a connection
    # without usable (older) credentials.
    class ProxyCredentialError < ArgumentError; end

    # Raised when the proxy server rejects the credentials offered in a
    # CONNECT handshake, i.e. the handshake ends with a 407 (Proxy
    # Authentication Required) response.
    #
    # This is deliberately distinct from failures of the target service
    # behind the proxy: a proxy that cannot reach the origin answers the
    # CONNECT with a regular error response (for example 502), which keeps
    # raising the usual Net::HTTPResponse exception (Net::HTTPFatalError).
    #
    # The rejected response is available via #response (from
    # Net::HTTPExceptions) and the generation of the credential snapshot
    # that was used on the wire via #generation.  Only the generation
    # identifier is exposed, never the credentials themselves.
    class ProxyAuthenticationError < HTTPClientException
      # The generation identifier of the credential snapshot used by the
      # failed handshake, or nil when only static proxy credentials (or no
      # credentials at all) were in use.
      attr_reader :generation

      def initialize(msg, response, generation = nil) #:nodoc:
        super(msg, response)
        @generation = generation
      end
    end

    # Net::HTTP::ProxyCredentialStore holds proxy credentials as a sequence
    # of immutable, generation-tagged snapshots, so that a platform can
    # rotate proxy credentials while Net::HTTP objects are in use.
    #
    # Semantics:
    #
    # - #publish validates and atomically installs a new snapshot, returning
    #   its generation identifier.  When validation fails,
    #   Net::HTTP::ProxyCredentialError is raised and the previously
    #   published snapshot is retained unchanged.
    # - New connections, and new non-tunneled requests, take the latest
    #   published snapshot.
    # - A CONNECT handshake pins the snapshot that is current when the
    #   handshake begins.  A 407 round-trip inside that handshake keeps
    #   using the pinned snapshot, so one handshake never mixes two
    #   credential identities, even if the platform publishes new
    #   credentials midway.
    # - Diagnostics expose generation identifiers only, never credentials.
    #
    # Example:
    #
    #   store = Net::HTTP::ProxyCredentialStore.new
    #   store.publish('user1', 'secret1')  # => 1
    #
    #   http = Net::HTTP.new('example.com', 443, 'proxy.example', 8080)
    #   http.proxy_credential_store = store
    #
    #   store.publish('user2', 'secret2')  # => 2
    #   http.proxy_credential_diagnostics
    #   # => {published_generation: 2, in_use_generation: nil}
    #
    class ProxyCredentialStore
      # An immutable view of one published credential set.  The
      # precomputed +authorization+ value is the complete
      # +Proxy-Authorization+ header value.
      class Snapshot #:nodoc:
        attr_reader :user, :pass, :generation, :authorization

        def initialize(user, pass, generation)
          @user = user.nil? ? nil : +user.dup.freeze
          @pass = pass.nil? ? nil : +pass.dup.freeze
          if /[\r\n]/.match?(@user.to_s) || /[\r\n]/.match?(@pass.to_s)
            raise ArgumentError, 'proxy credentials must not contain CR/LF'
          end
          @generation = generation
          @authorization = +"Basic #{["#{@user}:#{@pass}"].pack('m0')}".freeze
          freeze
        end
      end

      def initialize(user = nil, pass = nil)
        @mutex = Mutex.new
        @current = nil
        @generation = 0
        publish(user, pass) unless user.nil?
      end

      # Validates +user+ and +pass+, then atomically publishes them as a new
      # snapshot and returns its generation identifier.  Generations are
      # strictly increasing, starting at 1.
      #
      # On validation failure, Net::HTTP::ProxyCredentialError is raised
      # and the previously published snapshot is retained.
      def publish(user, pass)
        validate!(user, pass)
        @mutex.synchronize do
          snapshot = Snapshot.new(user, pass, @generation + 1)
          @current = snapshot
          @generation = snapshot.generation
          snapshot.generation
        end
      end

      # Returns the currently published snapshot, or nil if none has been
      # published yet.
      def current
        @mutex.synchronize { @current }
      end

      # Returns the generation identifier of the most recently published
      # snapshot, or nil if none has been published yet.
      def published_generation
        @mutex.synchronize { @current&.generation }
      end

      # Returns diagnostic information about this store:
      #
      #   store.publish('user2', 'secret2') # => 2
      #   store.diagnostics                 # => {published_generation: 2}
      #
      # Only generation identifiers are reported; credentials are never
      # included.
      def diagnostics
        {published_generation: published_generation}
      end

      private

      def validate!(user, pass)
        unless user.is_a?(String)
          raise ProxyCredentialError,
                "proxy user must be a String, got #{user.class}"
        end
        if user.empty?
          raise ProxyCredentialError, 'proxy user must not be empty'
        end
        if user.include?(':')
          raise ProxyCredentialError, 'proxy user must not contain a colon'
        end
        unless pass.is_a?(String)
          raise ProxyCredentialError,
                "proxy password must be a String, got #{pass.class}"
        end
        if /[\r\n]/.match?(user) || /[\r\n]/.match?(pass)
          raise ProxyCredentialError, 'proxy credentials must not contain CR/LF'
        end
      end
    end
  end
end