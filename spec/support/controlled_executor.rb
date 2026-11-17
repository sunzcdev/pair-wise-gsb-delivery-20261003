require 'concurrent/atomic/atomic_fixnum'
require 'concurrent/executor/executor_service'

module Concurrent
  # A deterministic executor for testing cancellation graphs.
  #
  # It records exactly how many tasks were *accepted* (`#post` calls, i.e.
  # scheduled on a node) and how many were *executed* (the block actually
  # ran), both globally and per label supplied by the test. A gate keeps the
  # accepted tasks blocked until the test releases it, allowing assertions on
  # a graph where every node has been accepted but none has completed.
  class ControlledExecutor
    include Concurrent::ExecutorService

    # @return [Hash(Object, AtomicFixnum)] accepted count keyed by label
    attr_reader :accepted_by_label
    # @return [Hash(Object, AtomicFixnum)] executed count keyed by label
    attr_reader :executed_by_label

    def initialize
      @accepted          = AtomicFixnum.new 0
      @executed          = AtomicFixnum.new 0
      @accepted_by_label = Hash.new { |hash, key| hash[key] = AtomicFixnum.new 0 }
      @executed_by_label = Hash.new { |hash, key| hash[key] = AtomicFixnum.new 0 }
      @threads           = []
      @threads_mutex     = Mutex.new
      @gate              = Mutex.new
      @open              = false
      @condition         = ConditionVariable.new
    end

    # Blocks until the given number of tasks has been accepted.
    def wait_for_accepted(count, timeout = 5)
      deadline = Time.now.to_f + timeout
      sleep 0.001 while @accepted.value < count && Time.now.to_f < deadline
      @accepted.value >= count
    end

    # Releases all blocked tasks to run and joins their threads, so no executor
    # thread leaks across examples.
    def release
      @gate.synchronize do
        @open = true
        @condition.broadcast
      end
      join_threads
      self
    end

    # Waits until all accepted tasks have executed.
    def wait_for_idle(timeout = 5)
      deadline = Time.now.to_f + timeout
      sleep 0.001 while @executed.value < @accepted.value && Time.now.to_f < deadline
      join_threads(timeout)
      @executed.value == @accepted.value
    end

    def accepted_count
      @accepted.value
    end

    def executed_count
      @executed.value
    end

    # The first positional argument is an optional test label used to count
    # acceptances and executions per graph node.
    def post(*args, &task)
      raise ArgumentError, 'no block given' unless task
      label = args.first
      @accepted.increment
      @accepted_by_label[label].increment
      thread = Thread.new do
        @gate.synchronize do
          @condition.wait(@gate) until @open
        end
        begin
          task.call
        ensure
          @executed.increment
          @executed_by_label[label].increment
        end
      end
      @threads_mutex.synchronize { @threads.push thread }
      true
    end

    # Joins all worker threads so none outlives the example using the executor.
    def join_threads(timeout = 5)
      threads = @threads_mutex.synchronize { @threads.dup }
      threads.each { |thread| thread.join(timeout) }
    end

    def can_overflow?
      false
    end

    def serialized?
      false
    end

    def auto_terminate?
      true
    end

    def auto_terminate=(_value)
      true
    end

    def kill
      release
    end

    def shutdown
      release
    end

    def wait_for_termination(_timeout = nil)
      true
    end

    def running?
      true
    end

    def shuttingdown?
      false
    end

    def shutdown?
      false
    end

    # Returns an executor service which forwards every posted task to this
    # controlled executor tagged with the given label, allowing per-node
    # acceptance and execution counts without changing production call sites.
    def labeled(label)
      LabeledExecutor.new self, label
    end

    # Forwards tasks to a {ControlledExecutor} under a fixed label.
    class LabeledExecutor
      include Concurrent::ExecutorService

      def initialize(controlled, label)
        @controlled = controlled
        @label      = label
      end

      def post(*_args, &task)
        @controlled.post(@label, &task)
      end
    end
  end
end