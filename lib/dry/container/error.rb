# frozen_string_literal: true

module Dry
  class Container
    # @api public
    Error = Class.new(StandardError)

    KeyError = Class.new(::KeyError)
    if defined?(DidYouMean::KeyErrorChecker)
      DidYouMean.correct_error(KeyError, DidYouMean::KeyErrorChecker)
    end

    deprecate_constant(:Error)

    # Base error for lifecycle and scope related failures
    #
    # @api public
    class ScopeError < ::StandardError; end

    # Raised when a closed scope is referenced or reactivated
    #
    # @api public
    class ScopeClosedError < ScopeError; end

    # Raised when a longer-lived factory captures a shorter-lived dependency
    # (e.g. an application factory depending on a scoped object)
    #
    # @api public
    class ScopeLeakError < ScopeError; end
  end
end
