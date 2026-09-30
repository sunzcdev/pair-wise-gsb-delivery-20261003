# frozen_string_literal: true

module Dry
  class Container
    # @api public
    Error = Class.new(StandardError)

    KeyError = Class.new(::KeyError)
    if defined?(DidYouMean::KeyErrorChecker)
      DidYouMean.correct_error(KeyError, DidYouMean::KeyErrorChecker)
    end

    # Raised while constructing an application-level item that captures a
    # scope-level dependency. The message carries the full construction chain
    # so the offending dependency can be fixed at registration/construction
    # time rather than surfacing as a wrong object later.
    #
    # @api public
    class CaptiveDependencyError < Error; end

    # Raised when resolving through a scope that has already been closed. A
    # closed scope can never be reactivated.
    #
    # @api public
    class ScopeClosedError < Error; end

    # Raised when a scope-level item is resolved without an active scope.
    #
    # @api public
    class NoActiveScopeError < Error; end

    deprecate_constant(:Error)
  end
end
