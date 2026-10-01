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

    # Public: Invoke an Interactor as a resumable sub-flow. The interactor is
    # invoked against this same context, but a rollback boundary is recorded
    # first so that a business failure raised by the sub-flow does not undo
    # work completed before entering it.
    #
    # If the sub-flow succeeds, any instances it ran and any keys it wrote to
    # the context become part of the outer flow. Should the outer flow fail
    # later, those instances still take part in the normal reverse-order
    # rollback.
    #
    # If the sub-flow calls "fail!", only the instances completed after the
    # boundary are rolled back, in reverse order. The sub-flow's additions,
    # removals, and overwrites of top-level context keys are then discarded,
    # restoring the keys to their values at the boundary, and the context is
    # marked successful again. When a block is given, it is called with a
    # snapshot of the failed context and may use it to record the reason or
    # otherwise recover; the outer flow then continues. Without a block the
    # failure is contained and the outer flow simply continues.
    #
    # Only failures raised by "fail!" against this context are caught. Any
    # other exception propagates normally. Such an exception still rolls back,
    # in reverse order, the instances completed after entering the sub-flow as
    # it unwinds, while outer instances are left for the outermost rollback.
    #
    # Restoring keys is shallow. Values are not deep-copied, and mutations made
    # inside objects referenced by the context are not undone.
    #
    # interactor - An Interactor class invoked with "call!" against this
    #              context.
    #
    # Yields a Context holding a shallow snapshot of the failed sub-flow's
    #   context, flagged as a failure, if the sub-flow fails and a block is
    #   given.
    #
    # Examples
    #
    #   context.attempt(SendReceipt) do |failure|
    #     context.notice = failure.error
    #   end
    #
    # Returns the Context.
    def attempt(interactor)
      @attempt_depth = _attempt_depth + 1
      depth = @attempt_depth
      snapshot = _snapshot_keys
      begin
        interactor.call!(self)
        # The whole sub-flow succeeded. Its surviving instances join the
        # enclosing flow so they take part in its normal reverse-order
        # rollback should it fail later.
        _called.each do |instance|
          _depth[instance.object_id] = depth - 1 if
            _depth[instance.object_id] == depth
        end
      rescue Failure => error
        raise unless error.context.equal?(self)

        failed = Context.new(to_h)
        failed._fail!
        _rollback_depth!(_attempt_depth)
        _restore_keys(snapshot)
        @failure = false
        yield failed if block_given?
      ensure
        @attempt_depth -= 1
      end
      self
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
      _depth[interactor.object_id] = _attempt_depth
    end

    # Internal: Mark the context as having failed without raising. Used to
    # build the failure snapshot handed to an "attempt" recovery block.
    #
    # Returns nothing.
    def _fail!
      @failure = true
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
      _rollback_all!
      @rolled_back = true
    end

    # Internal: Whether a resumable sub-flow is currently active. While this is
    # positive, a "run!" rescue does not roll back: a failure that belongs to
    # a sub-flow is rolled back once by that sub-flow's "attempt", and any
    # other exception is rolled back once by the outermost unwind.
    def _attempt_depth
      @attempt_depth ||= 0
    end

    # Internal: Roll back every tracked instance in reverse order as part of
    # the outermost unwind, marking the context as rolled back so repeated or
    # nested rollbacks stay idle, matching the public "rollback!" contract.
    #
    # Returns nothing.
    def _rollback_all!
      return if @rolled_back
      _rollback_depth!(0)
      @rolled_back = true
    end

    # Internal: Roll back, in reverse order, every tracked instance completed
    # at or below the given resumable sub-flow depth, removing each from the
    # tracking list as it is rolled back so that an instance is rolled back at
    # most once. A depth of zero rolls back the entire flow.
    #
    # Returns nothing.
    def _rollback_depth!(depth)
      instances = _called
      until instances.empty?
        index = instances.rindex { |instance|
          _depth_for(instance) >= depth
        }
        break unless index
        instance = instances.delete_at(index)
        _depth.delete(instance.object_id)
        instance.rollback
      end
    end

    # Internal: Depth of a tracked instance. Instances not registered through
    # "called!" (test doubles pushed directly onto "_called") default to the
    # outer-flow depth of zero.
    def _depth_for(instance)
      _depth[instance.object_id] || 0
    end

    # Internal: Maps a tracked instance's object id to the resumable sub-flow
    # depth at which it completed. Depth zero means the outer flow.
    def _depth
      @depth ||= {}
    end

    # Internal: Capture the current top-level keys and their values for later
    # restoration. Values are captured by reference; this is intentionally not
    # a deep copy.
    def _snapshot_keys
      to_h.transform_values(&:itself)
    end

    # Internal: Discard top-level key changes made since the snapshot: keys
    # added are removed, keys removed are restored, and overwritten keys revert
    # to their previous values.
    def _restore_keys(snapshot)
      (to_h.keys - snapshot.keys).each { |key| delete_field(key) }
      snapshot.each { |key, value| self[key] = value }
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
