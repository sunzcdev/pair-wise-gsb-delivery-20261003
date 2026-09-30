# frozen_string_literal: true

module Dry
  class Container
    # @api public
    Error = Class.new(StandardError)

    # Raised when an explicitly declared dependency is missing or when the
    # dependency chain of a target contains an eager cycle
    #
    # @api public
    DependencyError = Class.new(Error)

    KeyError = Class.new(::KeyError)
    if defined?(DidYouMean::KeyErrorChecker)
      DidYouMean.correct_error(KeyError, DidYouMean::KeyErrorChecker)
    end

    deprecate_constant(:Error)
  end
end
