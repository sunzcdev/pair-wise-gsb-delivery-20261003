module Concurrent
  module Collection

    # A registration handle returned by {Concurrent::IVar#register_observer}.
    #
    # The handle allows an observer to detach itself when it is no longer
    # interested in the result of the `IVar`. All state transitions of a
    # registration are performed while holding the owning `IVar`'s lock, so the
    # outcome of the race between deregistration and completion is always
    # deterministic and can be inspected through the returned symbol and the
    # predicate methods.
    #
    # A registration is always in exactly one of three states:
    #
    # - `:pending` - the `IVar` has not completed yet, the observer is still
    #   subscribed and will be notified on completion.
    # - `:deregistered` - the observer was removed before the `IVar` completed
    #   and will never be notified.
    # - `:notified` - the `IVar` completed (or was already complete when the
    #   observer was registered) and the observer has been, or is about to be,
    #   notified exactly once.
    #
    # @example
    #   ivar   = Concurrent::IVar.new
    #   handle = ivar.register_observer(observer)
    #
    #   # later, if the result is no longer needed
    #   outcome = handle.deregister
    #   outcome #=> :deregistered or :notified
    #   handle.notified? #=> false when deregistration won the race
    class IVarObserverRegistration

      # @!visibility private
      #
      # The handle is only instantiated by {IVarObserverSet}; all of its
      # mutation happens under the owning `IVar`'s lock.
      def initialize(ivar, observer, func)
        @ivar     = ivar
        @observer = observer
        @func     = func
        @state    = :pending
      end

      # The observer which will receive the notification.
      #
      # @return [Object] the registered observer
      attr_reader :observer

      # The method called on {#observer} when the `IVar` completes.
      #
      # @return [Symbol] the notification method name
      attr_reader :func

      # Detach the observer from the `IVar`.
      #
      # It is safe to call this method concurrently with completion of the
      # `IVar`, and it is safe to call it more than once; subsequent calls are
      # no-ops returning the same result.
      #
      # @return [Symbol] `:deregistered` when the observer was removed before
      #   the `IVar` was completed and therefore will not be notified;
      #   `:notified` when the `IVar` had already completed (or completion was
      #   in progress) and the observer will still receive, or has already
      #   received, its single notification
      def deregister
        @ivar.send(:deregister_observer_registration, self)
      end
      alias_method :unsubscribe, :deregister

      # Has the `IVar` already completed for this registration?
      #
      # @return [Boolean] `true` when the observer will be (or has been)
      #   notified exactly once, `false` otherwise
      def notified?
        @ivar.send(:synchronize) { @state == :notified }
      end

      # Was the observer removed before the `IVar` completed?
      #
      # @return [Boolean] `true` when {#deregister} won the race against
      #   completion and the observer will never be notified
      def deregistered?
        @ivar.send(:synchronize) { @state == :deregistered }
      end

      # Is the observer still waiting for completion of the `IVar`?
      #
      # @return [Boolean] `true` while the registration is neither
      #   deregistered nor notified
      def pending?
        @ivar.send(:synchronize) { @state == :pending }
      end

      # @!visibility private
      # The remaining methods are only invoked by the `IVar` while its lock is
      # held, hence the `ns_` (no-synchronize) prefix.
      def ns_deregister
        return false unless @state == :pending

        @state = :deregistered
        true
      end

      # @!visibility private
      def ns_mark_notified
        return false unless @state == :pending

        @state = :notified
        true
      end

      # @!visibility private
      def ns_notified?
        @state == :notified
      end

      # @!visibility private
      def ns_invoke(time, value, reason)
        @observer.send(@func, time, value, reason)
      end
    end

    # A thread safe observer set backing {Concurrent::IVar}.
    #
    # Unlike {CopyOnWriteObserverSet} this set does not own a lock: every one
    # of its methods is invoked by the owning `IVar` while the `IVar`'s lock is
    # held. Keeping a single lock guarantees that adding, removing and
    # notifying observers, together with the completion state of the `IVar`,
    # form one atomic decision. The actual observer callbacks are executed by
    # the `IVar` *after* releasing the lock so that user code never runs while
    # an internal lock is held.
    #
    # @api private
    class IVarObserverSet

      # @param ivar [Concurrent::IVar] the owning `IVar`, referenced by the
      #   registration handles so they can deregister themselves
      def initialize(ivar)
        @ivar          = ivar
        @registrations = {}
      end

      # Registers an observer and returns its registration handle.
      #
      # Re-registering the same observer object replaces its previous
      # registration (matching the behaviour of the other observer sets) and
      # returns the new handle. The replaced handle is moved to the
      # `:deregistered` state so it can never be notified.
      #
      # Must be called under the owning `IVar`'s lock.
      #
      # @return [IVarObserverRegistration]
      def add_observer(observer, func)
        registration = IVarObserverRegistration.new(@ivar, observer, func)
        previous     = @registrations.delete(observer)
        previous.ns_deregister if previous
        @registrations[observer] = registration
        registration
      end

      # Removes a registration added through the legacy
      # {Concurrent::Concern::Observable#delete_observer} API.
      #
      # Must be called under the owning `IVar`'s lock.
      #
      # @return [Object] the removed observer
      def delete_observer(observer)
        registration = @registrations.delete(observer)
        registration.ns_deregister if registration
        observer
      end

      # Deregisters a registration handle.
      #
      # Must be called under the owning `IVar`'s lock.
      #
      # @return [Symbol] `:deregistered` or `:notified`
      def deregister(registration)
        if registration.ns_notified?
          :notified
        elsif registration.ns_deregister
          @registrations.delete(registration.observer)
          :deregistered
        else
          # Already deregistered, the handle is a no-op from now on.
          :deregistered
        end
      end

      # Removes every pending registration.
      #
      # Must be called under the owning `IVar`'s lock.
      #
      # @return [IVarObserverSet] self
      def delete_observers
        @registrations.each_value { |registration| registration.ns_deregister }
        @registrations.clear
        self
      end

      # @return [Integer] the number of currently registered observers
      # Must be called under the owning `IVar`'s lock.
      def count_observers
        @registrations.count
      end

      # Atomically marks every pending registration as notified and returns it
      # for dispatch outside the lock. Registrations added afterwards start
      # fresh, so an observer is never notified more than once.
      #
      # Must be called under the owning `IVar`'s lock.
      #
      # @return [Array<IVarObserverRegistration>] the registrations to notify
      def take_for_notification
        registrations = @registrations.values
        registrations.each { |registration| registration.ns_mark_notified }
        @registrations.clear
        registrations
      end
    end
  end
end