# encoding: utf-8
# frozen_string_literal: true

module Warden
  # Authentication evidence records *how* and *when* a scope was last proven
  # to be who it claims to be. Being logged in (a serialized user in the
  # session) is not enough on its own: sensitive actions may additionally
  # require that the user re-authenticated recently, and/or through a
  # particular (stronger) method.
  #
  # Evidence is stored in the session, one entry per scope, so it survives
  # across requests. Simply restoring the user from the session never renews
  # the recorded time; only a successful authentication strategy does.
  #
  # @api public
  class AuthEvidence
    # Session key holding the hash of evidence, keyed by scope.
    # @api private
    SESSION_KEY = "warden.auth.evidence".freeze

    # @return [Symbol] the name of the winning authentication strategy
    attr_reader :method

    # @return [Float] the time (seconds since the epoch) authentication completed
    attr_reader :at

    # Builds evidence from the winning strategy's name and the given clock.
    #
    # @param method [Symbol, String] name of the successful strategy
    # @param clock [#now] object responding to +now+, defaults to +Time+
    # @return [Warden::AuthEvidence]
    # @api public
    def self.record(method, clock = Time)
      new(method, clock.now.to_f)
    end

    # Restores evidence from a serialized session value. Accepts either an
    # already-built instance or a hash (string or symbol keys).
    #
    # @param value [Warden::AuthEvidence, Hash, nil]
    # @return [Warden::AuthEvidence, nil]
    # @api private
    def self.from_session(value)
      return nil if value.nil?
      return value if value.is_a?(self)

      method = value[:method] || value["method"]
      at     = value[:at]     || value["at"]
      return nil if method.nil? || at.nil?

      new(method, at.to_f)
    end

    # @param method [Symbol, String] name of the successful strategy
    # @param at [Numeric] completion time in seconds since the epoch
    # @api public
    def initialize(method, at)
      @method = method.to_sym
      @at = at.to_f
    end

    # Whether this evidence satisfies the given requirements.
    #
    # With no requirements at all any evidence (or none) is sufficient; the
    # caller is responsible for first confirming the scope is logged in.
    #
    # @param methods [Array<Symbol>, nil] acceptable strategy names; +nil+
    #   means the method is not constrained
    # @param max_age [Numeric, nil] maximum acceptable age in seconds;
    #   the exact boundary is still accepted
    # @param clock [#now] object responding to +now+
    # @return [Boolean]
    # @api public
    def satisfies?(methods: nil, max_age: nil, clock: Time)
      return false if at.nil?
      return false if methods && !Array(methods).map(&:to_sym).include?(method)
      return false if max_age && (clock.now.to_f - at) > max_age.to_f
      true
    end

    # @return [Hash] a session-safe representation
    # @api private
    def to_session
      { "method" => method.to_s, "at" => at }
    end
  end
end
