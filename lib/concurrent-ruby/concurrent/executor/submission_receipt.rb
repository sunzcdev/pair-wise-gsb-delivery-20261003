require 'concurrent/errors'
require 'concurrent/synchronization/lockable_object'

module Concurrent

  # A receipt describing what happened to a task submitted with
  # {ExecutorService#post_with_receipt}.
  #
  # Unlike {ExecutorService#post}, which only reports whether a task was
  # accepted, a receipt keeps both the actual admission path and the terminal
  # outcome of the execution:
  #
  # * `admission` - how the task was taken in:
  #   * `:queued` - accepted by the pool (handed to an idle worker or put on
  #     the work queue) and executed by a pool thread;
  #   * `:caller_runs` - rejected by the pool and executed on the submitting
  #     thread by the `:caller_runs` fallback policy;
  #   * `:rejected` - not accepted and never executed (`:abort`/`:discard`).
  # * `status` - the execution outcome:
  #   * `:pending` - accepted, not finished yet;
  #   * `:completed` - the task finished without raising;
  #   * `:failed` - the task raised; the exception is available via {#error};
  #   * `:rejected` - the task will never run (also terminal).
  #
  # A receipt is safe to share between threads. {#wait} blocks until the
  # execution reaches a terminal state.
  #
  # @example
  #   receipt = pool.post_with_receipt(1, 2) { |a, b| a + b }
  #   receipt.wait(1)       #=> true
  #   receipt.admission     #=> :queued
  #   receipt.status        #=> :completed
  #   receipt.value         #=> 3
  class SubmissionReceipt < Synchronization::LockableObject
    safe_initialization!

    # The possible admission paths.
    ADMISSIONS = [:queued, :caller_runs, :rejected].freeze

    # The possible execution states.
    STATUSES = [:pending, :completed, :failed, :rejected].freeze

    def initialize
      super()
      synchronize do
        @admission = nil
        @status    = :pending
        @value     = nil
        @error     = nil
      end
    end

    # @return [Symbol, nil] the actual admission path, one of {ADMISSIONS}.
    def admission
      synchronize { @admission }
    end

    # @return [Symbol] the execution state, one of {STATUSES}.
    def status
      synchronize { @status }
    end

    # @return [Object, nil] the task's return value when it completed.
    def value
      synchronize { @value }
    end

    # @return [Exception, nil] the exception raised by the task or by the
    #   fallback path. For a `:rejected` receipt this is a
    #   {RejectedExecutionError} for the `:abort` policy and `nil` for
    #   `:discard`.
    def error
      synchronize { @error }
    end

    # @return [Boolean] true if the task was accepted onto the pool.
    def queued?
      synchronize { @admission == :queued }
    end

    # @return [Boolean] true if the task was executed by the submitting thread.
    def caller_runs?
      synchronize { @admission == :caller_runs }
    end

    # @return [Boolean] true if the task was not accepted and never executed.
    def rejected?
      synchronize { @admission == :rejected }
    end

    # @return [Boolean] true while the execution has not reached its final state.
    def pending?
      synchronize { !ns_finished? }
    end

    # @return [Boolean] true if the task finished without raising.
    def completed?
      synchronize { @status == :completed }
    end

    # @return [Boolean] true if the task raised an exception.
    def failed?
      synchronize { @status == :failed }
    end

    # @return [Boolean] true when no further state change can occur, that is
    #   when both the admission and the execution state are settled.
    def terminal?
      synchronize { ns_terminal? }
    end

    # Wait until the receipt is terminal.
    #
    # @param [Numeric, nil] timeout the maximum number of seconds to wait.
    #
    # @return [Boolean] true if a terminal state was reached, false on timeout.
    def wait(timeout = nil)
      synchronize { ns_wait_until(timeout) { ns_terminal? } }
    end

    def to_s
      synchronize do
        "#{self.class.name.split('::').last}(admission: #{@admission.inspect}, status: #{@status.inspect})"
      end
    end
    alias_method :inspect, :to_s

    # Records that the task was accepted onto the pool.
    #
    # @note Intended for executor implementations only.
    # @!visibility private
    def mark_queued!
      synchronize do
        @admission ||= :queued
        ns_broadcast if ns_terminal?
      end
      self
    end

    # Records that the task is executed by the submitting thread.
    #
    # @note Intended for executor implementations only.
    # @!visibility private
    def mark_caller_runs!
      synchronize do
        @admission ||= :caller_runs
        ns_broadcast if ns_terminal?
      end
      self
    end

    # Records that the task was rejected and will never be executed.
    #
    # @param [Exception, nil] error the reason reported by the fallback policy.
    #
    # @note Intended for executor implementations only.
    # @!visibility private
    def mark_rejected!(error = nil)
      synchronize do
        @admission ||= :rejected
        unless ns_finished?
          @status = :rejected
          @error  = error
          ns_broadcast
        end
      end
      self
    end

    # Records the successful completion of the task.
    #
    # @param [Object, nil] value the task's return value.
    #
    # @note Intended for executor implementations only.
    # @!visibility private
    def complete!(value = nil)
      synchronize do
        unless ns_finished?
          @status = :completed
          @value  = value
          ns_broadcast if ns_terminal?
        end
      end
      self
    end

    # Records that the task raised an exception.
    #
    # @param [Exception] error the raised exception.
    #
    # @note Intended for executor implementations only.
    # @!visibility private
    def fail!(error)
      synchronize do
        unless ns_finished?
          @status = :failed
          @error  = error
          ns_broadcast if ns_terminal?
        end
      end
      self
    end

    private

    # @return [Boolean] true once the execution reached its final state.
    def ns_finished?
      @status != :pending
    end

    # A receipt is terminal only when both the admission path and the
    # execution state are known, so a waiter never observes a half-written
    # receipt (a task may finish before the submitter records :queued).
    #
    # @return [Boolean]
    def ns_terminal?
      !@admission.nil? && ns_finished?
    end
  end
end
