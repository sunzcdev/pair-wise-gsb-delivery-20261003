require 'concurrent/constants'
require 'concurrent/errors'
require 'concurrent/collection/copy_on_write_observer_set'
require 'concurrent/concern/obligation'
require 'concurrent/concern/observable'
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

    # A handle representing a single subscription created by {IVar#observe}.
    #
    # The handle can be used to cancel the delivery of the notification as
    # long as the notification has not started yet. {#cancel} makes the
    # outcome of the race explicit: it returns `true` when the cancellation
    # won the race against the callback (the callback is guaranteed to never
    # run) and `false` when the callback had already started, had already
    # completed, or the subscription had already been cancelled.
    #
    # A subscription holds its observer only until it is cancelled or
    # delivered; afterwards the `IVar` no longer references it.
    class Subscription

      # @!visibility private
      def initialize(ivar, observer, func)
        @ivar     = ivar
        @observer = observer
        @func     = func
        @lock     = Mutex.new
        @state    = :pending # one of :pending, :delivering, :delivered, :cancelled
      end

      # Attempt to cancel the subscription.
      #
      # When `true` is returned the cancellation happened before the
      # notification callback started, so the callback is guaranteed to never
      # be invoked and the subscription is removed from the `IVar` (it is no
      # longer strongly referenced by the observer set). When `false` is
      # returned the notification had already started (or finished), or the
      # subscription had already been cancelled.
      #
      # @return [Boolean] true if cancelled before the notification started
      def cancel
        @lock.synchronize do
          return false unless @state == :pending
          @state = :cancelled
        end
        @ivar.send(:remove_subscription, self)
        true
      end

      # @return [Boolean] true if the subscription was cancelled before delivery
      def cancelled?
        @lock.synchronize { @state == :cancelled }
      end

      # @return [Boolean] true if the notification callback has run to completion
      def delivered?
        @lock.synchronize { @state == :delivered }
      end

      # @return [Boolean] true if the notification has neither started nor been cancelled
      def pending?
        @lock.synchronize { @state == :pending }
      end

      # @!visibility private
      #
      # Atomically transitions from :pending to :delivering. Returns true when
      # this call won the race against a concurrent #cancel, false otherwise.
      def begin_delivery
        @lock.synchronize do
          if @state == :pending
            @state = :delivering
            true
          else
            false
          end
        end
      end

      # @!visibility private
      def finish_delivery
        @lock.synchronize { @state = :delivered }
        nil
      end

      # @!visibility private
      def deliver(time, value, reason)
        @observer.send(@func, time, value, reason)
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
    # @param [Object] observer the object that will be notified of changes
    # @param [Symbol] func symbol naming the method to call when this
    #   `Observable` has changes`
    def add_observer(observer = nil, func = :update, &block)
      raise ArgumentError.new('cannot provide both an observer and a block') if observer && block
      direct_notification = false

      if block
        observer = block
        func = :call
      end

      synchronize do
        if event.set?
          direct_notification = true
        else
          observers.add_observer(observer, func)
        end
      end

      observer.send(func, Time.now, self.value, reason) if direct_notification
      observer
    end

    # Add an observer on this object and return a {Subscription} handle that
    # can be used to cancel the notification before it starts.
    #
    # Behaves like {#add_observer} with respect to the notification itself:
    # the observer is called with the completion `Time`, the final `value`
    # (or `nil` on rejection) and the final `reason` (or `nil` on
    # fulfillment). When the `IVar` is already complete the observer is
    # notified immediately, before this method returns, and the returned
    # subscription is already delivered.
    #
    # Callbacks are never run while holding the internal lock, and an
    # exception raised by one callback does not prevent the remaining
    # observers from being notified.
    #
    # @param [Object] observer the object that will be notified of changes
    # @param [Symbol] func symbol naming the method to call on notification
    # @return [Subscription] a handle that can cancel the pending notification
    def observe(observer = nil, func = :update, &block)
      raise ArgumentError.new('cannot provide both an observer and a block') if observer && block

      if block
        observer = block
        func = :call
      end
      raise ArgumentError.new('should pass observer as a first argument or block') if observer.nil?

      subscription = Subscription.new(self, observer, func)
      direct_notification = synchronize do
        if event.set?
          true
        else
          @subscriptions[subscription] = true
          false
        end
      end

      deliver_subscription(subscription) if direct_notification
      subscription
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
      self.observers = Collection::CopyOnWriteObserverSet.new
      @subscriptions = {}
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
      notify_subscriptions(value, reason)
    end

    # @!visibility private
    #
    # Collects the pending subscriptions under the lock, then delivers every
    # notification outside of it. Each subscription is asked to begin its
    # delivery individually so that a concurrent Subscription#cancel can
    # still win the race and suppress its own callback.
    def notify_subscriptions(value, reason)
      pending = synchronize do
        subscriptions = @subscriptions
        @subscriptions = {}
        subscriptions.keys
      end
      time = Time.now
      pending.each { |subscription| deliver_subscription(subscription, time, value, reason) }
    end

    # @!visibility private
    #
    # Delivers a single subscription outside of any internal lock. Skips
    # subscriptions whose cancellation won the race. An exception raised by
    # the callback is swallowed so that the remaining observers are still
    # notified.
    def deliver_subscription(subscription, time = Time.now, value = self.value, reason = self.reason)
      return false unless subscription.begin_delivery
      begin
        subscription.deliver(time, value, reason)
      rescue StandardError
        # a misbehaving observer must not prevent the others from being notified
      ensure
        subscription.finish_delivery
      end
      true
    end

    # @!visibility private
    def remove_subscription(subscription)
      synchronize { @subscriptions.delete(subscription) }
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
