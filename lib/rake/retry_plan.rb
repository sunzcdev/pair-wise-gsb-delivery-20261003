# frozen_string_literal: true

module Rake

  # Base class for retry plan related errors.
  class RetryPlanError < StandardError; end

  # Raised when a retry plan no longer matches the application's
  # current retry plan version, e.g. a newer plan was built after
  # this one, or this plan was already executed.
  class StaleRetryPlanError < RetryPlanError; end

  # Raised when a retry plan is executed while it contains failed
  # nodes that may not be retried.  The message lists every rejected
  # node together with the reason it was rejected.
  class RejectedRetryNodesError < RetryPlanError; end

  ##
  # A RetryPlan is an explicit, inspectable description of how to
  # recover from a failed rake run.
  #
  # Simply reenabling a failed target is not enough: the invocation
  # exception cached in one of its failed dependencies would be
  # raised again on the next invocation.  A retry plan instead:
  #
  # * lists the failed nodes that must be re-admitted (+entries+),
  # * lists the targets affected by those failures (+targets+),
  # * never re-executes nodes that already succeeded and whose
  #   output is still valid (+skipped+),
  # * de-duplicates shared dependencies, so each failed node is
  #   retried exactly once,
  # * refuses to re-run side-effecting tasks unless they were
  #   explicitly declared retryable (+rejections+),
  # * preserves the historical failure trace of every task
  #   (see Rake::Task#failure_history),
  # * is versioned: only the most recently built plan for an
  #   application can be executed, and only once.
  #
  # A plan never clears global application state; it reenables only
  # the failed nodes it lists.
  #
  # Build a plan with Rake::Application#retry_plan and run it with
  # #execute.
  #
  class RetryPlan

    # A single failed node scheduled for re-admission, together with
    # the exception that made it fail.
    class Entry
      attr_reader :task, :failure

      def initialize(task, failure)
        @task = task
        @failure = failure
      end

      def name
        @task.name
      end
    end

    # Application this plan belongs to.
    attr_reader :application

    # Plan version.  Must match the application's current retry plan
    # version at execution time.
    attr_reader :version

    # Top level targets affected by at least one failure.
    attr_reader :targets

    # Failed nodes that will be re-admitted (list of Entry).  Shared
    # dependencies appear exactly once.
    attr_reader :entries

    # Failed nodes that may not be retried: { task_name => reason }.
    attr_reader :rejections

    # Nodes that already succeeded; they will not be executed again.
    attr_reader :skipped

    def initialize(application, target_names, version) # :nodoc:
      @application = application
      @version = version
      @targets = []
      @entries = []
      @rejections = {}
      @skipped = []
      @executed = false
      build(target_names)
    end

    # True if every failed node may be retried.
    def executable?
      @rejections.empty?
    end

    # Has this plan already been executed?
    def executed?
      @executed
    end

    # Names of the failed nodes that will be retried.
    def retryable_task_names
      @entries.map(&:name)
    end

    # Execute the plan: re-admit each failed node exactly once and
    # re-invoke the affected targets.  Nodes that already succeeded
    # are left untouched, so their (still valid) output is reused.
    #
    # Raises StaleRetryPlanError if this plan is not the current
    # plan version for the application, or was already executed.
    # Raises RejectedRetryNodesError if the plan contains nodes
    # that may not be retried.
    def execute
      if @executed || application.retry_plan_version != version
        raise StaleRetryPlanError,
          "retry plan version #{version} is stale " \
          "(current version is #{application.retry_plan_version})"
      end
      unless rejections.empty?
        raise RejectedRetryNodesError, rejection_message
      end

      @executed = true
      entries.each { |entry| entry.task.reenable }
      targets.each(&:invoke)
      application.invalidate_retry_plans!
      true
    end

    # Human readable summary of the plan.
    def summary
      lines = ["Retry plan v#{version}:"]
      lines << "  affected targets: #{target_names.join(', ')}"
      lines << "  re-admit:         #{retryable_task_names.join(', ')}"
      lines << "  skipped (ok):     #{@skipped.map(&:name).join(', ')}"
      rejections.each do |name, reason|
        lines << "  rejected:         #{name} (#{reason})"
      end
      lines.join("\n")
    end

    private

    def target_names
      @targets.map(&:name)
    end

    def build(target_names)
      seen = {}
      target_names.each do |name|
        target = application[name]
        nodes = [target] + target.all_prerequisite_tasks
        next unless nodes.any?(&:failed?)

        @targets << target
        nodes.each do |node|
          next if seen[node.name]
          seen[node.name] = true
          admit(node)
        end
      end
    end

    def admit(node)
      if !node.failed?
        @skipped << node
      elsif node.retryable?
        @entries << Entry.new(node, node.invocation_exception)
      else
        @rejections[node.name] = node.rejection_reason
      end
    end

    def rejection_message
      lines = ["retry plan contains nodes that may not be retried:"]
      rejections.each do |name, reason|
        lines << "  #{name}: #{reason}"
      end
      lines.join("\n")
    end
  end
end
