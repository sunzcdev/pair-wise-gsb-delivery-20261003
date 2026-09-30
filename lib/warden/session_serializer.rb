# encoding: utf-8
# frozen_string_literal: true
module Warden
  class SessionSerializer
    # The outcome of trying to restore an identity from the session.
    #
    # Three distinct situations are represented so callers never have to
    # confuse a deleted identity with an identity they could not check:
    #
    #   :restored    - the user was successfully reconstituted
    #   :not_found   - the identity is definitively gone (deserialize returned nil)
    #   :unavailable - the identity store could not answer, the status is unknown
    class RestoreResult
      STATUSES = [:restored, :not_found, :unavailable].freeze

      attr_reader :status, :user, :error

      def initialize(status, user = nil, error = nil)
        raise ArgumentError, "unknown restore status #{status.inspect}" unless STATUSES.include?(status)
        @status, @user, @error = status, user, error
      end

      def restored?
        status == :restored
      end

      def not_found?
        status == :not_found
      end

      def unavailable?
        status == :unavailable
      end
    end

    attr_reader :env

    def initialize(env)
      @env = env
    end

    def key_for(scope)
      "warden.user.#{scope}.key"
    end

    def serialize(user)
      user
    end

    def deserialize(key)
      key
    end

    def store(user, scope)
      return unless user
      method_name = "#{scope}_serialize"
      specialized = respond_to?(method_name)
      session[key_for(scope)] = specialized ? send(method_name, user) : serialize(user)
    end

    # Restores the user stored for +scope+ and reports exactly what happened.
    #
    # Unlike +fetch+, this method distinguishes three outcomes:
    #
    # * a successful restoration (+restored?+),
    # * an identity that no longer exists (+not_found?+, the session entry is
    #   removed just like +fetch+ used to do),
    # * an identity store that could not be consulted (+unavailable+). In this
    #   last case the raw session value is left untouched so the session can be
    #   retried later; the serializer signals the situation by raising
    #   Warden::IdentityStoreTemporarilyUnavailable from +deserialize+.
    def restore(scope)
      key = session[key_for(scope)]
      return RestoreResult.new(:not_found) unless key

      method_name = "#{scope}_deserialize"
      user = respond_to?(method_name) ? send(method_name, key) : deserialize(key)

      if user
        RestoreResult.new(:restored, user)
      else
        delete(scope)
        RestoreResult.new(:not_found)
      end
    rescue Warden::IdentityStoreTemporarilyUnavailable => e
      # The session value MUST be preserved: we do not know whether the
      # identity exists or not.
      RestoreResult.new(:unavailable, nil, e)
    end

    # Backwards-compatible wrapper around +restore+.
    #
    # Returns the restored user or nil when there is no session entry or the
    # identity no longer exists. When the identity store is temporarily
    # unavailable the Warden::IdentityStoreTemporarilyUnavailable error is
    # re-raised instead of pretending the identity was deleted.
    def fetch(scope)
      result = restore(scope)
      raise result.error if result.unavailable?
      result.user
    end

    def stored?(scope)
      !!session[key_for(scope)]
    end

    def delete(scope, user=nil)
      session.delete(key_for(scope))
    end

    # We can't cache this result because the session can be lazy loaded
    def session
      env["rack.session"] || {}
    end
  end # SessionSerializer
end # Warden
