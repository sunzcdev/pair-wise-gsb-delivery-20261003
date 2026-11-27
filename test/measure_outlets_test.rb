require_relative "test_helper"
require_relative "controlled_clock"

# End-to-end behavior for one measured event fanned out to two outlets that read the same duration
# in different units, where one outlet is slow (async) and the other immediate, and the slow one
# retries after a failure. A controlled clock makes the measured duration and the queue wait exact.
class MeasureOutletsTest < Minitest::Test
  # Captures every entry it writes, recording the duration in the unit it is configured with, so two
  # outlets can read one measurement in different units.
  class UnitAppender < SemanticLogger::Subscriber
    attr_reader :written

    def initialize(duration_unit:, **args)
      super(duration_unit: duration_unit, **args)
      @written = []
    end

    def log(log)
      @written << {
        log:            log,
        duration:       duration_in(log),
        duration_unit:  duration_unit,
        queue_duration: queue_duration_in(log)
      }
    end
  end

  # Like UnitAppender, but the first write of an entry whose message includes "retry" fails with a
  # destination error that wrote nothing; the held entry is then written again by the worker thread
  # (see QueueProcessor). The decision to log it and its event time are made before it is queued, so
  # the retry neither re-filters nor re-times the entry.
  class SlowAppender < UnitAppender
    class DestinationError < StandardError; end

    attr_reader :failures, :attempts

    def initialize(**args)
      super
      @failures = Queue.new
      @attempts = []
      @retried  = false
    end

    def log(log)
      @attempts << log.message
      if log.message.include?("retry") && !@retried
        @retried = true
        @failures << log.message
        raise DestinationError, "destination unavailable"
      end
      super
    end

    # A destination outage, not bad content: the worker backs off and writes the entry again.
    def message_error?(_exception)
      false
    end

    # The failed write reached no destination, so writing it again cannot duplicate it and is worth
    # retrying (see Subscriber#wrote_nothing?).
    def wrote_nothing?(exception)
      exception.is_a?(DestinationError)
    end
  end

  # Holds the worker on a blocker entry until a gate opens, so entries queued meanwhile really wait
  # on the queue. The blocker itself is neither measured nor retried.
  class BlockingAppender < SlowAppender
    attr_reader :gate, :blocking

    def initialize(**args)
      super
      @gate     = Queue.new
      @blocking = Queue.new
    end

    def log(log)
      if log.message == "blocker"
        @blocking << log.message
        @gate.pop
        return
      end
      super
    end
  end

  # A back-off that never sleeps and never counts a retry, so a held entry is written again as
  # soon as the worker loops, without approaching async_max_retries or reporting a back-off that
  # did not happen. Used directly (rather than Minitest's thread-local #stub) because the wait runs
  # on the worker thread, which a stub installed on the test thread does not reach.
  class ImmediateBackoff < SemanticLogger::QueueProcessor::Backoff
    def wait
      nil
    end

    def retry?
      true
    end

    def retry_count
      0
    end
  end

  describe "one measured event at two outlets" do
    let(:clock) { ControlledClock.new }

    # Track every queue processor whose worker thread is started, so each test can stop it.
    let(:processors) { [] }

    after do
      processors.each do |processor|
        thread = processor.instance_variable_get(:@thread)
        thread&.kill
        thread&.join
      end
    end

    def start_processor(appender, **args)
      processor = SemanticLogger::QueueProcessor.new(appender: appender, clock: clock,
                                                                 async_max_retries: 100, **args)
      # Swap in a back-off that does not sleep before the worker thread starts.
      processor.instance_variable_set(:@backoff, ImmediateBackoff.new(100))
      processor.thread
      processors << processor
      processor
    end

    # Waits until the block is true or the deadline passes.
    def wait_until(seconds = 5)
      deadline = Time.now + seconds
      Thread.pass until yield || Time.now > deadline
      assert yield, "timed out waiting for the expected state"
    end

    # Builds one measured entry on the controlled clock: an operation just over the millisecond
    # min_duration threshold. The filtering decision and the event time are set here, before it is
    # handed to any queue.
    def measured_log(logger, message, min_duration:)
      event_time = nil
      log        = nil
      clock.use do
        logger.measure_info(message, min_duration: min_duration, metric: "supplier/call") do
          clock.advance(0.0016) # 1.6 ms, just over a millisecond threshold
        end
        log        = logger.events.last
        event_time = log.time
      end
      [log, event_time]
    end

    def blocker_log
      SemanticLogger::Log.new("MeasureOutletsTest", :info).tap { |log| log.message = "blocker" }
    end

    it "lets each outlet read the same measurement in its own unit, keeping the millisecond reading" do
      slow_appender = SlowAppender.new(duration_unit: :microseconds)
      fast_appender = UnitAppender.new(duration_unit: :milliseconds)
      slow          = start_processor(slow_appender)
      fast          = start_processor(fast_appender)
      logger        = SemanticLogger::Test::CaptureLogEvents.new

      log, event_time = measured_log(logger, "retry measured", min_duration: 1.5)
      refute_nil log, "the 1.6 ms operation must pass the 1.5 ms min_duration threshold"
      assert_in_delta 1.6, log.duration # the stored duration stays in milliseconds

      # Hand the very same event to both outlets.
      assert slow.log(log)
      assert fast.log(log)

      wait_until { slow_appender.written.size == 1 && fast_appender.written.size == 1 }
      assert slow.flush
      assert fast.flush

      slow_reading = slow_appender.written.first
      fast_reading = fast_appender.written.first

      # The two outlets reconstruct the same value in different units.
      assert_equal :microseconds, slow_reading[:duration_unit]
      assert_equal :milliseconds, fast_reading[:duration_unit]
      assert_in_delta 1_600.0, slow_reading[:duration]
      assert_in_delta 1.6, fast_reading[:duration]
      assert_in_delta slow_reading[:duration] / 1_000.0, fast_reading[:duration]
      # The shared entry keeps its millisecond duration and original event time.
      assert_in_delta 1.6, log.duration
      assert_equal event_time, log.time
    end

    it "does not re-filter or re-time the entry when the slow outlet retries it" do
      slow_appender = SlowAppender.new(duration_unit: :microseconds)
      slow          = start_processor(slow_appender)
      logger        = SemanticLogger::Test::CaptureLogEvents.new

      log, event_time = measured_log(logger, "retry measured", min_duration: 1.5)
      original        = log.dup

      assert slow.log(log)
      wait_until { slow_appender.written.size == 1 }
      assert slow.flush

      # The destination failed once and the entry was retried.
      assert_equal 1, slow_appender.failures.size
      assert_equal ["retry measured", "retry measured"], slow_appender.attempts
      assert_equal 1, slow_appender.written.size

      written = slow_appender.written.first[:log]
      assert_same log, written
      # The retry neither re-ran the min_duration decision nor reset the event time or measurement.
      assert_equal event_time, written.time
      assert_equal original.time, written.time
      assert_in_delta original.duration, written.duration
      assert_in_delta 1_600.0, slow_appender.written.first[:duration]
    end
    it "reflects the async wait separately from the measurement and advances wall time during it" do
      slow_appender = BlockingAppender.new(duration_unit: :microseconds)
      fast_appender = UnitAppender.new(duration_unit: :milliseconds)
      slow          = start_processor(slow_appender)
      fast          = start_processor(fast_appender)
      logger        = SemanticLogger::Test::CaptureLogEvents.new

      # Occupy the slow worker on the blocker, so the measured event waits in its queue.
      assert slow.log(blocker_log)
      wait_until { slow_appender.blocking.size == 1 }

      log, = measured_log(logger, "retry measured", min_duration: 1.5)
      wall_time_before_wait = Time.now

      # The event is queued behind the blocker at both outlets. The fast one has no blocker and so
      # writes it immediately; the slow one keeps it waiting on its queue.
      assert slow.log(log)
      assert fast.log(log)

      wait_until { fast_appender.written.size == 1 && slow.queue.size == 1 }
      assert fast.flush
      assert_empty slow_appender.written

      # Wall time advances for real while the entry waits; advance the controlled monotonic clock by
      # the same window, which is what the queue wait is measured from.
      sleep 0.05
      clock.advance(0.08) # 80 ms of controlled monotonic time while the entry is queued
      slow_appender.gate << true # release the blocker; the worker reaches the measured event

      wait_until { slow_appender.written.size == 1 }
      assert slow.flush
      wall_time_elapsed = Time.now - wall_time_before_wait
      assert_operator wall_time_elapsed, :>=, 0.05

      slow_reading = slow_appender.written.first
      fast_reading = fast_appender.written.first

      # The slow outlet's queue wait matches the actual waiting window and is separate from the
      # measurement, while both outlets still read the same 1.6 ms measurement.
      assert_in_delta 80_000.0, slow_reading[:queue_duration], 5_000
      # The immediate outlet's worker was free, so its queue wait is negligible next to the slow one.
      assert_operator fast_reading[:queue_duration], :<, slow_reading[:queue_duration] / 100
      assert_in_delta 1_600.0, slow_reading[:duration]
      assert_in_delta 1.6, fast_reading[:duration]
      assert_in_delta 1.6, log.duration # the measurement is separate from the queue wait
      # The blocker made the worker pause before transmission; the measured event was still retried
      # once after its destination failed, without a second enqueue.
      assert_equal ["retry measured", "retry measured"], slow_appender.attempts
    end
  end
end
