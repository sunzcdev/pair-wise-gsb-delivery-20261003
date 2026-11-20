# frozen_string_literal: true

module ActiveRecord
  module Locking
    # \Pessimistic \Locking
    # =====================
    #
    # Locking::Pessimistic provides support for row-level locking using
    # SELECT ... FOR UPDATE and other lock types.
    #
    # Chain `ActiveRecord::Base#find` to ActiveRecord::QueryMethods#lock to obtain an exclusive
    # lock on the selected rows:
    #
    # ```
    # # select * from accounts where id=1 for update
    # Account.lock.find(1)
    # ```
    #
    # Call `lock('some locking clause')` to use a database-specific locking clause
    # of your own such as 'LOCK IN SHARE MODE' or 'FOR UPDATE NOWAIT'. Example:
    #
    # ```
    # Account.transaction do
    #   # select * from accounts where name = 'shugo' limit 1 for update nowait
    #   shugo = Account.lock("FOR UPDATE NOWAIT").find_by(name: "shugo")
    #   yuko = Account.lock("FOR UPDATE NOWAIT").find_by(name: "yuko")
    #   shugo.balance -= 100
    #   shugo.save!
    #   yuko.balance += 100
    #   yuko.save!
    # end
    # ```
    #
    # You can also use `ActiveRecord::Base#lock!` method to lock one record by id.
    # This may be better if you don't need to lock every row. Example:
    #
    # ```
    # Account.transaction do
    #   # select * from accounts where ...
    #   accounts = Account.where(...)
    #   account1 = accounts.detect { |account| ... }
    #   account2 = accounts.detect { |account| ... }
    #   # select * from accounts where id=? for update
    #   account1.lock!
    #   account2.lock!
    #   account1.balance -= 100
    #   account1.save!
    #   account2.balance += 100
    #   account2.save!
    # end
    # ```
    #
    # You can start a transaction and acquire the lock in one go by calling
    # `with_lock` with a block. The block is called from within
    # a transaction, the object is already locked, and the transaction is
    # yielded so you can register callbacks. Example:
    #
    # ```
    # account = Account.first
    # account.with_lock do |transaction|
    #   # This block is called within a transaction,
    #   # account is already locked.
    #   transaction.after_commit { puts "hello" }
    #   account.balance -= 100
    #   account.save!
    # end
    # ```
    #
    # Database-specific information on row locking:
    #
    # MySQL
    # :   https://dev.mysql.com/doc/refman/en/innodb-locking-reads.html
    #
    # PostgreSQL
    # :   https://www.postgresql.org/docs/current/interactive/sql-select.html#SQL-FOR-UPDATE-SHARE
    module Pessimistic
      # Exceptions that can be raised while acquiring a row lock, before the
      # lock was granted and before the block of #with_lock had a chance to
      # run, and that are therefore safe to retry.
      DEFAULT_LOCK_RETRY_ERRORS = [
        ActiveRecord::Deadlocked,
        ActiveRecord::LockWaitTimeout,
        ActiveRecord::ConnectionFailed,
      ]

      # Obtain a row lock on this record. Reloads the record to obtain the requested
      # lock. Pass an SQL locking clause to append the end of the SELECT statement
      # or pass true for "FOR UPDATE" (the default, an exclusive row lock). Returns
      # the locked record.
      def lock!(lock = true)
        if self.class.current_preventing_writes
          raise ActiveRecord::ReadOnlyError, "Lock query attempted while in readonly mode"
        end

        if persisted?
          if has_changes_to_save?
            raise(<<-MSG.squish)
              Locking a record with unpersisted changes is not supported. Use
              `save` to persist the changes, or `reload` to discard them
              explicitly.
              Changed attributes: #{changed.map(&:inspect).join(', ')}.
            MSG
          end

          reload(lock: lock)
        end

        self
      end

      # Wraps the passed block in a transaction, reloading the object with a
      # lock before yielding. Yields the current transaction so you can
      # register callbacks. You can pass the SQL locking clause
      # as an optional argument (see #lock!).
      #
      # You can also pass options like `requires_new:`, `isolation:`,
      # and `joinable:` to the wrapping transaction (see
      # ActiveRecord::ConnectionAdapters::DatabaseStatements#transaction).
      #
      # ==== Retrying lock acquisition
      #
      # Some updates must happen exactly once, yet the database may refuse to
      # grant the lock with a retryable error (a deadlock, a lock wait
      # timeout, or a dropped connection) before the transaction has done any
      # work. Pass +lock_retry+ to retry just the lock acquisition, so the
      # block is still guaranteed to run at most once:
      #
      #   account.with_lock(lock_retry: { attempts: 3, deadline: 2.0 }) do
      #     account.balance -= 100
      #     account.save!
      #   end
      #
      # +lock_retry+ accepts +true+ (to use the defaults below) or a hash with
      # any of the following keys:
      #
      # * +:attempts+ - maximum number of times the lock acquisition is
      #   attempted, including the first one (default: 3).
      # * +:deadline+ - stop retrying this many seconds from now, even if
      #   +:attempts+ would allow more tries (default: no deadline).
      # * +:backoff+ - base number of seconds to sleep between attempts,
      #   multiplied by the attempt number (default: 0.05).
      # * +:retry_on+ - exception class, or array of classes, considered
      #   retryable (default: ActiveRecord::Deadlocked,
      #   ActiveRecord::LockWaitTimeout, ActiveRecord::ConnectionFailed).
      #
      # Only errors raised while acquiring the lock are retried; an error
      # raised from inside the block is re-raised immediately, whatever
      # +lock_retry+ says.
      #
      # When +with_lock+ joins an enclosing transaction, no retry is
      # performed: the failure is left to be handled by that transaction's
      # rules, since it may already have been doomed by the failed statement.
      # Pass <tt>requires_new: true</tt> to give the lock its own
      # (savepoint-backed) transaction, which can be retried safely.
      def with_lock(*args)
        transaction_opts = args.extract_options!
        lock_retry = transaction_opts.delete(:lock_retry)
        lock = args.present? ? args.first : true

        if lock_retry
          with_lock_retry(transaction_opts, lock_retry, lock) { |transaction| yield transaction }
        else
          transaction(**transaction_opts) do |transaction|
            lock!(lock)
            yield transaction
          end
        end
      end

      private
        def with_lock_retry(transaction_opts, retry_opts, lock) # :nodoc:
          retry_opts = {} unless retry_opts.is_a?(Hash)
          max_attempts = retry_opts.fetch(:attempts, 3)
          backoff      = retry_opts.fetch(:backoff, 0.05)
          retry_on     = Array(retry_opts[:retry_on] || DEFAULT_LOCK_RETRY_ERRORS)
          deadline     = retry_opts[:deadline] && Process.clock_gettime(Process::CLOCK_MONOTONIC) + retry_opts[:deadline]

          # Retrying is only safe when this with_lock owns its transaction.
          # When joining an outer transaction, a failed lock acquisition may
          # already have doomed it, so the error must propagate and be handled
          # by that transaction's rules.
          may_retry = transaction_opts[:requires_new] ||
            !self.class.with_connection(&:transaction_open?)

          attempts = 0
          begin
            attempts += 1
            lock_acquired = false
            transaction(**transaction_opts) do |transaction|
              lock!(lock)
              lock_acquired = true
              yield transaction
            end
          rescue => error
            if !lock_acquired && may_retry && attempts < max_attempts &&
                (deadline.nil? || Process.clock_gettime(Process::CLOCK_MONOTONIC) < deadline) &&
                retry_on.any? { |retryable| retryable === error }
              sleep(backoff * attempts) if backoff > 0
              retry
            end
            raise
          end
        end
    end
  end
end
