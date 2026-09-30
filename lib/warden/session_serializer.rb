# encoding: utf-8
# frozen_string_literal: true
module Warden
  # Raised by an application supplied +deserialize+ (or +{scope}_deserialize+)
  # block when the backing identity store is temporarily unavailable.
  #
  # This must be distinguished from the block returning +nil+, which signals
  # that the identity genuinely no longer exists. A temporary outage must not
  # be treated as a deleted account, otherwise every session would be purged
  # and every user signed out.
  class IdentityStoreUnavailable < StandardError
    # @return [Object, nil] the session key that could not be restored
    attr_reader :key

    def initialize(message = nil, key: nil)
      @key = key
      super(message || "identity store is temporarily unavailable")
    end
  end

  # The tri-state outcome of attempting to restore a session for a scope.
  #
  # * +:restored+   - a user was successfully recovered (+#user+ is set, the
  #                   session value is kept untouched).
  # * +:missing+    - there was nothing stored, or the stored identity no
  #                   longer exists. In the latter case the session value has
  #                   been deleted.
  # * +:unavailable+- the identity store could not be reached. The original
  #                   session value is preserved so the session can be
  #                   restored later, and +#error+ carries the cause.
  class SessionRestoreResult
    STATUSES = [:restored, :missing, :unavailable].freeze

    # @return [Symbol] one of {STATUSES}
    attr_reader :status
    # @return [Object, nil] the recovered user, only set when restored
    attr_reader :user
    # @return [IdentityStoreUnavailable, nil] the cause when unavailable
    attr_reader :error

    def initialize(status, user: nil, error: nil)
      raise ArgumentError, "unknown status #{status.inspect}" unless STATUSES.include?(status)
      @status, @user, @error = status, user, error
    end

    # A user was successfully restored from the session.
    def restored?
      status == :restored
    end

    # There was no session or the identity no longer exists. The session
    # value has been deleted when there was one.
    def missing?
      status == :missing
    end

    # The identity store was temporarily unavailable. The original session
    # value was preserved and no other authentication strategy should be
    # attempted automatically.
    def unavailable?
      status == :unavailable
    end
  end

  class SessionSerializer
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

    # Attempts to restore the user for the given scope and reports an explicit
    # tri-state {SessionRestoreResult}.
    #
    # * when a user is recovered the session is left untouched and a
    #   +:restored+ result is returned;
    # * when there is no stored key, or +deserialize+ returns +nil+ (the
    #   identity no longer exists), the stored value is deleted and a
    #   +:missing+ result is returned;
    # * when +deserialize+ raises {IdentityStoreUnavailable} (a transient
    #   store failure), the original session value is preserved and an
    #   +:unavailable+ result is returned, so the same session can be
    #   recovered once the store comes back.
    def restore(scope)
      key = session[key_for(scope)]
      return SessionRestoreResult.new(:missing) unless key

      method_name = "#{scope}_deserialize"
      user = respond_to?(method_name) ? send(method_name, key) : deserialize(key)

      if user
        SessionRestoreResult.new(:restored, :user => user)
      else
        # A nil user means the identity genuinely no longer exists. Only in
        # this case is the session value safe to delete.
        delete(scope)
        SessionRestoreResult.new(:missing)
      end
    rescue IdentityStoreUnavailable => e
      # Keep the original session value: the account was not deleted, the
      # store just could not be reached.
      SessionRestoreResult.new(:unavailable, :error => e)
    end

    # Restores the user for the given scope, returning the user or +nil+.
    #
    # A temporarily unavailable identity store is treated like "no user for
    # this request" but, unlike a missing identity, the session value is
    # preserved. Callers that need to tell the two failure modes apart should
    # use {#restore} instead.
    def fetch(scope)
      result = restore(scope)
      result.restored? ? result.user : nil
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
