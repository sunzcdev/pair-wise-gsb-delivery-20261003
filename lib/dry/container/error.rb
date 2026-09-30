# frozen_string_literal: true

module Dry
  class Container
    # @api public
    Error = Class.new(StandardError)

    KeyError = Class.new(::KeyError)
    if defined?(DidYouMean::KeyErrorChecker)
      DidYouMean.correct_error(KeyError, DidYouMean::KeyErrorChecker)
    end

    # Raised when a dependency declared by a registered item is not registered
    MissingDependencyError = Class.new(Error)

    # Raised when non-lazy dependencies form a cycle
    CircularDependencyError = Class.new(Error)

    deprecate_constant(:Error)
  end
end
