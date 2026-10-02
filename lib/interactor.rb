require "interactor/context"
require "interactor/commit_error"
require "interactor/error"
require "interactor/hooks"
require "interactor/organizer"

# Public: Interactor methods. Because Interactor is a module, custom Interactor
# classes should include Interactor rather than inherit from it.
#
# Examples
#
#   class MyInteractor
#     include Interactor
#
#     def call
#       puts context.foo
#     end
#   end
module Interactor
  # Internal: Install Interactor's behavior in the given class.
  def self.included(base)
    base.class_eval do
      extend ClassMethods
      include Hooks

      # Public: Gets the Interactor::Context of the Interactor instance.
      attr_reader :context
    end
  end

  # Internal: Interactor class methods.
  module ClassMethods
    # Public: Invoke an Interactor. This is the primary public API method to an
    # interactor.
    #
    # context - A Hash whose key/value pairs are used in initializing a new
    #           Interactor::Context object. An existing Interactor::Context may
    #           also be given. (default: {})
    #
    # Examples
    #
    #   MyInteractor.call(foo: "bar")
    #   # => #<Interactor::Context foo="bar">
    #
    #   MyInteractor.call
    #   # => #<Interactor::Context>
    #
    # Returns the resulting Interactor::Context after manipulation by the
    #   interactor.
    def call(context = {})
      new(context).tap(&:run).context
    end

    # Public: Invoke an Interactor. The "call!" method behaves identically to
    # the "call" method with one notable exception. If the context is failed
    # during invocation of the interactor, the Interactor::Failure is raised.
    #
    # context - A Hash whose key/value pairs are used in initializing a new
    #           Interactor::Context object. An existing Interactor::Context may
    #           also be given. (default: {})
    #
    # Examples
    #
    #   MyInteractor.call!(foo: "bar")
    #   # => #<Interactor::Context foo="bar">
    #
    #   MyInteractor.call!
    #   # => #<Interactor::Context>
    #
    #   MyInteractor.call!(foo: "baz")
    #   # => Interactor::Failure: #<Interactor::Context foo="baz">
    #
    # Returns the resulting Interactor::Context after manipulation by the
    #   interactor.
    # Raises Interactor::Failure if the context is failed.
    def call!(context = {})
      new(context).tap(&:run!).context
    end
  end

  # Internal: Initialize an Interactor.
  #
  # context - A Hash whose key/value pairs are used in initializing the
  #           interactor's context. An existing Interactor::Context may also be
  #           given. (default: {})
  #
  # Examples
  #
  #   MyInteractor.new(foo: "bar")
  #   # => #<MyInteractor @context=#<Interactor::Context foo="bar">>
  #
  #   MyInteractor.new
  #   # => #<MyInteractor @context=#<Interactor::Context>>
  def initialize(context = {})
    @context = Context.build(context)
  end

  # Internal: Invoke an interactor instance along with all defined hooks. The
  # "run" method is used internally by the "call" class method. The following
  # are equivalent:
  #
  #   MyInteractor.call(foo: "bar")
  #   # => #<Interactor::Context foo="bar">
  #
  #   interactor = MyInteractor.new(foo: "bar")
  #   interactor.run
  #   interactor.context
  #   # => #<Interactor::Context foo="bar">
  #
  # After successful invocation of the interactor, the instance is tracked
  # within the context. If the context is failed or any error is raised, the
  # context is rolled back.
  #
  # Returns nothing.
  def run
    run!
  rescue Failure => e
    if context.object_id != e.context.object_id
      raise
    end
  rescue CommitError => error
    belongs_to_context = context._commit_stack.any? do |frame|
      frame[:boundary].equal?(error.boundary)
    end
    raise if belongs_to_context
    context.after_commit_result = error.result
  end

  # Internal: Invoke an Interactor instance along with all defined hooks. The
  # "run!" method is used internally by the "call!" class method. The following
  # are equivalent:
  #
  #   MyInteractor.call!(foo: "bar")
  #   # => #<Interactor::Context foo="bar">
  #
  #   interactor = MyInteractor.new(foo: "bar")
  #   interactor.run!
  #   interactor.context
  #   # => #<Interactor::Context foo="bar">
  #
  # After successful invocation of the interactor, the instance is tracked
  # within the context. If the context is failed or any error is raised, the
  # context is rolled back.
  #
  # The "run!" method behaves identically to the "run" method with one notable
  # exception. If the context is failed during invocation of the interactor,
  # the Interactor::Failure is raised.
  #
  # Returns nothing.
  # Raises Interactor::Failure if the context is failed.
  def run!
    frame = context._begin_commit_boundary
    begin
      with_hooks do
        call
        context.called!(self)
        self.class.after_commit_hooks.each do |hook|
          context.after_commit(register_after_commit(hook))
        end
      end
    rescue CommitError
      context._end_commit_frame!(frame)
      raise
    rescue
      if context._commit_boundary_owner?(frame)
        context._discard_commit_boundary!(frame)
      end
      context._end_commit_frame!(frame)
      context.rollback!
      raise
    end

    if context._commit_boundary_owner?(frame)
      begin
        result = context._commit!(frame)
        context.after_commit_result = result
      rescue CommitError => error
        raise if error.boundary != frame[:boundary]

        context.after_commit_result = error.result
        context._end_commit_frame!(frame)
        raise
      end
    end
    context._end_commit_frame!(frame)
  end

  # Public: Invoke an Interactor instance without any hooks, tracking, or
  # rollback. It is expected that the "call" instance method is overwritten for
  # each interactor class.
  #
  # Returns nothing.
  def call
  end

  # Public: Register a callback to run after the outermost successful
  # invocation of the context has completed. May be called from within "call"
  # or a hook; callbacks run in registration order.
  #
  # block - A block to execute during the commit phase.
  #
  # Examples
  #
  #   def call
  #     after_commit { Notifier.deliver(context.user) }
  #   end
  #
  # Returns nothing.
  def after_commit(&block)
    context.after_commit(register_after_commit(block))
  end

  # Public: Reverse prior invocation of an Interactor instance. Any interactor
  # class that requires undoing upon downstream failure is expected to overwrite
  # the "rollback" instance method.
  #
  # Returns nothing.
  def rollback
  end

  private

  # Internal: Wrap a declared hook into a callable that executes in the scope
  # of this interactor instance during the commit phase.
  #
  # hook - A Symbol or Proc hook.
  #
  # Returns a callable.
  def register_after_commit(hook)
    if hook.is_a?(Symbol)
      proc { send(hook) }
    else
      proc { instance_exec(&hook) }
    end
  end
end
