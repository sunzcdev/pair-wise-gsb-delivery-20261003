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
        organized.concat(interactors.flatten)
      end

      # Public: Declare an Interactor to be invoked as part of the
      # Interactor::Organizer's invocation with its context keys adapted to
      # the keys used by the organizer. The interactor is invoked against a
      # local context built from the declared inputs, and on success the
      # declared outputs are merged back into the organizer's context. The
      # interactor class itself does not need to change.
      #
      # interactor_class - An Interactor class to be invoked with adapted
      #                    context keys.
      # inputs           - A Hash mapping each local context key expected by
      #                    the interactor to the organizer context key
      #                    providing its value. (default: {})
      # outputs          - A Hash mapping each local context key produced by
      #                    the interactor to the organizer context key
      #                    receiving its value. (default: {})
      #
      # Examples
      #
      #   class MyOrganizer
      #     include Interactor::Organizer
      #
      #     organize FetchAccount
      #
      #     adapt ReceivePayment,
      #       inputs: {payer: :account},
      #       outputs: {receipt: :payment_receipt}
      #   end
      #
      # Returns nothing.
      def adapt(interactor_class, inputs: {}, outputs: {})
        organized << Adapter.new(interactor_class, inputs: inputs, outputs: outputs)
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
      # Returns an Array of Interactor classes and Interactor::Adapter
      #   instances or an empty Array.
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
          interactor.call!(context)
        end
      end
    end
  end
end
