require_relative "test_helper"

class AppenderDiagnosticsTest < Minitest::Test
  # An in-memory appender whose normal writes and diagnostics writes can be made to fail a chosen
  # number of times, independently. It records every entry it keeps, and how often it is entered.
  class FlakyAppender < SemanticLogger::Test::CaptureLogEvents
    attr_reader :entered

    def initialize(failures: 0, diagnostic_failures: 0)
      super()
      @entered              = 0
      @failures_left        = failures
      @diagnostic_failures  = diagnostic_failures
    end

    def log(log)
      @entered += 1
      if diagnostic?(log)
        if @diagnostic_failures.positive?
          @diagnostic_failures -= 1
          raise IOError, "diagnostic sink down"
        end
      elsif @failures_left.positive?
        @failures_left -= 1
        raise IOError, "real destination boom"
      end

      super
    end

    private

    def diagnostic?(log)
      log.name == "SemanticLogger::AppenderDiagnostics"
    end
  end

  # On a diagnostics entry, calls back into the diagnostics, so the re-entrancy guard is exercised.
  class ReentrantAppender < SemanticLogger::Test::CaptureLogEvents
    def initialize(diagnostics, appenders)
      super()
      @diagnostics = diagnostics
      @appenders   = appenders
    end

    def log(log)
      if log.name == "SemanticLogger::AppenderDiagnostics"
        @diagnostics.failure(@appenders, self, IOError.new("nested"), SemanticLogger::Test::CaptureLogEvents.new)
      end
      super
    end
  end

  describe SemanticLogger::AppenderDiagnostics do
    let(:internal) { SemanticLogger::Test::CaptureLogEvents.new }
    let(:appenders) { SemanticLogger::Appenders.new(internal, diagnostics: true) }
    let(:diagnostics) { appenders.diagnostics }

    def entry(message = "hello")
      SemanticLogger::Log.new("Producer", :info).tap { |log| log.message = message }
    end

    it "is enabled only when asked, leaving the standalone internal logger path as it was" do
      enabled  = SemanticLogger::Appenders.new(SemanticLogger::Test::CaptureLogEvents.new, diagnostics: true)
      disabled = SemanticLogger::Appenders.new(SemanticLogger::Test::CaptureLogEvents.new)

      assert_kind_of SemanticLogger::AppenderDiagnostics, enabled.diagnostics
      assert_nil disabled.diagnostics
    end

    it "reports the failure of the first appender to the second, naming the real source" do
      first  = FlakyAppender.new(failures: 1)
      second = FlakyAppender.new
      appenders << first << second

      appenders.log(entry)

      report = second.events.find { |log| log.name == "SemanticLogger::AppenderDiagnostics" }

      refute_nil report
      assert_equal :error, report.level
      assert_includes report.message, "Appender #{first.name} failed"
      assert_includes report.message, "real destination boom"
      assert_equal first.name, report.payload[:appender]
      assert_kind_of IOError, report.exception
      assert_equal "real destination boom", report.exception.message
      # The failing appender itself never receives its own report.
      refute first.events.any? { |log| log.name == "SemanticLogger::AppenderDiagnostics" }
    end

    it "keeps reporting the original failure to the internal logger when the sibling cannot take the report" do
      first  = FlakyAppender.new(failures: 1)
      second = FlakyAppender.new(diagnostic_failures: 1)
      appenders << first << second

      appenders.log(entry)

      messages = internal.events.map(&:message)

      assert_includes messages, "Failed to log to appender: #{first.name}"
      assert_includes messages, "Failed to write the diagnostics for appender #{first.name} to appender #{second.name}"
      # The sibling's own failure is recorded, not lost.
      assert_equal 1, diagnostics.suppressed_count
      # The report never starts another report, so there is no recursion.
      assert_equal 0, diagnostics.recursion_count
    end

    it "reports the recovery of the first appender once it writes again, after the sibling recovers too" do
      first  = FlakyAppender.new(failures: 1)
      second = FlakyAppender.new(diagnostic_failures: 1)
      appenders << first << second

      appenders.log(entry("one"))
      appenders.log(entry("two"))

      reports = second.events.select { |log| log.name == "SemanticLogger::AppenderDiagnostics" }

      # The failure report could not be written while the sibling was down; once it recovers, the
      # recovery report for the first appender is written, naming the error it recovered from.
      recovery = reports.find { |log| log.message.include?("has recovered") }

      refute_nil recovery
      assert_equal :info, recovery.level
      assert_includes recovery.message, first.name
      assert_includes recovery.message, "real destination boom"
      refute diagnostics.failed?(first)
      refute diagnostics.failed?(second)
    end

    it "does not flood the sibling while the appender stays down, reporting the failure once" do
      down    = FlakyAppender.new(failures: 100)
      healthy = FlakyAppender.new
      appenders << down << healthy

      5.times { appenders.log(entry) }

      failure_reports = healthy.events.count do |log|
        log.name == "SemanticLogger::AppenderDiagnostics" && log.level == :error
      end

      assert_equal 1, failure_reports
      assert diagnostics.failed?(down)
    end

    it "keeps the recursion one level deep and its counter bounded when a report re-enters" do
      first     = FlakyAppender.new(failures: 1)
      reentrant = ReentrantAppender.new(diagnostics, appenders)
      appenders << first << reentrant

      appenders.log(entry)

      # The nested report is stopped by the guard rather than run again, so the one outer report
      # adds exactly one to the counter: the depth never exceeds one.
      assert_equal 1, diagnostics.recursion_count
      assert_equal 0, diagnostics.suppressed_count

      # Another outer report on a fresh collection finds the guard reset (no state leaks between
      # reports), and again reaches only one level deep, adding exactly one more.
      fresh       = SemanticLogger::Appenders.new(internal, diagnostics: true)
      fresh_diag  = fresh.diagnostics
      fresh_first = FlakyAppender.new(failures: 1)
      fresh << fresh_first << ReentrantAppender.new(fresh_diag, fresh)
      fresh.log(entry)

      assert_equal 1, fresh_diag.recursion_count
      assert_equal 0, fresh_diag.suppressed_count
    end

    it "resets its record of failed appenders when the appenders close" do
      first = FlakyAppender.new(failures: 100)
      appenders << first
      appenders.log(entry)

      assert diagnostics.failed?(first)

      appenders.close

      refute diagnostics.failed?(first)
      assert_empty appenders
    end
  end

  # Plain #log-only in-memory appender, so it is never wrapped in an async/batch proxy: every write
  # goes through Appenders#log inline, the path on which the diagnostics fan out.
  class MemoryAppender < SemanticLogger::Subscriber
    attr_reader :events

    def initialize
      super
      @events = []
    end

    def log(log)
      @events << log
    end

    # Fail the next `count` ordinary (non-diagnostics) writes, then recover and take the rest.
    def fail_next(count, message)
      remaining = count
      error     = IOError.new(message)
      define_singleton_method(:log) do |log|
        if log.name != "SemanticLogger::AppenderDiagnostics" && remaining.positive?
          remaining -= 1
          raise error
        end
        @events << log
      end
    end

    # Fail the next diagnostics write once, then take the ones that follow.
    def fail_diagnostics_once(message)
      remaining = 1
      error     = IOError.new(message)
      define_singleton_method(:log) do |log|
        if log.name == "SemanticLogger::AppenderDiagnostics" && remaining.positive?
          remaining -= 1
          raise error
        end
        @events << log
      end
    end
  end

  describe "SemanticLogger diagnostics, end to end" do
    before do
      @original_diagnostics = SemanticLogger.diagnostics?
      # Enable on whatever processor the test environment built (sync or async); the setter applies
      # it to the existing appenders straight away.
      SemanticLogger.diagnostics = true
    end

    after do
      SemanticLogger.close
      SemanticLogger.diagnostics = @original_diagnostics
    end

    it "records one appender's failure and recovery on the other, which fails the report once then recovers" do
      internal = SemanticLogger::Test::CaptureLogEvents.new
      first    = MemoryAppender.new
      second   = MemoryAppender.new
      first.fail_next(1, "real destination boom")
      second.fail_diagnostics_once("diagnostic sink down once")
      SemanticLogger.add_appender(appender: first)
      SemanticLogger.add_appender(appender: second)
      SemanticLogger.appenders.logger = internal

      SemanticLogger["Producer"].info("first message")
      SemanticLogger["Producer"].info("second message")
      # In async mode the fan-out runs on the processor's worker thread; wait for both entries.
      SemanticLogger.flush

      diagnostics = SemanticLogger.appenders.diagnostics

      # The recursion never grows: the report that the second appender could not take did not start
      # another report, and was handed to the internal logger instead.
      assert_equal 0, diagnostics.recursion_count
      assert_equal 1, diagnostics.suppressed_count

      reports = second.events.select { |log| log.name == "SemanticLogger::AppenderDiagnostics" }

      # The output points at the real source: the first appender, with its own exception. Its failure
      # report could not be delivered while the second was down; the recovery that follows names the
      # first appender and the error it recovered from.
      recovery = reports.find { |log| log.level == :info && log.message.include?("has recovered") }

      refute_nil recovery
      assert_includes recovery.message, first.name
      assert_includes recovery.message, "real destination boom"
      assert_equal first.name, recovery.payload[:appender]

      # The original failure is never lost: it stays on the standalone internal logger, and so does
      # the report that the second appender could not take.
      assert internal.events.any? { |log| log.message.include?("Failed to log to appender: #{first.name}") }
      assert internal.events.any? { |log| log.message.include?("Failed to write the diagnostics") }

      # Both appenders have recovered and take ordinary entries again.
      assert_includes first.events.map(&:message), "second message"
      assert_includes second.events.map(&:message), "second message"
      refute diagnostics.failed?(first)
      refute diagnostics.failed?(second)
    end

    it "leaves the old standalone internal logger usage working when diagnostics are off" do
      SemanticLogger.diagnostics = false

      internal = SemanticLogger::Test::CaptureLogEvents.new
      appenders = SemanticLogger::Appenders.new(internal)
      failing = MemoryAppender.new
      failing.fail_next(1, "boom")
      appenders << failing

      appenders.log(SemanticLogger::Log.new("Producer", :info).tap { |log| log.message = "hi" })

      assert_nil appenders.diagnostics
      assert internal.events.any? { |log| log.message.include?("Failed to log to appender") }
    end
  end
end
