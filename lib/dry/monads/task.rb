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

        # Runs a stack-safe tail-recursive loop. The block receives the
        # state of the current iteration, runs asynchronously, and must
        # return a {Task} wrapping either {Dry::Monads::Continue
        # Continue}(new_state) or {Dry::Monads::Done Done}(final_value).
        #
        # Every iteration is scheduled on the task executor, so the chain
        # neither grows the Ruby call stack nor blocks the calling thread:
        # the method itself always returns immediately with a pending
        # {Task}. A rejected task stops the loop and its error is propagated,
        # never swallowed.
        #
        # @example
        #   include Dry::Monads[:task, :tail_rec]
        #
        #   Task.tail_rec(first_page) do |page|
        #     Task { fetch_page(page) }.fmap do |response|
        #       response.last? ? Done(response.items) : Continue(response.next_page)
        #     end
        #   end
        #
        # @param state [Object] state of the first iteration
        # @yield [Object] state of the current iteration
        # @yieldreturn [Task] a task wrapping a Continue/Done marker
        # @return [Task]
        def tail_rec(state = Undefined, &block)
          new(Promise.fulfill(Undefined.default(state, Unit))).tail_rec(&block)
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

      # Runs a stack-safe tail-recursive loop on top of this task.
      # See {Task.tail_rec} for the block contract. The method returns
      # immediately: iterations are driven asynchronously by the task
      # executor without growing the call stack or blocking the caller.
      #
      # @yield [Object] state of the current iteration
      # @yieldreturn [Task] a task wrapping a Continue/Done marker
      # @return [Task]
      def tail_rec(&block)
        self.class.new(tail_rec_promise(promise, &block))
      end

      private

      # Builds the final promise and drives iterations asynchronously.
      #
      # A single unscheduled root promise is reused as the result and the
      # loop is a trampoline: a worker job runs steps inline while their
      # promises are already resolved (the common, IO-free case), so a
      # flat call stack never costs more than one executor hop per
      # scheduled chunk. When a step is genuinely pending the worker
      # parks and the promise's callbacks post the next job — no thread
      # is ever blocked waiting. A rejected step or an error raised in a
      # block rejects the final promise, following the usual Task
      # semantics: failures stop the loop and are never swallowed.
      #
      # @param initial [Promise]
      # @yield [Object]
      # @return [Promise]
      # @api private
      def tail_rec_promise(initial, &block)
        result = Promise.new
        executor = Concurrent.global_io_executor

        # Runs resolved steps inline until Done, a failure, or a pending
        # promise is hit. +state+ is the pending input for the first step.
        trampoline = lambda do |state|
          loop do
            next_task =
              begin
                block.(state)
              rescue ::StandardError => exception
                result.on_reject(exception)
                return
              end

            unless next_task.is_a?(Task)
              result.on_reject(TailRec::MarkerError.new(next_task))
              return
            end

            step = next_task.promise
            step.execute

            if step.pending?
              # Genuinely async step: park until it completes.
              step.on_success { |marker| tail_rec_resume(result, marker, executor, &trampoline) }
              step.on_error { |error| result.on_reject(error) }
              return
            elsif step.rejected?
              result.on_reject(step.reason)
              return
            end

            marker = step.value

            case marker
            when TailRec::Done
              result.on_fulfill(marker.value!)
              return
            when TailRec::Continue
              state = marker.value!
            else
              result.on_reject(TailRec::MarkerError.new(marker))
              return
            end
          end
        end

        initial.execute

        if initial.unscheduled? || initial.pending?
          initial.on_success { |state| executor.post { trampoline.(state) } }
          initial.on_error { |error| result.on_reject(error) }
        elsif initial.rejected?
          result.on_reject(initial.reason)
        else
          state = initial.value
          executor.post { trampoline.(state) }
        end

        result
      end

      # Resumes a parked trampoline with a marker resolved off-thread.
      # Done and marker errors finish the result; Continue schedules a
      # fresh worker job so the call chain never nests.
      #
      # @return [void]
      # @api private
      def tail_rec_resume(result, marker, executor)
        case marker
        when TailRec::Done
          result.on_fulfill(marker.value!)
        when TailRec::Continue
          executor.post { yield(marker.value!) }
        else
          result.on_reject(TailRec::MarkerError.new(marker))
        end
      end

      public

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
      end
    end

    require "dry/monads/registry"
    register_mixin(:task, Task::Mixin)
  end
end
