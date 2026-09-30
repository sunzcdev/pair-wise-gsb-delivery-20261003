# frozen_string_literal: true

require "concurrent/promise"

module Dry
  module Monads
    # The Task monad represents an async computation. The implementation
    # is a rather thin wrapper of Concurrent::Promise from the concurrent-ruby.
    # The API supports setting a custom executor from concurrent-ruby.
    #
    # @api public
    class Task
      # @api private
      class Promise < ::Concurrent::Promise
        public :on_fulfill, :on_reject
      end
      private_constant :Promise

      class << self
        # Creates a Task from a block
        #
        # @overload new(promise)
        #   @param promise [Promise]
        #   @return [Task]
        #
        # @overload new(&block)
        #   @param block [Proc] a task to run
        #   @return [Task]
        #
        def new(promise = nil, &)
          if promise
            super(promise)
          else
            super(Promise.execute(&))
          end
        end

        # Creates a Task with the given executor
        #
        # @example providing an executor instance, using Ruby 2.5+ syntax
        #   IO = Concurrent::ThreadPoolExecutor.new
        #   Task[IO] { do_http_request }
        #
        # @example using a predefined executor
        #    Task[:fast] { do_quick_task }
        #
        # @param executor [Concurrent::AbstractExecutorService,Symbol]
        #   Either an executor instance
        #   or a name of predefined global
        #   from concurrent-ruby
        #
        # @return [Task]
        def [](executor, &)
          new(Promise.execute(executor: executor, &))
        end

        # Returns a completed task from the given value
        #
        # @overload pure(value)
        #   @param value [Object]
        #   @return [Task]
        #
        # @overload pure(&block)
        #   @param block [Proc]
        #   @return [Task]
        #
        def pure(value = Undefined, &block)
          v = Undefined.default(value, block)
          new(Promise.fulfill(v))
        end

        # Returns a failed task from the given exception
        #
        # @param exc [Exception]
        # @return [Task]
        def failed(exc) = new(Promise.reject(exc))

        # Adapts a callback-based API (success/error callbacks, a.k.a. CPS)
        # to a Task.
        #
        # @example wrapping a legacy client
        #   Task.from_callbacks do |on_success, on_error|
        #     handle = legacy_client.submit(on_success, on_error)
        #     -> { handle.unsubscribe }
        #   end
        #
        # The block is called right away with two callables. It must register
        # them with the callback source and return an "unsubscribe" callable
        # (or nil if unsubscription isn't supported). The unsubscriber is
        # invoked exactly once, after the task reaches its first terminal
        # state, even if the registration block returned the handle later
        # than the callback (e.g. for synchronous callbacks).
        #
        # Resolution rules ("first terminal state wins"):
        #
        # 1. The first success or error signal completes the task, any later
        #    signals (including ones delivered from another thread) are
        #    ignored.
        # 2. A signal delivered synchronously, while the registration block
        #    is still running, still completes the task. If the block raises
        #    afterwards the exception is swallowed because a terminal result
        #    has already arrived.
        # 3. If the block raises before any signal arrives the task fails
        #    with that exception, so waiters are never left hanging.
        # 4. Errors raised by the unsubscribe callable are swallowed, they
        #    can't undo the terminal result.
        #
        # @yieldparam on_success [#call] completes the task with a value
        # @yieldparam on_error [#call] fails the task with an Exception
        # @yieldreturn [#call, nil] an unsubscribe handle, invoked once
        # @return [Task]
        # @api public
        def from_callbacks(&register)
          raise ArgumentError, "a block is required" unless block_given?

          gate = CallbackGate.new
          gate.start(&register)
          new(gate.promise)
        end
      end

      include ConversionStubs[:to_maybe, :to_result]
      extend ::Dry::Core::Deprecations[:"dry-monads"]

      # @api private
      attr_reader :promise
      protected :promise

      # @api private
      def initialize(promise)
        @promise = promise
      end

      # Retrieves the value of the computation.
      # Blocks current thread if the underlying promise
      # hasn't been complete yet.
      # Throws an error if the computation failed.
      #
      # @return [Object]
      # @api public
      def value!
        if promise.wait.fulfilled?
          promise.value
        else
          raise promise.reason
        end
      end

      # Lifts a block over the Task monad.
      #
      # @param block [Proc]
      # @return [Task]
      # @api public
      def fmap(&) = self.class.new(promise.then(&))

      # Composes two tasks to run one after another.
      # A more common name is `then` exists as an alias.
      #
      # @param block [Proc] A block that yields the result of the current task
      #                     and returns another task
      # @return [Task]
      def bind(&block)
        self.class.new(promise.flat_map { block.(_1).promise })
      end
      deprecate :then, :bind

      # @return [String]
      def to_s
        state = case promise.state
                when :fulfilled
                  if Unit.equal?(value!)
                    "value=()"
                  else
                    "value=#{value!.inspect}"
                  end
                when :rejected
                  "error=#{promise.reason.inspect}"
                else
                  "?"
                end

        "Task(#{state})"
      end
      alias_method :inspect, :to_s

      # Transforms the error if the computation wasn't successful.
      #
      # @param block [Proc]
      # @return [Task]
      def or_fmap(&) = self.class.new(promise.rescue(&))

      # Rescues the error with a block that returns another task.
      #
      # @param block [Proc]
      # @return [Object]
      def or(&block)
        child = Promise.new(
          parent: promise,
          executor: Concurrent::ImmediateExecutor.new
        )

        promise.on_error do |v|
          inner = block.(v).promise
          inner.execute
          inner.on_success { child.on_fulfill(_1) }
          inner.on_error { child.on_reject(_1) }
        rescue StandardError => exception
          child.on_reject(exception)
        end
        promise.on_success { child.on_fulfill(_1) }

        self.class.new(child)
      end

      # Extracts the resulting value if the computation was successful
      # otherwise yields the block and returns its result.
      #
      # @param block [Proc]
      # @return [Object]
      def value_or(&) = promise.rescue(&).wait.value

      # Blocks the current thread until the task is complete.
      #
      # @return [Task]
      def wait(timeout = nil)
        promise.wait(timeout)
        self
      end

      # Compares two tasks. Note, it works
      # good enough only for complete tasks.
      #
      # @return [Boolean]
      def ==(other)
        return true if equal?(other)
        return false unless self.class == other.class

        compare_promises(promise, other.promise)
      end

      # Whether the computation is complete.
      #
      # @return [Boolean]
      def complete? = promise.complete?

      # @return [Class]
      def monad = Task

      # Returns self.
      #
      # @return [Maybe::Some, Maybe::None]
      def to_monad = self

      # Applies the stored value to the given argument.
      #
      # @example
      #   Task.
      #     pure { |x, y| x ** y }.
      #     apply(Task { 2 }).
      #     apply(Task { 3 }).
      #     to_maybe # => Some(8)
      #
      # @param val [Task]
      # @return [Task]
      def apply(val = Undefined, &)
        arg = Undefined.default(val, &)
        bind { |f| arg.fmap { curry(f).(_1) } }
      end

      # Maps a successful result to Unit, effectively discards it
      #
      # @return [Task]
      def discard = fmap { Unit }

      # Maps the output of a callback-based source onto a single promise.
      #
      # Two things must each happen at most once, independently of each
      # other: settling the promise and invoking the unsubscribe handle.
      # The first terminal signal settles the promise at once so waiters
      # can never hang, even if the registration block raises afterwards
      # or never returns the handle. The unsubscribe, in contrast, has to
      # wait for the handle, which can be produced later than the signal
      # (synchronous callbacks).
      #
      # @api private
      class CallbackGate
        # @return [Task::Promise]
        attr_reader :promise

        def initialize
          @mutex = ::Mutex.new
          @promise = Promise.new(executor: Concurrent::ImmediateExecutor.new)
          @settled = false
          @unsubscriber = nil
        end

        # Runs the registration block. The block is expected to register
        # the two given callables with the callback source and return an
        # optional unsubscribe callable.
        #
        # @return [void]
        def start
          begin
            unsubscriber = yield(method(:succeed), method(:fail))
          rescue ::StandardError => exception
            registration_failed(exception)
            return
          end

          registered(unsubscriber)
        end

        # Completes the task with the given value. No-op if a terminal
        # signal has already arrived.
        #
        # @param value [Object]
        # @return [void]
        def succeed(value)
          signal(true, value, nil)
        end

        # Fails the task with the given reason. No-op if a terminal signal
        # has already arrived. Non-exception reasons are wrapped.
        #
        # @param reason [Exception]
        # @return [void]
        def fail(reason)
          unless reason.is_a?(::Exception)
            reason = StandardError.new(reason.inspect)
          end

          signal(false, nil, reason)
        end

        private

        # Handles the very first terminal signal.
        #
        # @return [void]
        def signal(success, value, reason)
          unsubscriber = @mutex.synchronize do
            return if @settled

            @settled = true
            consume_unsubscriber
          end

          settle_promise(success, value, reason)
          unsubscribe(unsubscriber)
        end

        # Handles the handle produced by a normally returned registration
        # block. A non-callable value is only rejected if no result has
        # arrived yet; otherwise the first terminal result wins and the bad
        # handle is ignored.
        #
        # @return [void]
        def registered(unsubscriber)
          unless unsubscriber.nil? || unsubscriber.respond_to?(:call)
            signal_first = @mutex.synchronize { @settled }
            raise TypeError, "unsubscribe handle must respond to #call" unless signal_first

            return
          end

          pending = @mutex.synchronize do
            @unsubscriber = unsubscriber
            if @settled
              consume_unsubscriber
            else
              nil
            end
          end
          unsubscribe(pending)
        end

        # Handles a registration block that raised. If no terminal signal
        # has arrived yet the exception becomes the failure of the task;
        # otherwise the "first terminal state wins" rule makes it
        # irrelevant, so it is swallowed instead of leaking into whoever
        # invoked the (possibly synchronous) callback chain.
        #
        # @return [void]
        def registration_failed(exception)
          unsubscriber, failure = @mutex.synchronize do
            if @settled
              [consume_unsubscriber, nil]
            else
              @settled = true
              [nil, exception]
            end
          end

          if failure
            settle_promise(false, nil, failure)
          else
            unsubscribe(unsubscriber)
          end
        end

        # Settles the pending promise. Called at most once.
        #
        # @return [void]
        def settle_promise(success, value, reason)
          if success
            @promise.on_fulfill(value)
          else
            @promise.on_reject(reason)
          end
        end

        # Takes the stored handle so it can be invoked exactly once. Must
        # be called while holding the mutex.
        #
        # @return [#call, nil]
        def consume_unsubscriber
          handle = @unsubscriber
          @unsubscriber = nil
          handle
        end

        # Invokes the handle, swallowing its errors: a broken unsubscriber
        # must not mask or undo the result.
        #
        # @return [void]
        def unsubscribe(callable)
          callable&.call
          nil
        rescue ::StandardError
          nil
        end
      end

      private

      # @api private
      def curry(value)
        if defined?(@curried)
          if @curried[0].equal?(value)
            @curried[1]
          else
            Curry.(value)
          end
        else
          @curried = [value, Curry.(value)]
          @curried[1]
        end
      end

      # @api private
      def compare_promises(x, y)
        x.equal?(y) ||
          (x.fulfilled? && y.fulfilled? && x.value == y.value) ||
          (x.rejected? && y.rejected? && x.reason == y.reason)
      end

      # Task constructors.
      #
      # @api public
      module Mixin
        # @private
        Task = Task

        # @see Dry::Monads::Unit
        Unit = Unit # @private

        # Created a mixin with the given executor injected.
        #
        # @param executor [Concurrent::AbstractExecutorService,Symbol]
        # @return [Module]
        def self.[](executor)
          Module.new do
            include Mixin

            # Created a new Task with an injected executor.
            #
            # @param block [Proc]
            # @return [Task]
            define_method(:Task) do |&block|
              Task[executor, &block]
            end
          end
        end

        # Task constructors
        module Constructors
          # Builds a new Task instance.
          #
          # @param block [Proc]
          # @return Task
          def Task(&) = Task.new(&)
        end

        include Constructors
      end
    end

    require "dry/monads/registry"
    register_mixin(:task, Task::Mixin)
  end
end
