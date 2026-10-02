# frozen_string_literal: true

module Pundit
  # An explicit authorization decision that policy query methods may return
  # instead of a boolean.
  #
  # A decision carries an allowed state plus an optional machine-readable
  # `code` and free-form `details` explaining the outcome to the caller.
  # Pundit only looks at the allowed state; `code` and `details` are never
  # interpolated into error messages, so they will not leak into logs unless
  # you log them yourself.
  #
  # @example
  #   def update?
  #     return Pundit::Decision.allow if record.user == user
  #
  #     Pundit::Decision.deny(code: :not_owner, details: {owner_id: record.user_id})
  #   end
  #
  # @since v2.6.0
  class Decision
    class << self
      # @param code [Symbol, String, nil] machine-readable reason code
      # @param details [Object, nil] additional details for the caller
      # @return [Decision] an allowed decision
      # @since v2.6.0
      def allow(code: nil, details: nil)
        new(allowed: true, code: code, details: details)
      end

      # @param code [Symbol, String, nil] machine-readable reason code
      # @param details [Object, nil] additional details for the caller
      # @return [Decision] a denied decision
      # @since v2.6.0
      def deny(code: nil, details: nil)
        new(allowed: false, code: code, details: details)
      end
    end

    # @param allowed [Boolean] whether the action is allowed
    # @param code [Symbol, String, nil] machine-readable reason code
    # @param details [Object, nil] additional details for the caller
    # @since v2.6.0
    def initialize(allowed:, code: nil, details: nil)
      @allowed = allowed ? true : false
      @code = code
      @details = details
      freeze
    end

    # @see #initialize
    # @since v2.6.0
    attr_reader :code

    # @see #initialize
    # @since v2.6.0
    attr_reader :details

    # @return [Boolean] whether the action is allowed
    # @since v2.6.0
    def allowed?
      @allowed
    end

    # @return [Boolean] whether the action is denied
    # @since v2.6.0
    def denied?
      !@allowed
    end

    # @api private
    def ==(other)
      other.is_a?(Decision) &&
        allowed? == other.allowed? &&
        code == other.code &&
        details == other.details
    end
    alias_method :eql?, :==

    # @api private
    def hash
      [self.class, @allowed, @code, @details].hash
    end
  end
end
