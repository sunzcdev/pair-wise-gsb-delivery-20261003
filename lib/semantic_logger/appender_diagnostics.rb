module SemanticLogger
  # Internal: reports the failure of an appender, and its recovery, to the other appenders, the
  # siblings that write entries alongside it, rather than only to the internal logger.
  #
  # A report is a log entry of its own, written straight to each sibling's #log, on the thread that
  # is already writing to the appenders, never put back through the logging pipeline, so that a
  # report never takes the failing appender's exit a second time. A report that a sibling cannot
  # take is reported to the internal logger instead, and does not start another report: a
  # fiber-local guard bounds the diagnostics to one level, so that a sibling that is also failing,
  # or slow, can neither recurse on the producing thread nor grow the recursion without end.
  #
  # The entry names the appender at fault and carries its exception, so that the real source of the
  # failure can be told apart from an appender that only received the report. The standalone
  # internal logger (SemanticLogger::Processor.logger) keeps reporting every failure as before; this
  # is an additional destination, off its path.
  class AppenderDiagnostics
    # Fiber-local key holding the guard that stops a report starting another report.
    GUARD_KEY = :semantic_logger_diagnostics_active
    private_constant :GUARD_KEY

    def initialize
      # The appenders that have failed and not yet written an entry again, by identity.
      @failed           = {}.compare_by_identity
      # How many reports were not written because a sibling could not take them.
      @suppressed_count = 0
      # How many further reports the guard stopped from starting, on any fiber.
      @recursion_count  = 0
    end

    # Returns [Integer] how many reports a sibling could not take, which went to the internal
    # logger instead.
    attr_reader :suppressed_count

    # Returns [Integer] how many further reports the re-entrancy guard stopped from starting.
    # It stays bounded, since a report never starts another report.
    attr_reader :recursion_count

    # Whether the appender has failed and not yet recovered.
    def failed?(appender)
      @failed.key?(appender)
    end

    # Report that writing to the appender raised the exception, to every appender but it, once. The
    # first failure for an appender is reported; later ones, before it recovers, change only the
    # exception held, so a down destination cannot flood its siblings during an outage.
    def failure(appenders, appender, exception, internal_logger)
      first = !@failed.key?(appender)
      @failed[appender] = exception
      return unless first

      entry = build_entry(
        :error,
        "Appender #{appender.name} failed to write a log message " \
          "(#{Utils.exception_name(exception)}: #{Utils.exception_message(exception)})",
        exception,
        appender
      )
      deliver(appenders, appender, entry, internal_logger)
    end

    # Report that the appender, having failed, has written an entry again, and forget it. Reports
    # once per failure, so the next failure reports its recovery only after it fails again.
    def recovery(appenders, appender, internal_logger)
      exception = @failed.delete(appender)
      return unless exception

      entry = build_entry(
        :info,
        "Appender #{appender.name} has recovered and is writing log messages again " \
          "(last error: #{Utils.exception_name(exception)}: #{Utils.exception_message(exception)})",
        nil,
        appender
      )
      deliver(appenders, appender, entry, internal_logger)
    end

    # Forget every appender still marked as failed, such as once the appenders are closed, without
    # reporting their recovery.
    def reset
      @failed.clear
    end

    private

    # Write the report to each sibling that takes the entry, on the current fiber, under the guard.
    # A sibling that raises, or one reached while a report is already being written on this fiber,
    # is not reported to the siblings again: the first goes to the internal logger, and the second
    # is counted and skipped, so the recursion stays one level deep.
    def deliver(appenders, failed_appender, entry, internal_logger)
      if active?
        @recursion_count += 1
        return
      end

      begin
        enter
        appenders.each do |appender|
          next if appender.equal?(failed_appender)
          next unless appender.should_log?(entry)

          begin
            appender.log(entry)
          rescue SystemStackError
            raise
          rescue Exception => e
            @suppressed_count += 1
            internal_logger.error(
              "Failed to write the diagnostics for appender #{failed_appender.name} " \
                "to appender #{appender.name}",
              e
            )
          end
        end
      ensure
        leave
      end
    end

    # Build the entry that reports the appender, naming it as the source, distinct from the sibling
    # that receives the entry, and carrying the exception that its write raised.
    def build_entry(level, message, exception, appender)
      Log.new("SemanticLogger::AppenderDiagnostics", level).tap do |entry|
        entry.assign(
          message:   message,
          exception: exception,
          payload:   {appender: appender.name, fault: :appender_failure}
        )
      end
    end

    def active?
      Thread.current[GUARD_KEY] == true
    end

    def enter
      Thread.current[GUARD_KEY] = true
    end

    def leave
      Thread.current[GUARD_KEY] = nil
    end
  end
end
