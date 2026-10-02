module Interactor
  # Public: Interactor::Organizer methods. Because Interactor::Organizer is a
  # module, custom Interactor::Organizer classes should include
  # Interactor::Organizer rather than inherit from it.
  #
  # Examples
  #
  #   class MyOrganizer
  #     include Interactor::Organizer
  #
  #     organize InteractorOne, InteractorTwo
  #   end
  module Organizer
    # Internal: Install Interactor::Organizer's behavior in the given class.
    def self.included(base)
      base.class_eval do
        include Interactor

        extend ClassMethods
        include InstanceMethods
      end
    end

    # Internal: Interactor::Organizer class methods.
    module ClassMethods
      # Public: Declare Interactors to be invoked as part of the
      # Interactor::Organizer's invocation. These interactors are invoked in
      # the order in which they are declared.
      #
      # interactors - Zero or more (or an Array of) Interactor classes.
      #
      # Examples
      #
      #   class MyFirstOrganizer
      #     include Interactor::Organizer
      #
      #     organize InteractorOne, InteractorTwo
      #   end
      #
      #   class MySecondOrganizer
      #     include Interactor::Organizer
      #
      #     organize [InteractorThree, InteractorFour]
      #   end
      #
      # Returns nothing.
      def organize(*interactors)
        @organized = interactors.flatten
      end

      # Internal: An Array of declared Interactors to be invoked.
      #
      # Examples
      #
      #   class MyOrganizer
      #     include Interactor::Organizer
      #
      #     organize InteractorOne, InteractorTwo
      #   end
      #
      #   MyOrganizer.organized
      #   # => [InteractorOne, InteractorTwo]
      #
      # Returns an Array of Interactor classes or an empty Array.
      def organized
        @organized ||= []
      end
    end

    # Internal: Interactor::Organizer instance methods.
    module InstanceMethods
      # Internal: Invoke the organized Interactors. An Interactor::Organizer is
      # expected not to define its own "#call" method in favor of this default
      # implementation.
      #
      # If the context carries an Interactor::Cancellation signal (under the
      # :cancellation key), the signal is checked at safe boundaries: before
      # the first interactor, after each interactor completes normally and
      # before the next interactor is invoked. A running interactor is never
      # interrupted. When the signal is found cancelled, the completed
      # interactor has already been tracked in the context, the context is
      # flagged as cancelled and Interactor::Cancelled is raised so that the
      # completed interactors are rolled back in the usual reverse order.
      # Nested organizers share the same context and therefore the same
      # signal.
      #
      # Returns nothing.
      def call
        signal = context.cancellation
        self.class.organized.each do |interactor|
          check_cancellation(signal)
          begin
            interactor.call!(context)
          rescue
            record_cancellation(signal)
            raise
          end
          check_cancellation(signal)
        end
      end

      private

      # Internal: Flag the context as cancelled and raise
      # Interactor::Cancelled if the given cancellation signal has been
      # cancelled.
      #
      # signal - An Interactor::Cancellation signal or nil.
      #
      # Returns nothing.
      # Raises Interactor::Cancelled if the signal is cancelled.
      def check_cancellation(signal)
        return unless signal && signal.cancelled?
        record_cancellation(signal)
        raise Cancelled.new(context, signal.reason)
      end

      # Internal: Flag the context as cancelled, preserving the cancellation
      # reason, without raising. Used when an ordinary error is already
      # propagating so that both the error and the cancellation reason are
      # preserved.
      #
      # signal - An Interactor::Cancellation signal or nil.
      #
      # Returns nothing.
      def record_cancellation(signal)
        return unless signal && signal.cancelled?
        context.cancelled!(signal.reason)
      end
    end
  end
end
