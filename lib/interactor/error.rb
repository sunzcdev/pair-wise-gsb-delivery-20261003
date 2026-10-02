module Interactor
  # Internal: Error raised during Interactor::Context failure. The error stores
  # a copy of the failed context for debugging purposes.
  class Failure < StandardError
    # Internal: Gets the Interactor::Context of the Interactor::Failure
    # instance.
    attr_reader :context

    # Internal: Initialize an Interactor::Failure.
    #
    # context - An Interactor::Context to be stored within the
    #           Interactor::Failure instance. (default: nil)
    #
    # Examples
    #
    #   Interactor::Failure.new
    #   # => #<Interactor::Failure: Interactor::Failure>
    #
    #   context = Interactor::Context.new(foo: "bar")
    #   # => #<Interactor::Context foo="bar">
    #   Interactor::Failure.new(context)
    #   # => #<Interactor::Failure: #<Interactor::Context foo="bar">>
    #
    #   raise Interactor::Failure, context
    #   # => Interactor::Failure: #<Interactor::Context foo="bar">
    def initialize(context = nil)
      @context = context
      super
    end
  end

  # Internal: Error raised when an Interactor::Cancellation signal is found
  # cancelled at a safe boundary during Interactor::Organizer invocation. The
  # error is distinct from Interactor::Failure so that cancellation is never
  # conflated with an ordinary context failure. The error stores the context
  # and the cancellation reason.
  class Cancelled < StandardError
    # Internal: Gets the Interactor::Context of the Interactor::Cancelled
    # instance.
    attr_reader :context

    # Internal: Gets the reason given when the cancellation signal was
    # cancelled.
    attr_reader :reason

    # Internal: Initialize an Interactor::Cancelled.
    #
    # context - An Interactor::Context to be stored within the
    #           Interactor::Cancelled instance. (default: nil)
    # reason  - The reason given when the cancellation signal was cancelled.
    #           (default: nil)
    #
    # Examples
    #
    #   Interactor::Cancelled.new
    #   # => #<Interactor::Cancelled: Interactor::Cancelled>
    #
    #   context = Interactor::Context.new(foo: "bar")
    #   # => #<Interactor::Context foo="bar">
    #   Interactor::Cancelled.new(context, "client went away")
    #   # => #<Interactor::Cancelled: client went away>
    def initialize(context = nil, reason = nil)
      @context = context
      @reason = reason
      super(reason)
    end
  end
end
