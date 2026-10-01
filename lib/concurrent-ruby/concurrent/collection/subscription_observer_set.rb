require 'concurrent/synchronization/lockable_object'

module Concurrent
  module Collection

    # A thread safe observer set which keeps every observation as its own
    # object (a "subscription"), allowing an individual observation to be
    # cancelled before it is notified.
    #
    # Unlike {CopyOnNotifyObserverSet} it is not keyed by the observer: the
    # same observer may be registered more than once and each registration
    # gets an independent handle.
    #
    # @api private
    class SubscriptionObserverSet < Synchronization::LockableObject

      def initialize(subscription_class)
        super()
        @subscription_class = subscription_class
        synchronize { ns_initialize }
      end

      # @!macro observable_add_observer
      def add_observer(observer = nil, func = :update, &block)
        if observer.nil? && block.nil?
          raise ArgumentError, 'should pass observer as a first argument or block'
        elsif observer && block
          raise ArgumentError.new('cannot provide both an observer and a block')
        end

        if block
          observer = block
          func     = :call
        end

        synchronize do
          subscription = @subscription_class.new(self, observer, func)
          @subscriptions << subscription
          subscription
        end
      end

      # @!macro observable_delete_observer
      def delete_observer(observer)
        synchronize do
          @subscriptions.delete_if { |subscription| subscription.observer.equal?(observer) }
          observer
        end
      end

      # @!macro observable_delete_observers
      def delete_observers
        synchronize do
          @subscriptions = []
          self
        end
      end

      # @!macro observable_count_observers
      def count_observers
        synchronize { @subscriptions.count }
      end

      # Atomically removes and returns all the registered subscriptions.
      # After this call returns the set is empty, so any subscription that is
      # cancelled concurrently is no longer strongly referenced here.
      #
      # @return [Array<Object>] the subscriptions present at drain time
      def drain
        synchronize do
          subscriptions = @subscriptions
          @subscriptions = []
          subscriptions
        end
      end

      # Removes a single subscription from the live set. Used by a
      # subscription when it is cancelled or has already been delivered, so
      # that the set does not keep the observer (and whatever it closes over)
      # alive until the subject completes.
      #
      # @return [void]
      def discard(subscription)
        synchronize do
          @subscriptions.delete(subscription)
        end
        nil
      end

      # Notifies all registered subscriptions with optional args and deletes
      # them. Each subscription decides atomically whether it still wants to
      # be notified (it may have been cancelled after the drain), therefore
      # cancellation racing with completion has a single, consistent outcome.
      #
      # @param [Object] args arguments to be passed to each observer
      # @return [SubscriptionObserverSet] self
      def notify_and_delete_observers(*args, &block)
        drain.each do |subscription|
          arguments = block ? yield : args
          subscription.deliver(*arguments)
        end
        self
      end

      protected

      def ns_initialize
        @subscriptions = []
      end
    end
  end
end
