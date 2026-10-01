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

  # Public: Error raised when an Interactor::Adapter cannot find a declared
  # input key in the parent context. A key that is present with a nil value
  # does not raise this error.
  class MissingInputError < StandardError
  end

  # Public: Error raised when an Interactor::Adapter cannot find a declared
  # output key in the interactor's local context after invocation. A key that
  # is present with a nil value does not raise this error.
  class MissingOutputError < StandardError
  end

  # Public: Error raised when an Interactor::Adapter is declared with more
  # than one output mapped to the same parent context key. The error is
  # raised when the adapter is declared, before any invocation.
  class DuplicateOutputError < StandardError
  end
end
