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

        # Runs a loop over Task-returning steps and returns a Task
        # for the final value.
        #
        # The block is called with the current state and must return
        # a Task producing a control marker: Continue(next_state) to
        # keep looping or Done(value) to stop. A rejected task halts
        # the loop and its error is propagated as is, without calling
        # the block again.
        #
        # Steps that have already completed are advanced in a trampoline
        # loop, so any number of them runs without growing the Ruby call
        # stack (unlike a recursion built on top of #bind). Steps that
        # are still running are chained asynchronously: the calling
        # thread is never blocked waiting for them.
        #
        # @example
        #   Dry::Monads::Task.tail_rec(0) do |i|
        #     Task { i < 10 ? Continue(i + 1) : Done(i) }
        #   end # => Task(value=10)
        #
        # @param initial [Object] initial state
        # @yieldparam state [Object] current state
        # @yieldreturn [Task] a task of Continue(state) or Done(value)
        # @return [Task]
        def tail_rec(initial, &block)
          state = initial

          loop do
            task = block.call(state)

            unless task.is_a?(Task)
              raise ::TypeError,
                    "Task.tail_rec expects the block to return a Task, " \
                    "got: #{task.inspect}"
            end

            promise = task.__send__(:promise)

            if promise.fulfilled?
              step = TailRec.check_step!(promise.value)

              return pure(step.value) if step.is_a?(TailRec::Done)

              state = step.state
            elsif promise.rejected?
              return task
            else
              # The step is still running: chain the rest of the loop
              # asynchronously instead of blocking the calling thread
              return task.bind do |value|
                step = TailRec.check_step!(value)

                if step.is_a?(TailRec::Done)
                  pure(step.value)
                else
                  tail_rec(step.state, &block)
                end
              end
            end
          end
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
        include TailRec::Constructors
      end
    end

    require "dry/monads/registry"
    register_mixin(:task, Task::Mixin)
  end
end
