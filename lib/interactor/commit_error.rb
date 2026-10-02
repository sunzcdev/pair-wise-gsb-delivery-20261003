module Interactor
  # Internal: Raised when an after_commit callback is registered while no
  # interactor invocation is in progress.
  class CommitRegistrationError < StandardError
  end

  # Public: Aggregate result of draining the after_commit callbacks registered
  # during an interactor invocation. The result is inspectable even when a
  # callback raises: every registered callback is still run and every failure
  # is collected rather than aborting the remaining callbacks or triggering a
  # business rollback.
  class CommitResult
    # Public: Gets the Array of callbacks that were registered, in the order in
    # which they were registered.
    attr_reader :callbacks

    # Public: Gets the Array of callbacks that completed without raising.
    attr_reader :succeeded

    # Public: Gets the Array of [callback, exception] pairs for every callback
    # that raised.
    attr_reader :failures

    # Internal: Initialize an Interactor::CommitResult.
    #
    # callbacks - The Array of callbacks that were drained.
    # succeeded - The Array of callbacks that completed successfully.
    # failures  - The Array of [callback, exception] failure pairs.
    def initialize(callbacks: [], succeeded: [], failures: [])
      @callbacks = callbacks
      @succeeded = succeeded
      @failures = failures
    end

    # Public: Whether every registered callback completed without raising.
    #
    # Returns true if no callback raised or false otherwise.
    def success?
      failures.empty?
    end

    # Public: Whether any registered callback raised.
    #
    # Returns true if at least one callback raised or false otherwise.
    def failure?
      !success?
    end

    # Public: Gets the exceptions raised by failing callbacks.
    #
    # Returns an Array of exceptions.
    def exceptions
      failures.map(&:last)
    end
  end

  # Public: Raised by "call!" when one or more after_commit callbacks fail.
  # Business rollback is never triggered by a failing callback, but the
  # aggregated result is still surfaced through this error. The "call" method
  # rescues it and instead attaches the result to the returned context.
  class CommitError < StandardError
    # Public: Gets the Interactor::CommitResult summarizing the drain.
    attr_reader :result

    # Public: Gets the boundary Hash the drained callbacks belonged to.
    attr_reader :boundary

    # Internal: Initialize an Interactor::CommitError.
    #
    # result - The Interactor::CommitResult summarizing the drain.
    # boundary - The boundary Hash that was drained.
    def initialize(result, boundary = nil)
      @result = result
      @boundary = boundary
      super("#{result.failures.length} after_commit callback(s) failed")
    end
  end
end
