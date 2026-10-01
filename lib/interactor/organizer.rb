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
        @organized = (@organized || []).concat(interactors.flatten)
      end

      # Public: Declare an optional, recoverable subflow. The given
      # Interactors are invoked as an isolated unit at this point in the
      # Organizer's sequence. When the subflow calls "context.fail!", its
      # steps are scoped-rolled-back, its top-level Context changes are
      # discarded, and the block is evaluated with the failed Context in the
      # Organizer instance's scope so the remaining organized Interactors still
      # run. Any other exception propagates.
      #
      # See Interactor#attempt for the equivalent that can be called from
      # inside an Interactor's "#call" method.
      #
      # interactors - Zero or more (or an Array of) Interactor classes.
      # recovery    - An optional Symbol naming an instance method invoked with
      #               the failed Context.
      # block       - An optional block evaluated with the failed Context.
      #
      # Examples
      #
      #   class PlaceOrder
      #     include Interactor::Organizer
      #
      #     organize CreateOrder, ChargeCard
      #
      #     attempt SendThankYou do |failure|
      #       context.notice = failure.message
      #     end
      #
      #     organize FinalizeOrder
      #   end
      #
      # Returns nothing.
      def attempt(*interactors, recovery: nil, &block)
        (@organized ||= []) << Attempt.new(interactors, recovery: recovery, &block)
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
      # Returns nothing.
      def call
        self.class.organized.each do |interactor|
          if interactor.is_a?(Attempt)
            interactor.run(context, self)
          else
            interactor.call!(context)
          end
        end
      end
    end
  end
end
