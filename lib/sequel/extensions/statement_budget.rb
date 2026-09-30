# frozen_string_literal: true
#
# The statement_budget extension adds support for limiting the number
# of business statements sent to the adapter inside a transaction:
#
#   DB.extension :statement_budget
#   DB.transaction(statement_budget: 5) do
#     # At most 5 business statements may be executed in here.
#   end
#
# The budget is owned by the outermost transaction. Nested transactions
# and savepoints share the remaining allowance instead of receiving their
# own budget:
#
#   DB.transaction(statement_budget: 5) do
#     DB.transaction(savepoint: true) do
#       # Uses the same budget as the enclosing transaction.
#     end
#   end
#
# Only business statements actually sent to the adapter (SELECT/INSERT/
# UPDATE/DELETE/DDL, ...) are counted. Transaction control and cleanup
# statements such as BEGIN, COMMIT, ROLLBACK, SAVEPOINT, and RELEASE
# SAVEPOINT are not counted, so they always run even after the budget is
# exhausted. Once the budget is used up, any further business statement
# raises a Sequel::StatementBudget::StatementBudgetExceeded exception,
# which rolls back the transaction.
#
# Statements executed outside of a transaction that has a budget are not
# affected.
#
# Related module: Sequel::StatementBudget

require 'rbconfig'

module Sequel
  module StatementBudget
    SEQUEL_LIB_PATH = (File.expand_path('../../..', __FILE__) + '/').freeze
    RUBY_STDLIB = RbConfig::CONFIG["rubylibdir"]
    INTERNAL = '<internal'

    # Exception raised when a business statement is attempted after the
    # transaction statement budget has been exhausted.
    class StatementBudgetExceeded < Sequel::Error
      # The configured statement budget for the transaction.
      attr_reader :statement_budget

      # The number of business statements already used when this was raised.
      attr_reader :statements_used

      # The first non-Sequel caller location that triggered the statement.
      attr_reader :trigger_location

      def initialize(statement_budget, statements_used, trigger_location)
        @statement_budget = statement_budget
        @statements_used = statements_used
        @trigger_location = trigger_location

        super("transaction statement budget exhausted: #{statements_used} of #{statement_budget} business statements used (triggered at #{trigger_location})")
      end
    end

    def self.extended(db)
      db.instance_exec do
        @statement_budget_skip_key = :"statement_budget_skip_#{object_id}"
      end
    end

    # Validate the budget option before a transaction is opened, so an invalid
    # value does not leave transaction bookkeeping behind.
    def transaction(opts=OPTS, &block)
      if opts.has_key?(:statement_budget)
        budget = opts[:statement_budget]
        unless budget.is_a?(Integer) && budget >= 0
          raise Sequel::Error, ":statement_budget transaction option must be a nonnegative integer, #{budget.inspect} given"
        end
      end
      super
    end

    private

    # Store the budget on the outermost transaction hash so that nested
    # transactions and savepoints share the same counter.
    def transaction_options(conn, opts)
      h = super

      unless _trans(conn)
        if opts.has_key?(:statement_budget)
          h[:statement_budget] = opts[:statement_budget]
          h[:statements_used] = 0
        end
      end

      h
    end

    # Transaction control statements are exempt from the budget.
    def begin_transaction(conn, opts=OPTS)
      with_statement_budget_skipped{super}
    end

    # Transaction control statements are exempt from the budget.
    def commit_transaction(conn, opts=OPTS)
      with_statement_budget_skipped{super}
    end

    # Transaction control statements are exempt from the budget, so rolling
    # back (including to a savepoint) always works after exhaustion.
    def rollback_transaction(conn, opts=OPTS)
      with_statement_budget_skipped{super}
    end

    # Count business statements before they are sent to the adapter. Every
    # adapter executes queries through this method; statements issued by the
    # transaction control methods are marked as skipped.
    def log_connection_yield(sql, conn, args=nil)
      check_statement_budget!(conn) unless statement_budget_skipped?
      super
    end

    # Run the block with statement budget counting disabled. Uses a counter
    # per thread so nested exemption regions (e.g. a savepoint inside a
    # transaction begin) stay balanced.
    def with_statement_budget_skipped
      key = @statement_budget_skip_key
      h = Thread.current
      h[key] = h[key].to_i + 1
      begin
        yield
      ensure
        level = h[key].to_i - 1
        h[key] = level > 0 ? level : nil
      end
    end

    # Whether budget counting is currently disabled in this thread.
    def statement_budget_skipped?
      Thread.current[@statement_budget_skip_key].to_i > 0
    end

    # Raise if the current transaction has consumed its statement budget,
    # otherwise reserve one slot for the statement being executed.
    def check_statement_budget!(conn)
      return unless h = _trans(conn)
      budget = h[:statement_budget]
      return unless budget

      used = nil
      Sequel.synchronize do
        used = h[:statements_used]
        if used >= budget
          location = external_statement_budget_caller
          raise StatementBudgetExceeded.new(budget, used, location)
        end

        h[:statements_used] = used + 1
      end
    end

    # The first caller location outside of Sequel and Ruby's standard
    # library, so the reported trigger points at application code. Falls
    # back to the nearest caller if every frame is internal.
    def external_statement_budget_caller
      c = caller.find do |line|
        !line.start_with?(SEQUEL_LIB_PATH, RUBY_STDLIB, INTERNAL)
      end
      c || caller.first
    end
  end

  Database.register_extension(:statement_budget, StatementBudget)
end
