# frozen_string_literal: true

module Pundit
  # A policy query may return a {Decision} instead of a bare boolean in order
  # to communicate a reason code and additional details to the caller.
  #
  # Only {#allowed?} is consulted when authorizing: a denied decision is still
  # a truthy Ruby object, but Pundit never relies on that truthiness.
  #
  # @example
  #   def update?
  #     return Pundit::Decision.deny(code: :not_owner, details: { user_id: user.id }) unless post.user == user
  #
  #     Pundit::Decision.allow
  #   end
  #
  # @see .allow
  # @see .deny
  # @see .allowed?
  # @since v2.6.0
  class Decision
    # @param allowed [Boolean] whether the action is allowed
    # @param code [Object, nil] optional machine-readable reason code
    # @param details [Object, nil] optional data intended for the caller
    def initialize(allowed, code: nil, details: nil)
      @allowed = allowed
      @code = code
      @details = details
    end

    # @!attribute [r] allowed
    #   @return [Object] the raw value passed as the allowed flag
    # @!attribute [r] code
    #   @return [Object, nil] the machine-readable reason code
    # @!attribute [r] details
    #   @return [Object, nil] additional data intended for the caller
    attr_reader :allowed, :code, :details

    # @return [Boolean] whether the action is allowed
    def allowed?
      !!allowed
    end

    # @param code [Object, nil] optional machine-readable reason code
    # @param details [Object, nil] optional data intended for the caller
    # @return [Decision] a decision that allows the action
    def self.allow(code: nil, details: nil)
      new(true, code: code, details: details)
    end

    # @param code [Object, nil] optional machine-readable reason code
    # @param details [Object, nil] optional data intended for the caller
    # @return [Decision] a decision that denies the action
    def self.deny(code: nil, details: nil)
      new(false, code: code, details: details)
    end

    # Evaluates the result of a policy query without relying on the result's
    # truthiness.
    #
    # @param result [Decision, Object] either a {Decision} or a plain
    #   boolean-ish policy result
    # @return [Boolean] whether the action is allowed
    def self.allowed?(result)
      result.is_a?(Decision) ? result.allowed? : !!result
    end
  end
end
