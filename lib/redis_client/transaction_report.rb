# frozen_string_literal: true

class RedisClient
  # Detailed, stage aware report of a `MULTI`/`EXEC` transaction.
  #
  # The regular {RedisClient#multi} method collapses every possible outcome to
  # either an Array of results or `nil`, which can't tell apart:
  #
  # * a transaction that was never entered, because `MULTI` was refused or no
  #   connection could be established;
  # * a command refused while queueing, which aborts the whole transaction;
  # * a transaction `EXEC` refused to run, because a `WATCH`ed key changed;
  # * a transaction that ran, where some commands failed during execution;
  # * a transaction whose outcome is unknown, because the connection was lost.
  #
  # `RedisClient#multi(report: true)` returns one of these objects instead of
  # the results. Every command written by the caller keeps a stable position,
  # and server side errors stay attached to that position, including the errors
  # nested in the `EXEC` reply.
  #
  # A report records facts. It never retries on the caller's behalf: if the
  # connection is lost mid transaction, commands whose outcome couldn't be
  # observed are reported as {Entry#unknown?} rather than being blindly re-run.
  #
  # @example
  #   report = redis.multi(report: true) do |transaction|
  #     transaction.call("SET", "foo", "bar")
  #     transaction.call("INCR", "foo") # WRONGTYPE, only when executed
  #   end
  #
  #   report.executed?                      # => true
  #   report.results                        # => ["OK", #<RedisClient::WrongTypeError...>]
  #   report.failed_entries.map(&:position) # => [2]
  class TransactionReport
    # Sentinel used by {#results} for positions with neither a value nor an
    # error, i.e. commands that were not executed or whose outcome is unknown.
    UNKNOWN = Object.new
    def UNKNOWN.inspect
      "RedisClient::TransactionReport::UNKNOWN"
    end
    UNKNOWN.freeze

    def self.unknown?(value)
      UNKNOWN.equal?(value)
    end

    # The status of a single command of the transaction.
    module Status
      PENDING = :pending
      EXECUTED = :executed
      FAILED = :failed
      REJECTED = :rejected
      NOT_QUEUED = :not_queued
      ABORTED = :aborted
      UNKNOWN_OUTCOME = :unknown
    end

    # A single command of the transaction, with its own stage outcome.
    class Entry
      attr_reader :index, :command, :status, :error, :value

      def initialize(index, command)
        @index = index
        @command = command
        @status = Status::PENDING
        @value = nil
        @error = nil
      end

      # 1-based position of the command, in the order it was written by the
      # caller. The `MULTI` and `EXEC` commands are not counted.
      def position
        @index + 1
      end

      # No outcome recorded yet.
      def pending?
        @status == Status::PENDING
      end

      # Accepted while queueing and executed by `EXEC`.
      def executed?
        @status == Status::EXECUTED
      end

      # Executed by `EXEC`, but the server replied with an error for it.
      def failed?
        @status == Status::FAILED
      end

      # Refused while queueing. A rejection aborts the whole transaction.
      def rejected?
        @status == Status::REJECTED
      end

      # Never transmitted, because the transaction was never entered.
      def not_queued?
        @status == Status::NOT_QUEUED
      end

      # Not executed, because a `WATCH`ed key was modified.
      def aborted?
        @status == Status::ABORTED
      end

      # The outcome is unknown, e.g. the connection was lost mid transaction.
      def unknown?
        @status == Status::UNKNOWN_OUTCOME
      end

      # True when the outcome of this command is known, whatever it is.
      def known?
        !pending? && !unknown?
      end

      # The value, or the error, recorded at this position.
      def result
        @error || @value
      end

      def inspect
        "#<#{self.class.name} position=#{position} status=#{@status.inspect} command=#{@command.inspect}>"
      end

      def to_h
        {
          index: @index,
          position: position,
          command: @command,
          status: @status,
          value: @value,
          error: @error,
        }
      end

      def _execute!(value) # :nodoc:
        @status = Status::EXECUTED
        @value = value
      end

      def _fail!(error) # :nodoc:
        @status = Status::FAILED
        @error = error
      end

      def _reject!(error) # :nodoc:
        @status = Status::REJECTED
        @error = error
      end

      def _not_queued! # :nodoc:
        @status = Status::NOT_QUEUED
      end

      def _abort! # :nodoc:
        @status = Status::ABORTED
      end

      def _unknown! # :nodoc:
        @status = Status::UNKNOWN_OUTCOME
      end

      def _apply_block!(block) # :nodoc:
        @value = block.call(@value)
      end
    end

    attr_reader :watch, :status, :error, :multi_error, :exec_error
    attr_reader :interruption_error, :commands, :entries

    def initialize(watch: nil)
      @watch = watch
      @status = :pending
      @error = nil
      @multi_error = nil
      @exec_error = nil
      @interruption_error = nil
      @commands = [].freeze
      @entries = [].freeze
      @started = false
    end

    # Was the `MULTI`/queue/`EXEC` exchange actually attempted on the wire?
    def started?
      @started
    end

    def finished?
      !(@status == :pending || @status == :in_progress)
    end

    # There was no transaction command to run, so nothing was queued and
    # `EXEC` wasn't sent.
    def empty?
      @status == :empty
    end

    # The transaction envelope was never established, so none of its commands
    # was queued as part of a transaction. Whether the absence of outcome is
    # certain for each command is reported by {Entry#not_queued?} and
    # {Entry#unknown?}.
    def not_entered?
      @status == :not_entered
    end

    # `MULTI` itself was refused, so the commands were executed (or failed) on
    # their own instead of as a transaction. Check {#entries} for the individual
    # outcomes.
    def untransacted?
      @status == :untransacted
    end

    # At least one command was refused while queueing, so the transaction never
    # ran.
    def rejected?
      @status == :rejected
    end

    # `EXEC` refused to run the transaction, because a `WATCH`ed key changed.
    def aborted?
      @status == :aborted
    end

    # `EXEC` ran the transaction. Individual commands may still have failed,
    # see {#failed_entries}.
    def executed?
      @status == :executed
    end

    # The transaction was in flight when the connection was lost, and at least
    # one command has an unknown outcome.
    def interrupted?
      @status == :interrupted
    end
    alias_method :unknown?, :interrupted?

    # The transaction was entered, executed, and every command succeeded.
    #
    # A `nil` `EXEC` reply (transaction aborted by `WATCH`) is never a success,
    # and neither is a broken `MULTI`, a rejected command or an interrupted
    # transaction. An empty report is a success only because there was nothing
    # to do.
    def success?
      return true if empty?

      executed? && multi_error.nil? && entries.all?(&:executed?)
    end

    def failure?
      !success?
    end

    def size
      @commands.size
    end

    def each_entry(&block)
      return entries.each unless block

      entries.each(&block)
      self
    end
    alias_method :each, :each_entry

    def executed_entries
      entries.select(&:executed?)
    end

    def failed_entries
      entries.select(&:failed?)
    end

    def rejected_entries
      entries.select(&:rejected?)
    end

    def unknown_entries
      entries.select(&:unknown?)
    end

    def not_queued_entries
      entries.select(&:not_queued?)
    end

    def aborted_entries
      entries.select(&:aborted?)
    end

    # One slot per command, in the original order:
    #
    # * the value, for commands executed successfully;
    # * the error, for commands rejected while queueing or failed on execution;
    # * {UNKNOWN}, for commands not executed or with an unknown outcome.
    #
    # It intentionally never substitutes `[]`/`nil` for an unknown outcome, so
    # an aborted or interrupted transaction can't be mistaken for an empty
    # successful one.
    def results
      entries.map do |entry|
        if entry.executed? || entry.failed? || entry.rejected?
          entry.result
        else
          UNKNOWN
        end
      end
    end

    # Errors, indexed by the original 0-based command index.
    def errors
      errors = {}
      entries.each do |entry|
        errors[entry.index] = entry.error if entry.error
      end
      errors
    end

    def first_error
      return @error if @error

      entries.each do |entry|
        return entry.error if entry.error
      end
      nil
    end

    def inspect
      "#<#{self.class.name} status=#{@status.inspect} size=#{size} error=#{@error.inspect}>"
    end

    def to_h
      {
        status: @status,
        watch: @watch,
        error: @error,
        multi_error: @multi_error,
        exec_error: @exec_error,
        interruption_error: @interruption_error,
        entries: entries.map(&:to_h),
      }
    end

    def _attach!(commands) # :nodoc:
      # `MULTI` and `EXEC` are the transaction envelope, not caller commands.
      @commands = (commands || []).freeze
      @entries = @commands.each_with_index.map do |command, index|
        Entry.new(index, command)
      end.freeze
      self
    end

    def _started! # :nodoc:
      @started = true
      @status = :in_progress
      self
    end

    def _empty! # :nodoc:
      @started = false
      @status = :empty
      self
    end

    # Nothing was transmitted for this transaction: every command certainly did
    # not run.
    def _not_entered!(error) # :nodoc:
      @started = false
      @status = :not_entered
      @error = error
      @entries.each do |entry|
        entry._not_queued! if entry.pending?
      end
      self
    end

    # The whole pipeline of replies was received. `results` holds one reply per
    # command of the pipeline: `MULTI`, then the queued commands, then `EXEC`.
    def _ingest!(results) # :nodoc:
      unless results.is_a?(Array) && results.size == @entries.size + 2
        return _interrupted!(ProtocolError.new("Incomplete transaction reply"))
      end

      multi_reply = results.first
      exec_reply = results.last
      queue_replies = results[1...-1]

      if multi_reply.is_a?(Error)
        # `MULTI` was refused: there is no transaction, and the commands were
        # executed (or failed) on their own.
        @multi_error = multi_reply
        @exec_error = exec_reply if exec_reply.is_a?(Error)
        return _ingest_without_multi!(queue_replies)
      end

      queue_error = _apply_queue_replies!(queue_replies)
      if queue_error
        # A command refused while queueing aborts the whole transaction: none
        # of the commands were executed, whatever `EXEC` replied.
        @exec_error = exec_reply if exec_reply.is_a?(Error)
        return _rejected!(queue_error)
      end

      case exec_reply
      when nil
        # `EXEC` was refused because a WATCHed key changed. This is NOT an
        # empty successful transaction.
        @status = :aborted
        @entries.each(&:_abort!)
      when Array
        _ingest_exec_reply(exec_reply)
      when Error
        # e.g. EXECABORT: the transaction didn't run.
        @exec_error = exec_reply
        _rejected!(exec_reply)
      else
        _interrupted!(ProtocolError.new("Unexpected transaction reply: #{exec_reply.inspect}"))
      end

      self
    end

    # Dispatches an interruption, depending on whether anything was written.
    def _interruption!(error, progress) # :nodoc:
      if started?
        _interrupted!(error, progress)
      else
        _not_entered!(error)
      end
    end

    # The connection was lost while the transaction was in flight. Facts already
    # observed are kept, every other command is marked unknown.
    def _interrupted!(error, progress = nil) # :nodoc:
      @interruption_error = error
      progress ||= {}

      multi_reply = progress[0]
      if multi_reply.is_a?(Error)
        # The transaction envelope was refused. Commands read so far ran outside
        # of a transaction, the rest have an unknown outcome.
        @multi_error = multi_reply
        return _ingest_without_multi!(progress)
      end

      queue_error = nil
      @entries.each do |entry|
        reply = progress[entry.index + 1]
        next unless reply.is_a?(Error)

        _attach_command(reply, entry)
        entry._reject!(reply)
        queue_error ||= reply
      end

      if queue_error
        # The queue rejection deterministically aborted the transaction, which
        # stays a known fact even though the connection was then lost.
        return _rejected!(queue_error)
      end

      exec_index = @entries.size + 1
      if progress.key?(exec_index)
        exec_reply = progress[exec_index]
        if exec_reply.is_a?(Array)
          # `EXEC` actually answered, so the whole transaction is known.
          _ingest_exec_reply(exec_reply)
          return self
        elsif exec_reply.nil?
          @status = :aborted
          @error = error
          @entries.each(&:_abort!)
          return self
        end
      end

      @status = :interrupted
      @error = error
      _mark_pending_unknown!
      self
    end

    def _apply_blocks!(blocks) # :nodoc:
      return self unless blocks

      # `blocks` is indexed like the pipeline, hence the `index - 1` offset.
      blocks.each_with_index do |block, pipeline_index|
        next unless block

        entry = @entries[pipeline_index - 1]
        next unless entry&.executed?

        entry._apply_block!(block)
      end
      self
    end

    private

    def _attach_command(error, entry)
      error._set_command(entry.command) if error.respond_to?(:_set_command)
    end

    def _mark_pending_unknown!
      @entries.each do |entry|
        entry._unknown! if entry.pending?
      end
    end

    def _rejected!(error)
      @status = :rejected
      @error ||= error
      @entries.each do |entry|
        entry._not_queued! if entry.pending?
      end
      self
    end

    # `MULTI` itself was refused, so the queue replies belong to commands
    # executed outside of a transaction.
    def _ingest_without_multi!(replies)
      @status = :untransacted
      @error = @multi_error
      _apply_untransacted_replies!(replies)
      _mark_pending_unknown!
      self
    end

    # Records replies of commands that were executed outside of a `MULTI`.
    # `replies` may be a positional Array or a Hash keyed by pipeline index.
    def _apply_untransacted_replies!(replies)
      @entries.each do |entry|
        reply = replies.is_a?(Hash) ? replies[entry.index + 1] : replies[entry.index]
        next unless reply

        if reply.is_a?(Error)
          _attach_command(reply, entry)
          entry._fail!(reply)
        elsif reply != "QUEUED"
          entry._execute!(reply)
        end
      end
      self
    end

    # Returns the first queue-stage error and records the per-command outcome.
    def _apply_queue_replies!(queue_replies)
      first_error = nil
      queue_replies.each_with_index do |reply, index|
        entry = @entries[index]
        break unless entry
        next unless reply.is_a?(Error)

        _attach_command(reply, entry)
        entry._reject!(reply)
        first_error ||= reply
      end
      first_error
    end

    def _ingest_exec_reply(exec_reply)
      exec_reply.each_with_index do |value, index|
        entry = @entries[index]
        break unless entry

        if value.is_a?(Error)
          _attach_command(value, entry)
          entry._fail!(value)
        else
          # A `nil` here is a real reply (e.g. a conditional write), never a
          # missing result: the i-th slot belongs to the i-th command.
          entry._execute!(value)
        end
      end

      # If `EXEC` returned fewer replies than commands, the missing ones have an
      # unknown outcome rather than an assumed one.
      if @entries.any?(&:pending?)
        @interruption_error ||= ProtocolError.new("Incomplete EXEC reply")
        _mark_pending_unknown!
      end

      if @entries.any?(&:unknown?)
        @status = :interrupted
        @error = @interruption_error || ProtocolError.new("Incomplete EXEC reply")
      else
        # The whole transaction was observed, so an earlier connection error is
        # irrelevant to the outcome.
        @status = :executed
        @error = first_entry_error
      end
      self
    end

    def first_entry_error
      entries.each do |entry|
        return entry.error if entry.error
      end
      nil
    end
  end
end

