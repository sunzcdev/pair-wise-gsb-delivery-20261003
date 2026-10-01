# frozen_string_literal: true

module Dry
  module Monads
    # A combinator for the two-layered `Task<Result>` effect.
    #
    # Asynchronous APIs that return a `Task` of `Success`/`Failure`
    # normally force every step to bind through both layers by hand.
    # TaskResult fuses the two layers into a single chainable object:
    #
    #   * `#bind`/`#fmap` hand the _business value_ (the payload of
    #     `Success`) to the callback, skipping both layers at once.
    #   * `#or`/`#or_fmap` recover _business failures_ (`Failure`)
    #     without touching the Task layer.
    #   * The Task layer's own exception channel (a rejected promise)
    #     is preserved as-is: it never gets confused with a `Failure`
    #     and keeps propagating unless the underlying Task is rescued.
    #
    # Dispatch is done on the `Result` type itself, never on the
    # payload, so `Success(nil)` is still a success and is not
    # confused with "no result".
    #
    # Callbacks may return a TaskResult, a `Task<Result>`, or a plain
    # `Result`. A returned `Failure` is reported through the business
    # failure channel and a raised exception through the Task channel;
    # either way the underlying promise is always settled, so waiters
    # are never left hanging.
    #
    # Every derived computation reuses the executor of the original
    # Task (via `Promise#then`/`#flat_map`), and `#to_task` converts
    # back to the plain `Task<Result>` losslessly.
    #
    # @example
    #   TaskResult[:io] { http_get("/user") }     # => TaskResult
    #     .bind { |user| TaskResult { validate(user) } }
    #     .fmap { |user| user.name }
    #     .or_fmap { |err| "fallback (#{err})" }
    #     .to_task                               # => Task<Result>
    #
    # @api public
    class TaskResult
      # @api private
      attr_reader :task
      protected :task

      class << self
        # Wraps an existing `Task<Result>`, or builds one from a block.
        #
        # @overload new(task)
        #   @param task [Task] a task producing a Result
        #   @return [TaskResult]
        #
        # @overload new(&block)
        #   @param block [Proc] a computation returning a Result,
        #     run as a Task on the default executor
        #   @return [TaskResult]
        def new(task = nil, &block)
          if task
            super(task)
          else
            super(Task.new(&block))
          end
        end

        # Builds a TaskResult with the given executor.
        # The block is expected to return a Result.
        #
        # @param executor [Concurrent::AbstractExecutorService,Symbol]
        # @param block [Proc] a computation returning a Result
        # @return [TaskResult]
        def [](executor, &) = new(Task[executor, &])

        # Lifts a raw value into a successful TaskResult.
        #
        # @overload pure(value)
        #   @param value [Object]
        #   @return [TaskResult]
        #
        # @overload pure(&block)
        #   @param block [Proc]
        #   @return [TaskResult]
        def pure(value = Undefined, &block)
          new(Task.pure(Result::Success.new(Undefined.default(value, block))))
        end

        # Builds an already-resolved business failure.
        #
        # @param value [Object] the failure payload
        # @return [TaskResult]
        def failure(value = Undefined, &block)
          v = Undefined.default(value, block || Unit)
          new(Task.pure(Result::Failure.new(v, RightBiased::Left.trace_caller)))
        end

        # Builds a TaskResult whose Task layer has already failed
        # with the given exception.
        #
        # @param exception [Exception]
        # @return [TaskResult]
        def failed(exception) = new(Task.failed(exception))
      end

      # @api private
      def initialize(task)
        @task = task
      end

      # Composes two-layered computations. The block receives the
      # business value of a `Success` and may return a TaskResult,
      # a `Task<Result>`, or a plain `Result`. Returning a `Failure`
      # (or raising) short-circuits the rest of the chain through the
      # corresponding channel.
      #
      # @param block [Proc]
      # @return [TaskResult]
      def bind(&block)
        self.class.new(
          @task.bind do |result|
            case result
            when Result::Success
              coerce(block.(result.value!)).task
            when Result::Failure
              Task.pure(result)
            else
              raise TypeError, "expected a Task of Result, got Task(#{result.class})"
            end
          end
        )
      end

      # Maps the business value of a `Success`, leaving `Failure`
      # and Task-level errors untouched.
      #
      # @param block [Proc]
      # @return [TaskResult]
      def fmap(&block)
        self.class.new(
          @task.fmap do |result|
            case result
            when Result::Success
              Result::Success.new(block.(result.value!))
            when Result::Failure
              result
            else
              raise TypeError, "expected a Task of Result, got Task(#{result.class})"
            end
          end
        )
      end

      # Recovers a business failure. The block receives the `Failure`
      # payload and may return a TaskResult, a `Task<Result>`, or a
      # plain `Result`. Task-level exceptions bypass the block and
      # keep propagating through the Task channel.
      #
      # @param block [Proc]
      # @return [TaskResult]
      def or(&block)
        self.class.new(
          @task.bind do |result|
            case result
            when Result::Failure
              coerce(block.(result.failure)).task
            when Result::Success
              Task.pure(result)
            else
              raise TypeError, "expected a Task of Result, got Task(#{result.class})"
            end
          end
        )
      end

      # Maps a business failure back to a success. Task-level
      # exceptions are not affected.
      #
      # @param block [Proc]
      # @return [TaskResult]
      def or_fmap(&block)
        self.class.new(
          @task.fmap do |result|
            case result
            when Result::Failure
              Result::Success.new(block.(result.failure))
            when Result::Success
              result
            else
              raise TypeError, "expected a Task of Result, got Task(#{result.class})"
            end
          end
        )
      end

      # Converts back to the underlying `Task<Result>`. This is
      # lossless: the very same Task (and hence the same executor)
      # is returned.
      #
      # @return [Task]
      def to_task = @task

      # Returns self.
      #
      # @return [TaskResult]
      def to_monad = self

      # @return [Class]
      def monad = TaskResult

      # Blocks the current thread until the computation completes
      # and returns the `Result`. Reraises the exception if the Task
      # layer failed.
      #
      # @return [Result::Success, Result::Failure]
      def value! = @task.value!

      # Blocks the current thread until the computation is complete.
      #
      # @param timeout [Numeric, nil]
      # @return [TaskResult]
      def wait(timeout = nil)
        @task.wait(timeout)
        self
      end

      # Whether the computation is complete.
      #
      # @return [Boolean]
      def complete? = @task.complete?

      # Compares two task results. Only reliable for complete tasks.
      #
      # @return [Boolean]
      def ==(other)
        other.is_a?(TaskResult) && @task == other.task
      end

      # @return [String]
      def to_s = @task.to_s.sub(/\ATask\(/, "TaskResult(")
      alias_method :inspect, :to_s

      private

      # Normalizes a callback result to a TaskResult so that plain
      # Results (including `Failure`) and Tasks are reported through
      # the proper channel instead of breaking the chain.
      #
      # @api private
      def coerce(value)
        case value
        when TaskResult then value
        when Task then self.class.new(value)
        when Result then self.class.new(Task.pure(value))
        else
          raise TypeError,
                "callback must return a TaskResult, Task, or Result, got #{value.class}"
        end
      end
    end

    class Task
      # Wraps a `Task<Result>` into the two-layer combinator.
      #
      # @return [TaskResult]
      def to_task_result = TaskResult.new(self)
    end
  end
end
