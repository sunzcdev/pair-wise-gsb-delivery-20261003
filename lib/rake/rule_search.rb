# frozen_string_literal: true

require_relative "rule_recursion_overflow_error"

module Rake

  # Limits applied to the search for an implicit task through rules.
  #
  # Rake resolves a missing task by trying rules whose pattern matches the
  # task name.  A rule may recursively require another implicit task, so the
  # search is both deep and potentially wide.  RuleSearchBudget makes those
  # bounds explicit and configurable instead of relying solely on a hard
  # coded recursion limit.
  #
  #   Rake.application.rule_search_budget =
  #     Rake::RuleSearchBudget.new(max_depth: 16, max_candidates: 10_000)
  #
  # +max_depth+ is the maximum rule nesting level.  +max_candidates+ is the
  # maximum number of candidate sources examined while resolving a single
  # target.  Both limits must be positive integers.
  class RuleSearchBudget
    DEFAULT_MAX_DEPTH = 16
    DEFAULT_MAX_CANDIDATES = 10_000

    attr_reader :max_depth, :max_candidates

    def initialize(max_depth: DEFAULT_MAX_DEPTH,
                   max_candidates: DEFAULT_MAX_CANDIDATES)
      self.max_depth = max_depth
      self.max_candidates = max_candidates
    end

    def max_depth=(value)
      @max_depth = positive_integer!(:max_depth, value)
    end

    def max_candidates=(value)
      @max_candidates = positive_integer!(:max_candidates, value)
    end

    def to_h
      { max_depth: max_depth, max_candidates: max_candidates }
    end

    def ==(other)
      other.is_a?(RuleSearchBudget) &&
        other.max_depth == max_depth &&
        other.max_candidates == max_candidates
    end

    private

    def positive_integer!(name, value)
      number =
        case value
        when Integer
          value
        when String
          value =~ /\A\d+\z/ ? value.to_i : nil
        end
      unless number && number > 0
        fail ArgumentError, "#{name} must be a positive integer"
      end
      number
    end
  end

  # A single candidate source examined while resolving an implicit task.
  #
  # +outcome+ is +:accepted+ when the candidate can be used and +:rejected+
  # otherwise.  +reason+ explains the decision:
  #
  # [:existing_file]     the source already exists on disk
  # [:defined_task]      a task for the source is already defined
  # [:synthesized_rule]  the source was expanded by another rule
  # [:missing_candidate] the source is neither a file nor reachable by a rule
  # [:cycle]             the source is already on the current rule chain
  # [:budget_exhausted]  the candidate budget ran out at this candidate
  # [:depth_exceeded]    the rule nesting limit was reached
  class RuleSearchAttempt
    attr_reader :target, :rule, :source, :level, :outcome, :reason

    def initialize(target, rule, source, level, outcome, reason)
      @target = target.to_s
      @rule = rule
      @source = source
      @level = level
      @outcome = outcome
      @reason = reason
    end

    def accepted?
      outcome == :accepted
    end

    def rejected?
      outcome == :rejected
    end

    def to_h
      {
        target: target,
        rule: rule && rule.to_s,
        source: source,
        level: level,
        outcome: outcome,
        reason: reason,
      }
    end

    def to_s
      candidate = source ? "#{target} => #{source}" : target.dup
      via = rule ? " via #{rule}" : ""
      "#{candidate}#{via} (#{reason})"
    end
  end

  # Observation record for one implicit task lookup.
  #
  # The report collects every rule candidate that was examined together with
  # its rejection reason.  It also carries the diagnosis for cycles and for
  # exhausted budgets, and is available as
  # Rake.application.last_rule_search_report.
  #
  # A report belongs to a single lookup and records the rules revision it was
  # produced under.  It is never used to resolve a later lookup, so a result
  # produced under one rule configuration cannot be reused under another.
  class RuleSearchReport
    attr_reader :target, :budget, :attempts, :candidate_count,
                :rules_revision, :resolution_reason, :cycle

    # Whether the target itself exists on disk.  This is independent of the
    # rule plan: an undetermined plan can concern an existing file, and a
    # resolved rule plan can concern a file that has not been built yet.
    attr_accessor :target_file_exists

    def initialize(target, budget, rules_revision: 0)
      @target = target.to_s
      @budget = budget
      @rules_revision = rules_revision
      @attempts = []
      @candidate_count = 0
      @cycle = nil
      @target_file_exists = false
      @status = :pending
      @resolution_reason = nil
      @stack = []
    end

    def status
      @status
    end

    # True when the report was produced under a different rule revision than
    # +current_revision+.  Such a report must not be reused.
    def stale?(current_revision)
      rules_revision != current_revision
    end

    # An implicit task was found.  +reason+ is normally +:rule+,
    # +:existing_task+ or +:existing_file+.  An undetermined report is left
    # untouched.
    def resolved!(reason = :rule)
      return self if undetermined?
      @resolution_reason = reason
      @status = :resolved
      self
    end

    # No rule, task or file could resolve the target.  This is a proven
    # negative answer and not the same as an exhausted budget.
    def not_found!
      @status = :not_found if @status == :pending
      self
    end

    # The target itself was found as a plain file after the rules produced
    # no match.  This is the documented implicit file task case, so the
    # resolution is recorded even though no rule was involved.
    def resolved_by_file!
      return self if undetermined?
      @resolution_reason = :existing_file
      @status = :resolved
      @target_file_exists = true
      self
    end

    def mark_depth_exceeded!(target, rule = nil)
      @status = :depth_exceeded
      record_attempt(target, rule, nil, budget.max_depth, :rejected, :depth_exceeded)
      self
    end

    def resolved?
      @status == :resolved
    end

    def not_found?
      @status == :not_found
    end

    def budget_exhausted?
      @status == :budget_exhausted
    end

    def depth_exceeded?
      @status == :depth_exceeded
    end

    def cycle?
      !@cycle.nil?
    end

    # Make the detected cycle the outcome of the lookup.  This is called only
    # when no rule resolved the target, so a cycle seen on a rejected branch
    # does not mask a successful resolution.
    def mark_cycle!
      @status = :cycle if cycle?
      self
    end

    # True when the search stopped before it could decide whether the target
    # can be built.  Callers must not treat this as a missing file.
    def undetermined?
      case @status
      when :budget_exhausted, :depth_exceeded, :cycle
        true
      else
        false
      end
    end

    def record_attempt(target, rule, source, level, outcome, reason)
      attempt = RuleSearchAttempt.new(target, rule, source, level, outcome, reason)
      @attempts << attempt
      attempt
    end

    # Account for one candidate source.  Raises RuleSearchBudgetExceeded once
    # the candidate budget is used up.  The resolution is then undetermined;
    # it says nothing about whether the target file exists.
    def consume_candidate!(target, rule, source, level)
      @candidate_count += 1
      return if @candidate_count <= budget.max_candidates

      @status = :budget_exhausted
      record_attempt(target, rule, source, level, :rejected, :budget_exhausted)
      fail RuleSearchBudgetExceeded.new(self, target)
    end

    def cycle_target?(target)
      target = target.to_s
      @stack.any? { |entry| entry[:target] == target }
    end

    # Record that +source+ closes a cycle.  The chain keeps the files that
    # were already on the search path plus the edge that points back to the
    # repeated file.  +rule+ is the rule that produced +source+ from the
    # current target.
    def note_cycle!(source, rule)
      source = source.to_s
      index = @stack.index { |entry| entry[:target] == source } || 0
      chain = @stack[index..-1].map { |entry|
        { target: entry[:target], rule: entry[:rule], closing: false }
      }
      chain << { target: source, rule: rule, closing: true }
      @cycle ||= chain
      record_attempt(source, rule, source, @stack.size, :rejected, :cycle)
      self
    end

    # Run the block with +target+ on the current search chain.  +rule+ is the
    # rule edge that introduced the target (nil for the top level lookup).
    def enter(target, rule = nil)
      @stack.push(target: target.to_s, rule: rule)
      begin
        yield
      ensure
        @stack.pop
      end
    end

    # Current nesting level, derived from the live search chain.  A lookup
    # started from inside a rule dependent continues at this level so the
    # depth budget cannot be escaped through a proc.
    def current_level
      @stack.size
    end

    def accepted_attempts
      @attempts.select(&:accepted?)
    end

    def rejected_attempts
      @attempts.select(&:rejected?)
    end

    def attempted_rules
      @attempts.map(&:rule).compact.uniq
    end

    # Rejection reasons in the order they were first observed.
    def rejection_reasons
      rejected_attempts.map(&:reason).uniq
    end

    def cycle_description
      return nil unless cycle?
      cycle.map { |entry|
        rule = entry[:rule]
        rule ? "#{entry[:target]} (via #{rule})" : entry[:target].to_s
      }.join(" -> ")
    end

    # Compact human readable summary used to extend error messages.
    def diagnostic_suffix
      parts = []
      rules = attempted_rules
      parts << "Attempted rules: #{rules.join(", ")}" unless rules.empty?
      rejected = rejected_attempts.reject { |attempt| attempt.reason == :cycle }
      unless rejected.empty?
        parts << "Rejected candidates: #{rejected.map(&:to_s).join("; ")}"
      end
      parts << "Cycle: #{cycle_description}" if cycle?
      parts.empty? ? "" : " " + parts.join(". ") + "."
    end

    def to_h
      {
        target: target,
        status: status,
        resolution_reason: resolution_reason,
        rules_revision: rules_revision,
        target_file_exists: target_file_exists,
        candidate_count: candidate_count,
        max_candidates: budget.max_candidates,
        max_depth: budget.max_depth,
        undetermined: undetermined?,
        cycle: cycle_description,
        attempts: attempts.map(&:to_h),
      }
    end

    def to_s
      "#<#{self.class} target=#{target.inspect} status=#{status} " \
        "candidates=#{candidate_count}/#{budget.max_candidates} " \
        "depth_limit=#{budget.max_depth}>"
    end
  end

  # Raised when the candidate budget for implicit rule resolution is
  # exhausted.
  #
  # The search did not prove that the target is missing; it only ran out of
  # its inspection budget.  +report+ holds every attempted rule and the
  # rejection reasons.
  class RuleSearchBudgetExceeded < RuleRecursionOverflowError
    attr_reader :budget

    def initialize(report, target = nil)
      @budget = report.budget
      super(
        "Implicit rule search budget exhausted while resolving " \
        "'#{target || report.target}' " \
        "(#{report.candidate_count} candidate(s), " \
        "limit #{@budget.max_candidates}). " \
        "The plan is undetermined; this does not mean the file is missing. " \
        "Raise the budget or tighten the rule patterns, then retry."
      )
      self.report = report
    end

    def attempts
      report ? report.attempts : []
    end

    def rejection_reasons
      report ? report.rejection_reasons : []
    end
  end

  # Raised when implicit rule resolution walks into the same target twice.
  #
  # The message points at the actual file names and at the rule chain that
  # closed the cycle.  The resolution is undetermined rather than negative.
  class RuleCycleError < RuleRecursionOverflowError
    def initialize(report, target = nil)
      super(
        "Rule cycle detected while resolving " \
        "'#{target || report.target}'; the plan is undetermined"
      )
      self.report = report
    end

    def cycle
      report ? report.cycle : nil
    end

    def cycle_description
      report ? report.cycle_description : nil
    end
  end

end
