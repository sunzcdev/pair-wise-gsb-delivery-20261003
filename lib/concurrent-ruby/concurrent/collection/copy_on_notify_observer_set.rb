require 'concurrent/synchronization/lockable_object'

module Concurrent
  module Collection

    # A thread safe observer set implemented using copy-on-read approach:
    # observers are added and removed from a thread safe collection; every time
    # a notification is required the internal data structure is copied to
    # prevent concurrency issues
    #
    # @api private
    class CopyOnNotifyObserverSet < Synchronization::LockableObject

      def initialize
        super()
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
          @observers[observer] = func
          observer
        end
      end

      # @!macro observable_delete_observer
      def delete_observer(observer)
        synchronize do
          @observers.delete(observer)
          observer
        end
      end

      # @!macro observable_delete_observers
      def delete_observers
        synchronize do
          @observers.clear
          self
        end
      end

      # @!macro observable_count_observers
      def count_observers
        synchronize { @observers.count }
      end

      # Notifies all registered observers with optional args
      # @param [Object] args arguments to be passed to each observer
      # @return [CopyOnWriteObserverSet] self
      def notify_observers(*args, &block)
        observers = duplicate_observers
        notify_to(observers, *args, &block)
        self
      end

      # Notifies all registered observers with optional args and deletes them.
      #
      # @param [Object] args arguments to be passed to each observer
      # @return [CopyOnWriteObserverSet] self
      def notify_and_delete_observers(*args, &block)
        observers = duplicate_and_clear_observers
        notify_to(observers, *args, &block)
        self
      end

      # Notifies all registered observers, isolating observers from each other.
      #
      # Every observer is called exactly once, in registration order. Unlike
      # {#notify_observers}, an exception raised by one observer neither
      # prevents the remaining observers from being notified nor propagates to
      # the caller; the failures are collected and returned instead. This makes
      # the method suitable for announcing a change which has already been
      # committed and therefore cannot be undone by a failing observer.
      #
      # The arguments passed to each observer are either `args`, or the value
      # returned by the block, which is called once per observer and receives
      # the observer and the function name so the caller can tailor the payload
      # to the observer's arity.
      #
      # @param [Object] args arguments to be passed to each observer
      # @yield [observer, function] computes the arguments for a single observer
      # @return [Array<Array(Object, Exception)>] the observer/exception pairs
      #   in the order the failures happened, empty when nobody raised
      def notify_observers_safely(*args, &block)
        observers = duplicate_observers
        errors    = []

        observers.each do |observer, function|
          begin
            call_args = block ? block.call(observer, function) : args
            observer.send(function, *call_args)
          rescue Exception => error
            # `Exception` (not just `StandardError`) so that a `SystemExit` or
            # `Interrupt` raised by one observer does not stop the remaining
            # observers from being notified. The caller decides what to
            # propagate once everybody has been called.
            errors << [observer, error]
          end
        end

        errors
      end

      protected

      def ns_initialize
        @observers = {}
      end

      private

      def duplicate_and_clear_observers
        synchronize do
          observers = @observers.dup
          @observers.clear
          observers
        end
      end

      def duplicate_observers
        synchronize { @observers.dup }
      end

      def notify_to(observers, *args)
        raise ArgumentError.new('cannot give arguments and a block') if block_given? && !args.empty?
        observers.each do |observer, function|
          args = yield if block_given?
          observer.send(function, *args)
        end
      end
    end
  end
end
