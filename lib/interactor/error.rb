module Interactor
  # Internal: Error raised during Interactor::Context failure. The error stores
  # a copy of the failed context for debugging purposes.
  class Failure < StandardError
    # Internal: Gets the Interactor::Context of the Interactor::Failure
    # instance.
    attr_reader :context

    # Internal: Initialize an Interactor::Failure.
    #
    # context - An Interactor::Context to be stored within the
    #           Interactor::Failure instance. (default: nil)
    #
    # Examples
    #
    #   Interactor::Failure.new
    #   # => #<Interactor::Failure: Interactor::Failure>
    #
    #   context = Interactor::Context.new(foo: "bar")
    #   # => #<Interactor::Context foo="bar">
    #   Interactor::Failure.new(context)
    #   # => #<Interactor::Failure: #<Interactor::Context foo="bar">>
    #
    #   raise Interactor::Failure, context
    #   # => Interactor::Failure: #<Interactor::Context foo="bar">
    def initialize(context = nil)
      @context = context
      super
    end
  end

  # Public: Error raised when one or more after_commit callbacks fail while
  # being run after a successful interactor invocation. Callback failures do
  # not roll back the interactor; instead all remaining callbacks are still
  # run and every callback error is collected into this aggregate error.
  class AfterCommitError < StandardError
    # Internal: Gets the Array of errors raised by individual after_commit
    # callbacks, in the order the callbacks were run.
    attr_reader :errors

    # Internal: Initialize an Interactor::AfterCommitError.
    #
    # errors - An Array of errors raised by after_commit callbacks.
    #           (default: [])
    #
    # Examples
    #
    #   Interactor::AfterCommitError.new
    #   # => #<Interactor::AfterCommitError: Interactor::AfterCommitError>
    #
    #   error = Interactor::AfterCommitError.new([RuntimeError.new("boom")])
    #   # => #<Interactor::AfterCommitError: 1 after_commit callback(s) failed: boom>
    #   error.errors
    #   # => [#<RuntimeError: boom>]
    #
    # Returns the Interactor::AfterCommitError.
    def initialize(errors = [])
      @errors = errors
      super("#{errors.size} after_commit callback(s) failed: #{errors.map(&:message).join(", ")}")
    end
  end
end
