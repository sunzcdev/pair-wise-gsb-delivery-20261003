# frozen_string_literal: true

module Dry
  module Transaction
    class StepAdapters
      # @api private
      class Around
        include Dry::Monads[:result]

        INVOCATION_KEY = :"#{name}#invocation"

        class StrictContinuation
          def initialize(step_name, continuation, invocation)
            @step_name = step_name
            @continuation = continuation
            @invocation = invocation
            @mutex = Mutex.new
            @state = :available
          end

          def call(*args)
            @mutex.synchronize do
              if @state == :expired
                raise InvalidContinuationError.new(
                  @step_name,
                  "is no longer available (invocation ##{@invocation})"
                )
              end

              assert_invocation

              if @state == :invoked
                raise InvalidContinuationError.new(
                  @step_name,
                  "can only be called once (invocation ##{@invocation})"
                )
              end

              @state = :invoked
            end

            @continuation.call(*args)
          end

          def expire!
            @mutex.synchronize { @state = :expired }
          end

          def to_proc
            method(:call).to_proc
          end

          private

          def assert_invocation
            current_invocation = Thread.current[INVOCATION_KEY]

            return if current_invocation == @invocation

            if current_invocation.nil?
              raise InvalidContinuationError.new(
                @step_name,
                "can only be called during invocation ##{@invocation}"
              )
            end

            raise InvalidContinuationError.new(
              @step_name,
              "belongs to invocation ##{@invocation}, not invocation ##{current_invocation}"
            )
          end
        end

        def initialize
          @mutex = Mutex.new
          @invocation_count = 0
        end

        def call(operation, options, args, &block)
          if options[:strict]
            call_strict(operation, options, args, block)
          else
            result = operation.(*args, &block)
            assert_result(result, options[:step_name])
          end
        end

        private

        def call_strict(operation, options, args, continuation)
          invocation = next_invocation
          strict_continuation = StrictContinuation.new(options[:step_name], continuation, invocation)
          previous_invocation = Thread.current[INVOCATION_KEY]

          begin
            Thread.current[INVOCATION_KEY] = invocation
            result = operation.(*args, &strict_continuation)
          ensure
            Thread.current[INVOCATION_KEY] = previous_invocation
            strict_continuation.expire!
          end

          assert_result(result, options[:step_name])
        end

        def next_invocation
          @mutex.synchronize { @invocation_count += 1 }
        end

        def assert_result(result, step_name)
          unless result.is_a?(Dry::Monads::Result)
            raise InvalidResultError, step_name
          end

          result
        end
      end

      register :around, Around.new
    end
  end
end
