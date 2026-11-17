module Concurrent

  Error = Class.new(StandardError)

  # Raised when errors occur during configuration.
  ConfigurationError = Class.new(Error)

  # Raised when an asynchronous operation is cancelled before execution.
  CancelledOperationError = Class.new(Error)

  # Raised when a lifecycle method (such as `stop`) is called in an improper
  # sequence or when the object is in an inappropriate state.
  LifecycleError = Class.new(Error)

  # Raised when an attempt is made to violate an immutability guarantee.
  ImmutabilityError = Class.new(Error)

  # Raised when an operation is attempted which is not legal given the
  # receiver's current state
  IllegalOperationError = Class.new(Error)

  # Raised when an object's methods are called when it has not been
  # properly initialized.
  InitializationError = Class.new(Error)

  # Raised when an object with a start/stop lifecycle has been started an
  # excessive number of times. Often used in conjunction with a restart
  # policy or strategy.
  MaxRestartFrequencyError = Class.new(Error)

  # Raised when an attempt is made to modify an immutable object
  # (such as an `IVar`) after its final state has been set.
  class MultipleAssignmentError < Error
    attr_reader :inspection_data

    def initialize(message = nil, inspection_data = nil)
      @inspection_data = inspection_data
      super message
    end

    def inspect
      format '%s %s>', super[0..-2], @inspection_data.inspect
    end
  end

  # Raised by an `Executor` when it is unable to process a given task,
  # possibly because of a reject policy or other internal error.
  RejectedExecutionError = Class.new(Error)

  # Raised when any finite resource, such as a lock counter, exceeds its
  # maximum limit/threshold.
  ResourceLimitError = Class.new(Error)

  # Raised when an operation times out.
  TimeoutError = Class.new(Error)

  # Raised by a dataflow node whose dependencies were rejected, when the node's
  # block was therefore never executed.
  #
  # The exception identifies the failed dependencies by their position among the
  # node's inputs, in that same (stable) order, so a failed node is never
  # mistaken for a node whose own block raised. The dependencies' exceptions are
  # available via {#reasons}, and the complete account of every dependency
  # (including dependencies that fulfilled with a `nil` value) via
  # {#explanation}.
  #
  # @see Concurrent::DataflowExplanation
  class DataflowDependencyError < Error
    # @return [Concurrent::DataflowExplanation] the dependency account that
    #   produced this error
    attr_reader :explanation

    # @param [Concurrent::DataflowExplanation] explanation the dependency account
    def initialize(explanation)
      @explanation = explanation
      super(build_message(explanation))
    end

    # @return [Array<Concurrent::DataflowExplanation::Dependency>] the failed
    #   dependencies, ordered by their position among the inputs
    def failed_dependencies
      @explanation.failed_dependencies
    end

    # @return [Array<Integer>] the inputs' positions of the failed dependencies,
    #   in input order
    def failed_indexes
      @explanation.failed_indexes
    end

    # @return [Array<Exception>] the dependencies' exceptions, in input order
    def reasons
      failed_dependencies.map(&:reason)
    end

    private

    # @!visibility private
    def build_message(explanation)
      failed    = explanation.failed_dependencies
      indexes   = explanation.failed_indexes
      subject   = indexes.size == 1 ? "dependency at index #{indexes.first}" :
                                    "dependencies at indexes #{indexes.join(', ')}"
      details   = failed.map do |dependency|
        if dependency.reason.nil?
          format('index %d: no reason given', dependency.index)
        else
          format('index %d: %s: %s', dependency.index, dependency.reason.class, dependency.reason.message)
        end
      end
      format('dataflow %s failed, so the dataflow block was not executed (%s)',
             subject, details.join('; '))
    end
  end

  # Aggregates multiple exceptions.
  class MultipleErrors < Error
    attr_reader :errors

    def initialize(errors, message = "#{errors.size} errors")
      @errors = errors
      super [*message,
             *errors.map { |e| [format('%s (%s)', e.message, e.class), *e.backtrace] }.flatten(1)
            ].join("\n")
    end
  end

  # @!macro internal_implementation_note
  class ConcurrentUpdateError < ThreadError
    # frozen pre-allocated backtrace to speed ConcurrentUpdateError
    CONC_UP_ERR_BACKTRACE = ['backtrace elided; set verbose to enable'].freeze
  end
end
