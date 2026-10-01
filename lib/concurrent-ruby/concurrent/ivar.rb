require 'concurrent/constants'
require 'concurrent/errors'
require 'concurrent/concern/logging'
require 'concurrent/concern/obligation'
require 'concurrent/concern/observable'
require 'concurrent/collection/subscription_observer_set'
require 'concurrent/executor/safe_task_executor'
require 'concurrent/synchronization/lockable_object'

module Concurrent

  # An `IVar` is like a future that you can assign. As a future is a value that
  # is being computed that you can wait on, an `IVar` is a value that is waiting
  # to be assigned, that you can wait on. `IVars` are single assignment and
  # deterministic.
  #
  # Then, express futures as an asynchronous computation that assigns an `IVar`.
  # The `IVar` becomes the primitive on which [futures](Future) and
  # [dataflow](Dataflow) are built.
  #
  # An `IVar` is a single-element container that is normally created empty, and
  # can only be set once. The I in `IVar` stands for immutable. Reading an
  # `IVar` normally blocks until it is set. It is safe to set and read an `IVar`
  # from different threads.
  #
  # If you want to have some parallel task set the value in an `IVar`, you want
  # a `Future`. If you want to create a graph of parallel tasks all executed
  # when the values they depend on are ready you want `dataflow`. `IVar` is
  # generally a low-level primitive.
  #
  # ## Examples
  #
  # Create, set and get an `IVar`
  #
  # ```ruby
  # ivar = Concurrent::IVar.new
  # ivar.set 14
  # ivar.value #=> 14
  # ivar.set 2 # would now be an error
  # ```
  #
  # ## See Also
  #
  # 1. For the theory: Arvind, R. Nikhil, and K. Pingali.
  #    [I-Structures: Data structures for parallel computing](http://dl.acm.org/citation.cfm?id=69562).
  #    In Proceedings of Workshop on Graph Reduction, 1986.
  # 2. For recent application:
  #    [DataDrivenFuture in Habanero Java from Rice](http://www.cs.rice.edu/~vs3/hjlib/doc/edu/rice/hj/api/HjDataDrivenFuture.html).
  class IVar < Synchronization::LockableObject
    include Concern::Obligation
    include Concern::Observable

    # Handle returned by {IVar#subscribe}. It represents a single
    # observation of an {IVar}'s completion and is the only way to withdraw
    # a notification that has not been delivered yet.
    #
    # A subscription goes through the following atomic states:
    #
    #   :waiting  -> :delivering -> :delivered   (it was notified)
    #   :waiting  -> :cancelled                 (it was cancelled in time)
    #
    # Once a subscription has left `:waiting` its fate is fixed: cancellation
    # can no longer prevent (or undo) the notification. All state transitions
    # are serialized per subscription, so completion and cancellation have a
    # single consistent outcome even when they happen concurrently.
    #
    # Neither the subscription nor its owning set keeps a strong reference to
    # the observer after the subscription reaches a terminal state. This
    # prevents an observer (for example a finished web request) from being
    # retained until some unrelated background task finally completes.
    class Subscription < Synchronization::LockableObject
      include Concern::Logging

      # @!visibility private
      def initialize(observer_set, observer, func)
        super(&nil)
        @observer_set = observer_set
        @observer     = observer
        @func         = func
        @state        = :waiting
      end

      # The registered observer (or block). Reset to `nil` once the
      # subscription reaches a terminal state so it can be garbage collected.
      #
      # @!visibility private
      def observer
        synchronize { @observer }
      end

      # Withdraw the observation if its callback has not started yet.
      #
      # This is safe to call multiple times and from any thread, including
      # from inside the subscription's own callback or another observer's
      # callback.
      #
      # @return [Boolean] `true` when the call prevented the notification
      #   (i.e. it was made before the callback started), `false` when the
      #   notification had already started or completed, or the subscription
      #   had already been cancelled by an earlier call. The return value
      #   therefore answers exactly: "did this call win the race against the
      #   callback?"
      def cancel
        won = synchronize do
          if @state == :waiting
            @state = :cancelled
            true
          else
            false
          end
        end

        detach if won
        won
      end

      # Whether {#cancel} (ever) prevented the notification.
      #
      # @return [Boolean]
      def cancelled?
        synchronize { @state == :cancelled }
      end

      # Whether the callback has started running. A subscription whose
      # callback is currently executing also reports `true`.
      #
      # @return [Boolean]
      def notified?
        synchronize { @state == :delivering || @state == :delivered }
      end

      # Deliver the completion notification unless the subscription was
      # cancelled. The waiting -> delivering transition is atomic, so exactly
      # one of {#cancel} and this method wins. Called outside of the observer
      # set's lock and outside the `IVar`'s lock.
      #
      # @!visibility private
      # @return [Boolean] whether the observer was notified
      def deliver(time, value, reason)
        observer = nil
        func = nil
        claimed = synchronize do
          if @state == :waiting
            @state = :delivering
            observer = @observer
            func = @func
            true
          else
            false
          end
        end

        return false unless claimed

        begin
          observer.send(func, time, value, reason)
        rescue => ex
          # An observer raising must never prevent other observers from
          # running or bubble out of the completion path. The exception is
          # logged and then dropped; explicitly clear its backtrace (which can
          # hold the frame binding and therefore the observer and anything it
          # closed over) so logging does not accidentally extend the
          # observer's lifetime.
          ex.set_backtrace([])
          log(Concern::Logging::ERROR, self.class.name, ex)
        ensure
          synchronize { @state = :delivered }
          detach
        end
        true
      end

      private

      # Remove the subscription from the live observer set (if it is still
      # registered there) and drop the strong reference to the observer.
      def detach(observer_set = @observer_set)
        observer_set.discard(self) if observer_set
        synchronize do
          @observer = nil
          @func     = nil
        end
        self
      end
    end

    # Create a new `IVar` in the `:pending` state with the (optional) initial value.
    #
    # @param [Object] value the initial value
    # @param [Hash] opts the options to create a message with
    # @option opts [String] :dup_on_deref (false) call `#dup` before returning
    #   the data
    # @option opts [String] :freeze_on_deref (false) call `#freeze` before
    #   returning the data
    # @option opts [String] :copy_on_deref (nil) call the given `Proc` passing
    #   the internal value and returning the value returned from the proc
    def initialize(value = NULL, opts = {}, &block)
      if value != NULL && block_given?
        raise ArgumentError.new('provide only a value or a block')
      end
      super(&nil)
      synchronize { ns_initialize(value, opts, &block) }
    end

    # Add an observer on this object that will receive notification on update.
    #
    # Upon completion the `IVar` will notify all observers in a thread-safe way.
    # The `func` method of the observer will be called with three arguments: the
    # `Time` at which the `Future` completed the asynchronous operation, the
    # final `value` (or `nil` on rejection), and the final `reason` (or `nil` on
    # fulfillment).
    #
    # If the `IVar` has already completed the observer is notified immediately,
    # synchronously, by the calling thread (and outside of any internal lock).
    #
    # @param [Object] observer the object that will be notified of changes
    # @param [Symbol] func symbol naming the method to call when this
    #   `Observable` has changes`
    # @return [Object] the observer
    def add_observer(observer = nil, func = :update, &block)
      raise ArgumentError.new('cannot provide both an observer and a block') if observer && block

      if block
        observer = block
        func = :call
      end

      register_or_deliver(observer, func)
      observer
    end

    # Observe completion of the `IVar`, returning a cancellable
    # {Subscription} handle.
    #
    # When the `IVar` is still pending the returned subscription is stored and
    # its callback will run once, when the `IVar` completes. The subscription
    # can be cancelled with {Subscription#cancel} up until the moment its
    # callback starts.
    #
    # When the `IVar` has already completed the subscription is *not* stored:
    # the callback is delivered immediately and synchronously by the calling
    # thread (outside of any internal lock), and the returned handle already
    # reports the notification as delivered. Subscribing again after that
    # simply delivers again, once per call.
    #
    # @param [Object] observer the object whose `func` method is called on
    #   completion; usually a block is passed instead
    # @param [Symbol] func symbol naming the method to call on completion,
    #   defaults to `:update` (or `:call` when a block is given)
    # @yield [Time, Object, Exception] block run on completion with the
    #   completion time, final value (nil on rejection) and final reason
    #   (nil on fulfillment)
    # @return [Subscription] the subscription handle
    #
    # @example give up waiting for a result that is no longer needed
    #   subscription = ivar.subscribe { |time, value, reason| request.finish(value) }
    #   # later, e.g. when the request is abandoned:
    #   if subscription.cancel
    #     # the callback will never run
    #   else
    #     # it had already started, it cannot be recalled
    #   end
    def subscribe(observer = nil, func = :update, &block)
      raise ArgumentError.new('cannot provide both an observer and a block') if observer && block
      raise ArgumentError.new('must provide an observer or a block') if observer.nil? && block.nil?

      if block
        observer = block
        func = :call
      end

      register_or_deliver(observer, func)
    end

    # @!macro ivar_set_method
    #   Set the `IVar` to a value and wake or notify all threads waiting on it.
    #
    #   @!macro ivar_set_parameters_and_exceptions
    #     @param [Object] value the value to store in the `IVar`
    #     @yield A block operation to use for setting the value
    #     @raise [ArgumentError] if both a value and a block are given
    #     @raise [Concurrent::MultipleAssignmentError] if the `IVar` has already
    #       been set or otherwise completed
    #
    #   @return [IVar] self
    def set(value = NULL)
      check_for_block_or_value!(block_given?, value)
      raise MultipleAssignmentError unless compare_and_set_state(:processing, :pending)

      begin
        value = yield if block_given?
        complete_without_notification(true, value, nil)
      rescue => ex
        complete_without_notification(false, nil, ex)
      end

      notify_observers(self.value, reason)
      self
    end

    # @!macro ivar_fail_method
    #   Set the `IVar` to failed due to some error and wake or notify all threads waiting on it.
    #
    #   @param [Object] reason for the failure
    #   @raise [Concurrent::MultipleAssignmentError] if the `IVar` has already
    #     been set or otherwise completed
    #   @return [IVar] self
    def fail(reason = StandardError.new)
      complete(false, nil, reason)
    end

    # Attempt to set the `IVar` with the given value or block. Return a
    # boolean indicating the success or failure of the set operation.
    #
    # @!macro ivar_set_parameters_and_exceptions
    #
    # @return [Boolean] true if the value was set else false
    def try_set(value = NULL, &block)
      set(value, &block)
      true
    rescue MultipleAssignmentError
      false
    end

    protected

    # @!visibility private
    def ns_initialize(value, opts)
      value = yield if block_given?
      init_obligation
      self.observers = Collection::SubscriptionObserverSet.new(Subscription)
      set_deref_options(opts)

      @state = :pending
      if value != NULL
        ns_complete_without_notification(true, value, nil)
      end
    end

    # @!visibility private
    def safe_execute(task, args = [])
      if compare_and_set_state(:processing, :pending)
        success, val, reason = SafeTaskExecutor.new(task, rescue_exception: true).execute(*@args)
        complete(success, val, reason)
        yield(success, val, reason) if block_given?
      end
    end

    # @!visibility private
    def complete(success, value, reason)
      complete_without_notification(success, value, reason)
      notify_observers(self.value, reason)
      self
    end

    # @!visibility private
    def complete_without_notification(success, value, reason)
      synchronize { ns_complete_without_notification(success, value, reason) }
      self
    end

    # @!visibility private
    def notify_observers(value, reason)
      observers.notify_and_delete_observers{ [Time.now, value, reason] }
    end

    # Shared implementation for #subscribe and #add_observer. Returns the
    # {Subscription}: either the one stored while the IVar is pending, or the
    # one that was just delivered when the IVar had already completed.
    #
    # @!visibility private
    def register_or_deliver(observer, func)
      subscription = nil
      direct_notification = synchronize do
        if event.set?
          true
        else
          subscription = observers.add_observer(observer, func)
          false
        end
      end

      if direct_notification
        # Delivered outside of the IVar lock so the callback may safely call
        # back into the IVar (e.g. #value). It is not registered with the
        # observer set, so nothing retains the observer once it returns.
        subscription = Subscription.new(nil, observer, func)
        subscription.deliver(Time.now, self.value, reason)
      end

      subscription
    end

    # @!visibility private
    def ns_complete_without_notification(success, value, reason)
      raise MultipleAssignmentError if [:fulfilled, :rejected].include? @state
      set_state(success, value, reason)
      event.set
    end

    # @!visibility private
    def check_for_block_or_value!(block_given, value) # :nodoc:
      if (block_given && value != NULL) || (! block_given && value == NULL)
        raise ArgumentError.new('must set with either a value or a block')
      end
    end
  end
end
