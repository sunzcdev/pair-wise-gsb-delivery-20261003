require "ostruct"

module Interactor
  # Public: The object for tracking state of an Interactor's invocation. The
  # context is used to initialize the interactor with the information required
  # for invocation. The interactor manipulates the context to produce the result
  # of invocation.
  #
  # The context is the mechanism by which success and failure are determined and
  # the context is responsible for tracking individual interactor invocations
  # for the purpose of rollback.
  #
  # The context may be manipulated using arbitrary getter and setter methods.
  #
  # Examples
  #
  #   context = Interactor::Context.new
  #   # => #<Interactor::Context>
  #   context.foo = "bar"
  #   # => "bar"
  #   context
  #   # => #<Interactor::Context foo="bar">
  #   context.hello = "world"
  #   # => "world"
  #   context
  #   # => #<Interactor::Context foo="bar" hello="world">
  #   context.foo = "baz"
  #   # => "baz"
  #   context
  #   # => #<Interactor::Context foo="baz" hello="world">
  class Context < OpenStruct
    # Internal: Initialize an Interactor::Context or preserve an existing one.
    # If the argument given is an Interactor::Context, the argument is returned.
    # Otherwise, a new Interactor::Context is initialized from the provided
    # hash.
    #
    # The "build" method is used during interactor initialization.
    #
    # context - A Hash whose key/value pairs are used in initializing a new
    #           Interactor::Context object. If an existing Interactor::Context
    #           is given, it is simply returned. (default: {})
    #
    # Examples
    #
    #   context = Interactor::Context.build(foo: "bar")
    #   # => #<Interactor::Context foo="bar">
    #   context.object_id
    #   # => 2170969340
    #   context = Interactor::Context.build(context)
    #   # => #<Interactor::Context foo="bar">
    #   context.object_id
    #   # => 2170969340
    #
    # Returns the Interactor::Context.
    def self.build(context = {})
      if self === context
        context
      else
        new(context)
      end
    end

    # Public: Whether the Interactor::Context is successful. By default, a new
    # context is successful and only changes when explicitly failed.
    #
    # The "success?" method is the inverse of the "failure?" method.
    #
    # Examples
    #
    #   context = Interactor::Context.new
    #   # => #<Interactor::Context>
    #   context.success?
    #   # => true
    #   context.fail!
    #   # => Interactor::Failure: #<Interactor::Context>
    #   context.success?
    #   # => false
    #
    # Returns true by default or false if failed.
    def success?
      !failure?
    end

    # Public: Whether the Interactor::Context has failed. By default, a new
    # context is successful and only changes when explicitly failed.
    #
    # The "failure?" method is the inverse of the "success?" method.
    #
    # Examples
    #
    #   context = Interactor::Context.new
    #   # => #<Interactor::Context>
    #   context.failure?
    #   # => false
    #   context.fail!
    #   # => Interactor::Failure: #<Interactor::Context>
    #   context.failure?
    #   # => true
    #
    # Returns false by default or true if failed.
    def failure?
      @failure || false
    end

    # Public: Fail the Interactor::Context. Failing a context raises an error
    # that may be rescued by the calling interactor. The context is also flagged
    # as having failed.
    #
    # Optionally the caller may provide a hash of key/value pairs to be merged
    # into the context before failure.
    #
    # context - A Hash whose key/value pairs are merged into the existing
    #           Interactor::Context instance. (default: {})
    #
    # Examples
    #
    #   context = Interactor::Context.new
    #   # => #<Interactor::Context>
    #   context.fail!
    #   # => Interactor::Failure: #<Interactor::Context>
    #   context.fail! rescue false
    #   # => false
    #   context.fail!(foo: "baz")
    #   # => Interactor::Failure: #<Interactor::Context foo="baz">
    #
    # Raises Interactor::Failure initialized with the Interactor::Context.
    def fail!(context = {})
      context.each { |key, value| self[key.to_sym] = value }
      @failure = true
      raise Failure, self
    end

    # Internal: Track that an Interactor has been called. The "called!" method
    # is used by the interactor being invoked with this context. After an
    # interactor is successfully called, the interactor instance is tracked in
    # the context for the purpose of potential future rollback.
    #
    # interactor - An Interactor instance that has been successfully called.
    #
    # Returns nothing.
    def called!(interactor)
      _called << interactor
    end

    # Public: Roll back the Interactor::Context. Any interactors to which this
    # context has been passed and which have been successfully called are asked
    # to roll themselves back by invoking their "rollback" instance methods.
    #
    # Examples
    #
    #   context = MyInteractor.call(foo: "bar")
    #   # => #<Interactor::Context foo="baz">
    #   context.rollback!
    #   # => true
    #   context
    #   # => #<Interactor::Context foo="bar">
    #
    # Returns true if rolled back successfully or false if already rolled back.
    def rollback!
      return false if @rolled_back
      _called.reverse_each(&:rollback)
      @rolled_back = true
    end

    # Public: Register a callback to run only after the outermost interactor
    # invocation that owns this Context has completed successfully. Callbacks
    # run in the order in which they were registered.
    #
    # If the invocation fails or is rolled back, every registered callback is
    # discarded and never run. A fresh business invocation started from within
    # a callback gets its own boundary, so callbacks it registers are never
    # appended back to the batch currently being drained.
    #
    # callback - An object responding to "call".
    #
    # Returns the callback.
    # Raises Interactor::CommitRegistrationError if no interactor invocation
    #   is in progress.
    def after_commit(callback = nil, &block)
      callback ||= block
      boundary = _commit_boundary!
      boundary[:callbacks] << callback
      callback
    end

    # Internal: Open an after_commit boundary for an interactor invocation. If
    # a boundary is already open for this Context (a nested organizer or an
    # interactor called with the same context), the invocation joins it and
    # returns the existing boundary. A fresh invocation started while the
    # previous boundary is being drained opens a new, independent boundary.
    #
    # Returns an invocation frame Hash identifying the boundary and whether
    # this invocation owns it.
    def _begin_commit_boundary
      last_frame = _commit_stack.last
      if last_frame.nil? || last_frame[:boundary][:phase] == :commit
        boundary = {callbacks: [], phase: :business}
        owner = true
      else
        boundary = last_frame[:boundary]
        owner = false
      end

      frame = {boundary: boundary, owner: owner}
      _commit_stack.push(frame)
      frame
    end

    # Internal: Whether this invocation opened the given boundary rather than
    # joining one from an enclosing invocation. Only the owner drains or
    # discards the boundary.
    def _commit_boundary_owner?(frame)
      frame[:owner]
    end

    # Internal: Remove an invocation frame from the stack after it has
    # completed.
    def _end_commit_frame!(frame)
      _commit_stack.delete(frame)
    end

    # Internal: Discard every registered callback after a failed or rolled
    # back invocation.
    #
    # Returns nothing.
    def _discard_commit_boundary!(frame)
      boundary = frame[:boundary]
      boundary[:callbacks].clear
      boundary[:phase] = :discarded
    end

    # Internal: Run every registered callback in registration order. Callback
    # failures are collected rather than raised, so the remaining callbacks
    # always run. A fresh invocation started during the drain gets its own
    # boundary instead of mutating the one being drained.
    #
    # Returns the Interactor::CommitResult.
    # Raises Interactor::CommitError if any callback failed.
    def _commit!(frame)
      boundary = frame[:boundary]
      callbacks = boundary[:callbacks].dup
      boundary[:phase] = :commit
      succeeded = []
      failures = []

      callbacks.each do |callback|
        begin
          callback.call
          succeeded << callback
        rescue => error
          failures << [callback, error]
        end
      end

      boundary[:phase] = :committed

      result = CommitResult.new(
        callbacks: callbacks,
        succeeded: succeeded,
        failures: failures
      )
      raise CommitError.new(result, boundary) if result.failure?
      result
    end

    # Internal: The boundary a callback registered from business code belongs
    # to.
    #
    # Returns the boundary Hash.
    # Raises Interactor::CommitRegistrationError if registration is attempted
    #   outside of an interactor invocation or while a boundary is draining.
    def _commit_boundary!
      boundary = _commit_stack.last&.fetch(:boundary, nil)
      if boundary.nil? || boundary[:phase] != :business
        raise CommitRegistrationError,
          "after_commit can only be registered during an interactor invocation"
      end
      boundary
    end

    # Internal: The stack of open after_commit boundaries.
    #
    # Returns an Array of boundary Hashes.
    def _commit_stack
      @commit_stack ||= []
    end

    # Internal: An Array of successfully called Interactor instances invoked
    # against this Interactor::Context instance.
    #
    # Examples
    #
    #   context = Interactor::Context.new
    #   # => #<Interactor::Context>
    #   context._called
    #   # => []
    #
    #   context = MyInteractor.call(foo: "bar")
    #   # => #<Interactor::Context foo="baz">
    #   context._called
    #   # => [#<MyInteractor @context=#<Interactor::Context foo="baz">>]
    #
    # Returns an Array of Interactor instances or an empty Array.
    def _called
      @called ||= []
    end

    # Internal: Support for ruby 3.0 pattern matching
    #
    # Examples
    #
    #   context = MyInteractor.call(foo: "bar")
    #
    #   # => #<Interactor::Context foo="bar">
    #   context => { foo: }
    #   foo == "bar"
    #   # => true
    #
    #
    #   case context
    #   in success: true, result: { first:, second: }
    #     do_stuff(first, second)
    #   in failure: true, error_message:
    #     log_error(message: error_message)
    #   end
    #
    # Returns the context as a hash, including success and failure
    def deconstruct_keys(keys)
      to_h.merge(
        success: success?,
        failure: failure?
      )
    end
  end
end
