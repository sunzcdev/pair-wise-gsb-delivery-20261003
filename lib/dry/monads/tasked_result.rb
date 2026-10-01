# frozen_string_literal: true

module Dry
  module Monads
    # TaskedResult combines the two effects of a Task yielding a Result:
    #
    #   Task<Result<Failure, Success>>
    #
    # Normally chaining such a value requires two levels of `bind`: one for
    # the async layer and one for the business layer. TaskedResult collapses
    # these two levels into a single object:
    #
    # * a {#bind}/{#fmap} callback receives the unwrapped *business* value
    #   (the content of a Success) and runs only when the task is fulfilled
    #   with a Success;
    # * a {#or}/{#or_fmap} callback recovers business Failures without ever
    #   seeing Task-level exceptions;
    # * exceptions raised asynchronously keep flowing through the Task error
    #   channel and can be recovered with {#or_exception}.
    #
    # The two channels never get mixed: Success(nil) is still a successful
    # computation carrying nil, it is never confused with "no result".
    #
    # @example basic chain
    #   include Dry::Monads[:task_result]
    #
    #   TaskResult { Success(fetch_user) }.
    #     bind { |user| TaskResult { Success(render(user)) } }.
    #     or   { |error| TaskResult.pure("fallback") }.
    #     to_task # => Task(Success("..."))
    #
    # @api public
    class TaskedResult
      # @return [Task] the underlying task, fulfilled with a Result
      attr_reader :task
      protected :task

      class << self
        # Runs the block asynchronously. Its value is normalised into a
        # Result: a Result is taken as is, any other value is wrapped in
        # Success. An exception raised by the block goes to the Task error
        # channel.
        #
        # @overload new(executor, &block)
        #   @param executor [Concurrent::AbstractExecutorService, Symbol]
        #   @param block [Proc] a block returning a Result or a plain value
        #   @return [TaskedResult]
        #
        # @overload new(&block)
        #   @param block [Proc]
        #   @return [TaskedResult]
        #
        # @overload new(result)
        #   Lifts an already built Result or a Task fulfilled with a Result.
        #   The task is not copied, its executor is preserved.
        #   @param result [Result, Task]
        #   @return [TaskedResult]
        def new(value = Undefined, &block)
          if block
            task = if Undefined.equal?(value)
                     Task.new { wrap_result(block.call) }
                   else
                     Task[value] { wrap_result(block.call) }
                   end
            from_task(task)
          elsif value.is_a?(Task)
            from_task(value)
          elsif value.is_a?(Result)
            from_task(Task.pure(value))
          else
            raise TypeError, "expected a Task or a Result, got #{value.class}"
          end
        end

        # Wraps a plain Task without copying it, so the original promise
        # and its executor are preserved. The task must be (or eventually
        # be) fulfilled with a Result.
        #
        # @param task [Task]
        # @return [TaskedResult]
        def from_task(task)
          unless task.is_a?(Task)
            raise TypeError, "expected Task, got #{task.class}"
          end

          allocate.tap { _1.send(:initialize, task) }
        end

        # A completed TaskedResult holding a Success(value).
        #
        # @overload pure(value)
        #   @param value [Object]
        #   @return [TaskedResult]
        #
        # @overload pure(&block)
        #   @param block [Proc]
        #   @return [TaskedResult]
        def pure(value = Undefined, &block)
          new(Result::Success.new(Undefined.default(value, &block)))
        end
        alias_method :success, :pure

        # A completed TaskedResult holding a Failure(error).
        #
        # @param error [Object]
        # @return [TaskedResult]
        def failure(error)
          new(Result::Failure.new(error))
        end

        # A rejected TaskedResult, the exception goes to the Task channel.
        #
        # @param exception [Exception]
        # @return [TaskedResult]
        def failed(exception)
          from_task(Task.failed(exception))
        end

        # @api private
        #
        # Lifts the value returned by a chain callback into a TaskedResult.
        # Accepted values are TaskedResult, Task (fulfilled with a Result),
        # Result and any plain value (wrapped in Success).
        def lift(value)
          case value
          when TaskedResult
            value
          when Task
            from_task(value)
          when Result
            new(value)
          else
            new(Result::Success.new(value))
          end
        end

        private

        # @api private
        def wrap_result(value)
          value.is_a?(Result) ? value : Result::Success.new(value)
        end
      end

      # @api private
      def initialize(task)
        @task = task
      end

      # Lifts the block over the business value. The block runs only when
      # the task is fulfilled with a Success. It may return a plain value
      # (wrapped in Success), a Result, a Task or another TaskedResult.
      # Returning a Failure routes the value through the failure channel,
      # raising routes it through the Task exception channel.
      #
      # @param block [Proc]
      # @return [TaskedResult]
      def fmap(&block)
        bind { |value| self.class.lift(block.(value)) }
      end

      # Composes two computations over the business value. The block runs
      # only on a fulfilled Success and may return a TaskedResult, Task,
      # Result or a plain value. Business Failures short-circuit the chain.
      #
      # @param block [Proc]
      # @return [TaskedResult]
      def bind(&block)
        chain do |result|
          if result.success?
            self.class.lift(block.(result.success)).to_task
          else
            Task.pure(result)
          end
        end
      end

      # Recovers a business Failure with another asynchronous computation.
      # Task-level exceptions are NOT passed to the block.
      #
      # @param block [Proc] receives the failure value
      # @return [TaskedResult]
      def or(&block)
        chain do |result|
          if result.failure?
            self.class.lift(block.(result.failure)).to_task
          else
            Task.pure(result)
          end
        end
      end

      # Recovers a business Failure by transforming its value into a Success.
      #
      # @param block [Proc]
      # @return [TaskedResult]
      def or_fmap(&block)
        self.or { |error| Result::Success.new(block.(error)) }
      end

      # Recovers the Task exception channel: an exception raised
      # asynchronously (or by a chain callback). Business Failures are not
      # passed to the block.
      #
      # @param block [Proc] receives the exception
      # @return [TaskedResult]
      def or_exception(&block)
        self.class.from_task(@task.or { |exception|
          self.class.lift(block.(exception)).to_task
        })
      end

      # Returns the underlying Task<Result>. The conversion is lossless:
      # the original promise and its executor are returned untouched.
      #
      # @return [Task]
      def to_task = @task

      # Blocks until the task completes and returns the carried Result.
      # Raises if the Task itself was rejected.
      #
      # @return [Result]
      def value! = @task.value!

      # Blocks until the task completes and returns the carried Result.
      # Raises if the Task itself was rejected.
      #
      # @return [Result]
      def to_result = @task.value!

      # Blocks the current thread until the task is complete.
      #
      # @return [TaskedResult]
      def wait(timeout = nil)
        @task.wait(timeout)
        self
      end

      # Whether the underlying computation is complete.
      #
      # @return [Boolean]
      def complete? = @task.complete?

      # @return [Class]
      def monad = TaskedResult

      # Returns self.
      #
      # @return [TaskedResult]
      def to_monad = self

      # @return [String]
      def inspect
        promise = @task.send(:promise)
        case promise.state
        when :fulfilled
          case (result = promise.value)
          when Result::Success
            "TaskedResult(Success(#{result.success.inspect}))"
          when Result::Failure
            "TaskedResult(Failure(#{result.failure.inspect}))"
          else
            "TaskedResult(value=#{result.inspect})"
          end
        when :rejected
          "TaskedResult(error=#{promise.reason.inspect})"
        else
          "TaskedResult(?)"
        end
      end
      alias_method :to_s, :inspect

      private

      # Feeds the asserted Result carried by the task into the block.
      # The block returns the next Task<Result>. A non-Result payload or a
      # raised error rejects the returned task via the Task channel.
      def chain
        self.class.from_task(@task.bind { |result|
          unless result.is_a?(Result)
            raise TypeError, "expected a Result, got #{result.class}"
          end

          yield(result)
        })
      end

      # TaskedResult constructors.
      module Mixin
        # @private
        TaskedResult = TaskedResult

        # Value constructors.
        module Constructors
          # Creates a TaskedResult from a block returning a Result or from an
          # existing Task/TaskedResult.
          #
          # @overload TaskResult(&block)
          #   @return [TaskedResult]
          #
          # @overload TaskResult(task)
          #   @param task [Task, TaskedResult]
          #   @return [TaskedResult]
          def TaskResult(value = Undefined, &block)
            if block
              TaskedResult.new(&block)
            elsif value.is_a?(TaskedResult)
              value
            elsif value.is_a?(Task)
              TaskedResult.from_task(value)
            else
              raise TypeError,
                    "TaskResult expects a block or a Task, got #{value.class}"
            end
          end
          private :TaskResult
        end

        include Constructors
      end
    end

    require "dry/monads/registry"
    register_mixin(:task_result, TaskedResult::Mixin)
  end
end
