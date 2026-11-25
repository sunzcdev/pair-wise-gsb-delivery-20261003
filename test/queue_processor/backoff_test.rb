require_relative "../test_helper"
require_relative "../signal_trap_helper"

class BackoffTest < Minitest::Test
  # A fake clock: #wait returns at once, and records the seconds it was asked to wait for, so that a
  # test never waits out the real back-off, yet sees how long it would have been.
  class FakeClock
    attr_reader :waits

    def initialize
      @waits = []
    end

    def wait(queue, seconds)
      @waits << seconds
      queue.pop(timeout: 0)
    end
  end

  describe SemanticLogger::QueueProcessor::Backoff do
    include SignalTrapHelper

    let(:backoff) { SemanticLogger::QueueProcessor::Backoff.new(2) }

    # Runs #wait on another thread, and returns that thread.
    def waiting
      thread = Thread.new { backoff.wait }
      Thread.pass until thread.status == "sleep" || !thread.alive?
      thread
    end

    it "retries up to max_retries times in a row, counting each failure" do
      assert_predicate backoff, :retry?
      assert_predicate backoff, :retry?
      refute_predicate backoff, :retry?
      assert_equal 2, backoff.retry_count
      assert_equal 2, backoff.limit
    end

    it "retries for ever when max_retries is -1" do
      backoff = SemanticLogger::QueueProcessor::Backoff.new(-1)

      assert_predicate(Array.new(1_000) { backoff.retry? }, :all?)
      assert_equal "unlimited", backoff.limit
    end

    it "counts afresh once reset" do
      2.times { backoff.retry? }
      backoff.reset

      assert_equal 0, backoff.retry_count
      assert_predicate backoff, :retry?
    end

    it "waits retry_count seconds, until a request is submitted" do
      2.times { backoff.retry? }
      thread = waiting

      refute thread.join(0.2), "the back-off should still be waiting"
      backoff.request_submitted

      assert thread.join(1), "a request should end the back-off"
      assert_predicate backoff, :waited_on?
    end

    it "does not wait while a request is waiting" do
      backoff.retry?
      backoff.request_submitted
      backoff.request_submitted
      backoff.request_done

      assert waiting.join(0.5), "the back-off should not wait"
    end

    it "waits once the requests have run, despite their having ended an earlier back-off" do
      backoff.retry?
      backoff.request_submitted
      backoff.request_done

      refute_predicate backoff, :waited_on?
      refute waiting.join(0.2), "the back-off should wait"
    end

    it "forgets the requests counted as waiting, and counts afresh, on a restart after a fork" do
      2.times { backoff.retry? }
      backoff.request_submitted
      backoff.restart

      refute_predicate backoff, :waited_on?
      assert_equal 0, backoff.retry_count
    end

    it "keeps at most one interruption, however many requests are submitted without a back-off" do
      1_000.times do
        backoff.request_submitted
        backoff.request_done
      end

      assert_equal 1, backoff.instance_variable_get(:@interrupts).size
      refute_predicate backoff, :waited_on?
    end

    it "submits a request in a signal trap, ending the back-off under way" do
      backoff.retry?
      thread  = waiting
      results = Array.new(2) { in_signal_trap { backoff.request_submitted } }

      refute(results.any?(StandardError))
      assert thread.join(1), "a request submitted in a signal trap should end the back-off"
      assert_predicate backoff, :waited_on?
    end

    it "knows whether a request is waiting, not counting the process exiting" do
      backoff.exiting = true

      refute_predicate backoff, :request_waiting?

      backoff.request_submitted

      assert_predicate backoff, :request_waiting?

      backoff.request_done

      refute_predicate backoff, :request_waiting?
    end

    it "ignores a request done that was not submitted" do
      backoff.request_done

      refute_predicate backoff, :waited_on?
    end

    it "ends the back-off under way, and does not wait, while the process exits" do
      2.times { backoff.retry? }
      thread          = waiting
      backoff.exiting = true

      assert thread.join(1), "exiting should end the back-off"
      assert_predicate backoff, :waited_on?
      assert waiting.join(0.5), "the back-off should not wait while the process exits"
    end

    it "waits again once the process no longer exits" do
      backoff.retry?
      backoff.exiting = true
      backoff.exiting = false

      refute_predicate backoff, :waited_on?
      refute waiting.join(0.2), "the back-off should wait"
    end

    it "is told that the process exits in a signal trap" do
      backoff.retry?
      thread = waiting
      told   = in_signal_trap do
        backoff.exiting = true
        :told
      end

      assert_equal :told, told
      assert thread.join(1), "exiting should end the back-off"
    end

    it "gives up after a failure only while waited on" do
      error = IOError.new("destination unavailable")

      refute backoff.give_up?(error)
      assert_nil backoff.gave_up
      backoff.request_submitted

      assert backoff.give_up?(error)
      assert_same error, backoff.gave_up
    end

    it "stops giving up once a request has run" do
      2.times { backoff.request_submitted }
      backoff.give_up?(IOError.new("destination unavailable"))
      backoff.request_done

      assert_predicate backoff, :waited_on?, "the second request should still be waiting"
      assert_nil backoff.gave_up
    end

    it "gives up while the process exits, until it no longer does" do
      backoff.exiting = true
      error           = IOError.new("destination unavailable")

      assert backoff.give_up?(error)
      assert_same error, backoff.gave_up
      backoff.exiting = false
      backoff.exiting = true

      assert_nil backoff.gave_up, "exiting again should not give up before a failure"
    end

    it "forgets that it gave up on a restart after a fork" do
      backoff.exiting = true
      backoff.give_up?(IOError.new("destination unavailable"))
      backoff.restart

      assert_nil backoff.gave_up
    end

    # Such as when another thread forks while the parent process flushes at exit.
    it "forgets that the process was exiting on a restart after a fork" do
      backoff.exiting = true
      backoff.restart

      refute_predicate backoff, :waited_on?
      refute backoff.give_up?(IOError.new("destination unavailable"))
    end

    # Returns a fake clock, so that a test never waits out a real back-off, yet sees how long it
    # would have been.
    def fake_clock = BackoffTest::FakeClock.new

    describe "a report that the destination recovered" do
      it "ends the back-off from the failure it is for, at once, and lets the worker thread write again" do
        clock   = fake_clock
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10, clock: clock)
        failure = backoff.retry?(IOError.new("down"))

        assert_predicate failure, :pending?
        assert failure.recovered
        refute_predicate failure, :pending?
        assert_predicate failure, :honoured?
        assert_nil backoff.failure
        assert_equal 1, backoff.recovery_count
        assert_equal 0, backoff.late_recovery_count
      end

      it "ends a back-off the worker thread is waiting out, rather than let it wait the whole time" do
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10)
        failure = backoff.retry?
        thread  = Thread.new { backoff.wait }
        Thread.pass until thread.status == "sleep"

        assert failure.recovered
        assert thread.join(1), "a report of a recovery should end the wait under way"
        assert_nil backoff.failure
      end

      it "does not let the worker thread begin a wait for a failure that recovered before it did" do
        clock   = fake_clock
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10, clock: clock)
        failure = backoff.retry?
        failure.recovered
        backoff.wait

        assert_empty clock.waits, "the worker thread should not wait once the failure has recovered"
      end

      it "ends a back-off only once, whatever the number of reports for its failure" do
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10)
        failure = backoff.retry?

        assert failure.recovered
        refute failure.recovered
        refute failure.recovered
        assert_equal 1, backoff.recovery_count
        assert_equal 2, backoff.late_recovery_count
      end

      it "keeps the count of failures in a row, so a write that fails again is the next retry" do
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10)
        first   = backoff.retry?
        first.recovered

        assert_equal 1, backoff.retry_count
        second = backoff.retry?

        assert_equal 2, second.retry_count
        assert_equal 2, second.id
      end

      it "counts afresh once a message is written" do
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10)
        backoff.retry?.recovered
        backoff.reset

        assert_equal 0, backoff.retry_count
        assert_equal 1, backoff.retry?.retry_count
      end
    end

    describe "a report that arrives too late" do
      it "ends nothing once its back-off ran out, so starts no write" do
        clock   = fake_clock
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10, clock: clock)
        failure = backoff.retry?
        backoff.wait

        assert_nil backoff.failure, "the wait should have ended the back-off"
        refute failure.recovered
        assert_nil backoff.failure
        assert_equal 0, backoff.recovery_count
        assert_equal 1, backoff.late_recovery_count
      end

      it "ends nothing, and leaves the later failure's back-off under way, when it arrives during that one" do
        clock   = fake_clock
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10, clock: clock)
        first   = backoff.retry?
        backoff.wait
        second = backoff.retry?

        refute first.recovered
        assert_same second, backoff.failure, "the late report should not end the back-off from the second failure"
        assert_equal 0, backoff.recovery_count
        assert_equal 1, backoff.late_recovery_count
      end

      it "ends nothing once its back-off was cancelled by a request" do
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10)
        failure = backoff.retry?
        backoff.request_submitted

        refute failure.recovered
        assert_equal 0, backoff.recovery_count
        assert_equal 1, backoff.late_recovery_count
      end

      it "ends nothing once its back-off was cancelled by the process exiting, and cannot start a write" do
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10)
        failure = backoff.retry?
        backoff.exiting = true

        refute failure.recovered
        assert_equal 0, backoff.recovery_count
      end

      it "ends nothing once its back-off was reported recovered already" do
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10)
        failure = backoff.retry?
        failure.recovered

        refute failure.recovered
        assert_equal 1, backoff.late_recovery_count
      end
    end

    describe "the events it records" do
      it "records the failure, the recovery that ended it, the late report, and the write tried again" do
        clock   = fake_clock
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10, clock: clock)
        first   = backoff.retry?(IOError.new("down"))
        first.recovered
        backoff.wait
        second = backoff.retry?
        first.recovered
        second.recovered
        backoff.wait

        assert_equal %i[failure recovery write failure late_recovery recovery write],
                     backoff.events.events.map(&:kind)
        assert_equal [1, 1, 1, 2, 1, 2, 2], backoff.events.events.map(&:failure_id)
        assert_equal 2, backoff.events.count(:recovery)
      end

      it "keeps only the most recent events, so that a long outage cannot grow it without bound" do
        backoff = SemanticLogger::QueueProcessor::Backoff.new(-1)
        limit   = backoff.events.limit
        (limit + 5).times { backoff.retry? }

        assert_equal limit, backoff.events.size
        assert_equal limit + 5, backoff.failures, "the count itself is not bounded"
        assert_equal 6, backoff.events.events.first.failure_id
      end

      it "starts afresh after a fork" do
        backoff = SemanticLogger::QueueProcessor::Backoff.new(10)
        backoff.retry?
        backoff.restart

        assert_equal 0, backoff.failures
        assert_equal 0, backoff.recovery_count
        assert_empty backoff.events.events
      end
    end
  end
end
