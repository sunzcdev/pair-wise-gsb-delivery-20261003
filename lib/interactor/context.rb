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
      boundary = _subflow_boundaries.last

      if failure? && boundary
        return false if boundary[:rolled_back]
        boundary[:rolled_back] = true
        before = _top_level_keys
        _called[boundary[:index]..].reverse_each(&:rollback)
        after = _top_level_keys
        after.each do |key, value|
          boundary[:rollback_keys][key] = value unless before.key?(key)
        end
      else
        _called.reverse_each(&:rollback)
        @rolled_back = true
      end
    end

    # Internal: Push a recoverable subflow boundary. The boundary records where
    # the subflow's completed steps begin and what the Context's top-level keys
    # looked like beforehand, so an explicit business failure can be scoped to
    # just this subflow.
    #
    # Returns nothing.
    def _enter_subflow
      _subflow_boundaries << {
        index: _called.length,
        keys: _top_level_keys,
        rollback_keys: {}
      }
    end

    # Internal: Commit the innermost subflow boundary after a successful
    # subflow. The subflow's completed steps stay tracked and its Context
    # changes remain in place.
    #
    # Returns nothing.
    def _commit_subflow
      _subflow_boundaries.pop
    end

    # Internal: Abort the innermost subflow after an explicit business failure.
    # The steps completed inside the subflow are rolled back (if they have not
    # been already) and discarded, the Context's top-level keys are restored to
    # their pre-subflow state, and the Context is marked successful again.
    #
    # Returns a failed Context describing the failure for the recovery block.
    def _abort_subflow
      boundary = _subflow_boundaries.pop
      failure = _failure_snapshot

      unless boundary[:rolled_back]
        _called[boundary[:index]..].reverse_each(&:rollback)
      end
      @called = _called.first(boundary[:index])

      _restore_keys(boundary[:keys])
      boundary[:rollback_keys].each do |key, value|
        self[key] = value
      end
      @failure = false

      failure
    end

    # Internal: Pop the innermost subflow boundary after an ordinary exception.
    # The exception is allowed to propagate and the whole Context has already
    # been rolled back, so neither the completed steps nor the keys are kept.
    #
    # Returns nothing.
    def _dissolve_subflow
      _subflow_boundaries.pop
    end

    # Internal: Mark the Context as failed without raising Interactor::Failure.
    # Used to build the isolated failure Context handed to a recovery block.
    #
    # Returns nothing.
    def _mark_failed
      @failure = true
    end

    # Internal: A shallow snapshot of the Context's current top-level keys and
    # values. Values are shared rather than deep copied.
    #
    # Returns a Hash.
    def _top_level_keys
      to_h
    end

    # Internal: Build a failed Context that captures the top-level state at the
    # moment of failure. The state is copied shallowly; values are shared.
    #
    # Returns an Interactor::Context.
    def _failure_snapshot
      Context.new(to_h).tap(&:_mark_failed)
    end

    # Internal: Restore the Context's top-level keys to the state captured by a
    # boundary. Keys added by the subflow are removed, keys removed by the
    # subflow are restored, and every remaining key is reset to its old value.
    # Mutations inside the values themselves are not reverted.
    #
    # before - The Hash of top-level keys captured when the subflow began.
    #
    # Returns nothing.
    def _restore_keys(before)
      after = _top_level_keys

      after.each_key { |key| delete_field(key) unless before.key?(key) }
      before.each { |key, value| self[key] = value }
    end

    # Internal: The stack of active recoverable subflow boundaries.
    #
    # Returns an Array of boundary Hashes.
    def _subflow_boundaries
      @subflow_boundaries ||= []
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
