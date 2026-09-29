# frozen_string_literal: true

module Dry
  module Transaction
    class StepAdapters
      # @api private
      class Around
        include Dry::Monads[:result]

        def call(operation, options, args, &block)
          continuation = Continuation.new(options[:step_name], block)

          result = begin
            operation.(*args, &continuation)
          ensure
            continuation.expire!
          end

          unless result.is_a?(Dry::Monads::Result)
            raise InvalidResultError, options[:step_name]
          end

          result
        end

        # A continuation that may only resume the steps following an
        # `around` step exactly once, and only while the `around` operation
        # is still running. State is isolated per adapter invocation, so a
        # continuation captured by one call can never be validated against
        # the state of another.
        #
        # @api private
        class Continuation
          ALREADY_CALLED = "has already been called; it can only be used once"
          NO_LONGER_ACTIVE =
            "can no longer be called after the around step has returned"

          private_constant :ALREADY_CALLED, :NO_LONGER_ACTIVE

          def initialize(step_name, block)
            @step_name = step_name
            @block = block
            @state = :active
            @mutex = Mutex.new
          end

          def call(result)
            block = @mutex.synchronize do
              case @state
              when :consumed
                raise InvalidContinuationError.new(@step_name, ALREADY_CALLED)
              when :expired
                raise InvalidContinuationError.new(@step_name, NO_LONGER_ACTIVE)
              end

              @state = :consumed
              @block
            end

            block.(result)
          end

          def expire!
            @mutex.synchronize { @state = :expired }
          end

          def to_proc
            method(:call).to_proc
          end
        end
      end

      register :around, Around.new
    end
  end
end
