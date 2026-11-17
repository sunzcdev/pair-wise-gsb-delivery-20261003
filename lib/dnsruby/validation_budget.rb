require 'dnsruby/query_control'

module Dnsruby
  # Resource limits are verification failures, never evidence that a zone is
  # insecure. Keep this distinction through trust-anchor and algorithm fallbacks.
  class ValidationLimitError < VerifyError; end
  class ValidationCancelled < ValidationLimitError; end

  # One budget counts DNSSEC work across nested validation calls. Supporting
  # lookups share its deadline and cancellation signal through QueryControl.
  class ValidationBudget # :nodoc: all
    # Ordinary verification and lookup failures can select another key or
    # anchor. Exhaustion must cross those fallbacks unchanged, then become the
    # public verification error when the outermost validation returns.
    class Abort < StandardError
      attr_reader :public_error

      def initialize(public_error)
        @public_error = public_error
        super(public_error.message)
      end
    end
    private_constant :Abort

    DEFAULT_LIMITS = {
      max_signature_verifications: 64,
      max_work: 100_000,
      timeout: 5.0
    }.freeze

    # One recorded unit of validation work: a derived lookup, its reuse, or a
    # signature verification. Entries are frozen so a completed validation
    # graph can be inspected without retaining any mutable operation state.
    Charge = Struct.new(:kind, :detail, :work)

    # Work charged for one derived lookup (DS/DNSKEY/DLV). Repeating an
    # already completed lookup reuses that dependency and charges nothing.
    DERIVED_QUERY_WORK = 100
    # The ledger is diagnostic; bound it so a hostile graph cannot grow it
    # without limit. Counters in the summary remain exact past the cap.
    MAX_LEDGER_ENTRIES = 4096

    attr_reader :control, :transaction

    def self.current
      Thread.current[:dnsruby_validation_budget]
    end

    # Supporting lookup configuration has legacy catch-all fallbacks. None
    # may convert an enclosing operation's exhaustion into an ordinary failure.
    def self.abort?(error)
      error.is_a?(Abort) || error.is_a?(ValidationLimitError) ||
        error.is_a?(QueryControl::Stopped) || error.is_a?(RecursionLimitError)
    end

    def self.with(budget = nil)
      previous = current
      budget = previous || budget || new
      Thread.current[:dnsruby_validation_budget] = budget
      QueryControl.with(budget.control) do
        budget.check!
        result = yield budget
        budget.check!
        result
      end
    rescue Abort, QueryControl::Stopped => error
      raise if previous
      raise public_error(error), cause: error
    ensure
      Thread.current[:dnsruby_validation_budget] = previous
    end

    def initialize(deadline: nil, limits: Dnssec.validation_limits, parent: nil)
      # A plain token can be retained with learned records without retaining
      # the budget, resolver control, or the rest of a validation operation.
      if parent
        @transaction = parent.transaction
        @limits = parent.limits
        @state = parent.state
        @control = QueryControl.new(deadline: deadline, parent: parent.control)
      else
        @transaction = Object.new.freeze
        @limits = limits
        deadline = [deadline, TransportClock.now + limits[:timeout]].compact.min
        @control = QueryControl.new(deadline: deadline, parent: QueryControl.current)
        @state = { mutex: Mutex.new, signature_verifications: 0, work: 0,
          queries: {}, reused_queries: 0, ledger: [], ledger_overflow: 0 }
      end
    end

    # A selector cancels its completed hop. Keep that signal local while all
    # hops and parallel address workers count against the lookup's one budget.
    def child(deadline: nil)
      self.class.new(deadline: deadline, parent: self)
    end

    def deadline
      @control.deadline
    end

    def cancel
      @control.cancel
    end

    def check!(work = 0)
      @control.check!
      @state[:mutex].synchronize do
        @state[:work] += work
        check_counts!
      end
      true
    rescue QueryControl::Stopped => error
      raise if self.class.current
      raise self.class.public_error(error), cause: error
    end

    def self.public_error(error)
      public_error = case error
      when Abort then error.public_error
      when QueryControl::Cancelled then ValidationCancelled.new('DNSSEC validation cancelled')
      else ValidationLimitError.new('DNSSEC validation deadline exceeded')
      end
      public_error.set_backtrace(error.backtrace)
      public_error
    end

    def fail_with(error_class, message)
      error = error_class.new(message)
      raise(self.class.current ? Abort.new(error) : error)
    end
    private :fail_with

    def check_counts!
      if @state[:signature_verifications] > @limits[:max_signature_verifications]
        fail_with(ValidationLimitError, 'DNSSEC signature verification limit exceeded')
      end
      if @state[:work] > @limits[:max_work]
        fail_with(ValidationLimitError, 'DNSSEC validation work limit exceeded')
      end
    end
    private :check_counts!

    def signature!(detail = nil)
      check!
      @state[:mutex].synchronize do
        @state[:signature_verifications] += 1
        record(:signature, detail, 0)
        check_counts!
      end
      true
    end

    # Charge a lookup derived from this validation (DS, DNSKEY, DLV, ...).
    # The first occurrence of each owner/type is a real network request and
    # counts against the work limit. A repeat reuses the completed dependency:
    # it is recorded as a reuse and never charged as another network request.
    # Returns true when the charge was for a fresh lookup.
    def charge_query(name, type)
      check!
      owner = name.is_a?(Name) ? name : Name.create(name)
      code = type.is_a?(Types) ? type.code : Types.to_code(type)
      identity = [owner.canonical, code]
      detail = "#{Types.to_string(code)} #{owner.to_s(true).downcase}"
      fresh = false
      @state[:mutex].synchronize do
        fresh = !@state[:queries].key?(identity)
        @state[:work] += DERIVED_QUERY_WORK if fresh
        check_counts!
        @state[:queries][identity] = true if fresh
        @state[:reused_queries] += 1 unless fresh
        record(fresh ? :query : :reused_query, detail, fresh ? DERIVED_QUERY_WORK : 0)
      end
      fresh
    end

    # Inspectable breakdown of everything this validation graph was charged,
    # shared with every child budget. Remains readable after exhaustion or
    # cancellation so an incomplete validation can report what it attempted.
    def charges
      @state[:mutex].synchronize { @state[:ledger].dup.freeze }
    end

    # Exact counters for the whole graph, independent of the ledger cap.
    def summary
      @state[:mutex].synchronize do
        {
          work: @state[:work],
          signature_verifications: @state[:signature_verifications],
          derived_queries: @state[:queries].length,
          reused_queries: @state[:reused_queries],
          recorded_charges: @state[:ledger].length,
          unrecorded_charges: @state[:ledger_overflow]
        }.freeze
      end
    end

    protected

    attr_reader :limits, :state

    private

    # Caller holds the state mutex.
    def record(kind, detail, work)
      if @state[:ledger].length < MAX_LEDGER_ENTRIES
        @state[:ledger] << Charge.new(kind, detail, work).freeze
      else
        @state[:ledger_overflow] += 1
      end
    end
  end
end
