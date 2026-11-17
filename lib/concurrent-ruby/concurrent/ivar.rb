require 'concurrent/constants'
require 'concurrent/errors'
require 'concurrent/collection/ivar_observer_set'
require 'concurrent/concern/logging'
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
    include Concern::Logging

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
    # When the `IVar` has already completed the observer is notified
    # synchronously, from the calling thread, before this method returns.
    # Existing observers are never notified again.
    #
    # @param [Object] observer the object that will be notified of changes
    # @param [Symbol] func symbol naming the method to call when this
    #   `Observable` has changes`
    # @return [Object] the added observer
    #
    # @see #register_observer when a handle is needed to deregister the
    #   observer again
    def add_observer(observer = nil, func = :update, &block)
      register(observer, func, block).observer
    end

    # Add an observer and return a {IVarObserverRegistration} handle which can
    # be used to deregister it again and to inspect the outcome of the race
    # between deregistration and completion.
    #
    # The observer is notified exactly once:
    #
    # - while the `IVar` is pending, the registration stays active and the
    #   observer is notified on completion unless
    #   {IVarObserverRegistration#deregister} wins the race first;
    # - when the `IVar` has already completed, the observer is notified
    #   synchronously, from the calling thread, before this method returns and
    #   the returned handle is already in the `:notified` state.
    #
    # Re-registering the same observer object replaces its previous
    # registration and returns a fresh handle; the old handle becomes
    # deregistered and will not be notified.
    #
    # @param [Object] observer the object that will be notified of changes
    # @param [Symbol] func symbol naming the method to call when this
    #   `Observable` has changes
    # @return [IVarObserverRegistration] the registration handle
    #
    # @raise [ArgumentError] when neither an observer nor a block is given, or
    #   when both are given
    def register_observer(observer = nil, func = :update, &block)
      register(observer, func, block)
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

    # Remove `observer` as an observer on this object so that it will no
    # longer receive notifications.
    #
    # When the `IVar` has already completed the observer was already notified
    # and the call simply returns it.
    #
    # @param [Object] observer the observer to remove
    # @return [Object] the removed observer
    def delete_observer(observer)
      synchronize { observers.delete_observer(observer) }
    end

    # Remove all observers associated with this object.
    #
    # @return [IVar] self
    def delete_observers
      synchronize { observers.delete_observers }
      self
    end

    # Return the number of observers currently associated with this object.
    #
    # @return [Integer] the observers count
    def count_observers
      synchronize { observers.count_observers }
    end

    protected

    # @!visibility private
    def ns_initialize(value, opts)
      value = yield if block_given?
      init_obligation
      self.observers = Collection::IVarObserverSet.new(self)
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
    #
    # Takes a consistent snapshot of the observers and marks them as notified
    # while holding the lock, then runs the user callbacks outside the lock so
    # that arbitrary user code is never executed while an internal lock is
    # held. A failure in one callback is logged but neither prevents the other
    # observers from being notified nor changes the value or reason of the
    # `IVar`.
    def notify_observers(value, reason)
      registrations = synchronize { observers.take_for_notification }
      time          = Time.now
      registrations.each { |registration| invoke_observer(registration, time, value, reason) }
      self
    end

    # @!visibility private
    #
    # Invoked by {IVarObserverRegistration#deregister}. Returns `:deregistered`
    # when the registration was removed before completion, `:notified` when
    # completion won the race and the observer will still be notified once.
    def deregister_observer_registration(registration)
      synchronize { observers.deregister(registration) }
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

    private

    # Shared implementation of #add_observer and #register_observer. The
    # decision whether the observer must be notified directly is made
    # atomically with its registration under the lock; the resulting callback,
    # if any, is only executed after releasing the lock.
    def register(observer, func, block)
      raise ArgumentError.new('cannot provide both an observer and a block') if observer && block

      if block
        observer = block
        func     = :call
      end

      raise ArgumentError.new('should pass observer as a first argument or block') if observer.nil?

      direct_notification = false
      registration       = synchronize do
        # The observer set returns the registration handle. When it is stubbed
        # in specs and returns something else (typically `nil`) a fallback
        # handle is built here so the caller still receives the expected API.
        stored = observers.add_observer(observer, func)
        reg    = stored.is_a?(Collection::IVarObserverRegistration) ? stored :
                   Collection::IVarObserverRegistration.new(self, observer, func)
        if event.set?
          # The snapshot also clears the set, so an already completed `IVar`
          # keeps no observers around and the registration is atomically
          # marked as notified.
          observers.take_for_notification
          direct_notification = true
        end
        reg
      end

      invoke_observer(registration, Time.now, self.value, reason) if direct_notification
      registration
    end

    # Runs a single observer callback without holding any internal lock and
    # isolates its failures from other observers and from the `IVar` state.
    def invoke_observer(registration, time, value, reason)
      registration.ns_invoke(time, value, reason)
    rescue StandardError => error
      log(Concern::Logging::ERROR, self.class.name) { error }
      nil
    end
  end
end
