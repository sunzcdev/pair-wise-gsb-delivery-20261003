require_relative "../test_helper"

class EnqueueTimingTest < Minitest::Test
  # A controlled monotonic clock: readings are seconds, advanced explicitly, so a wait is exact
  # rather than measured from a real sleep.
  class ControlledClock
    def initialize(start = 100.0)
      @now = start
    end

    def call
      @now
    end

    def advance(seconds)
      @now += seconds
    end
  end

  describe SemanticLogger::QueueProcessor::EnqueueTiming do
    let(:clock) { ControlledClock.new }
    let(:timing) { SemanticLogger::QueueProcessor::EnqueueTiming.new(clock: clock) }

    def new_log
      SemanticLogger::Log.new("EnqueueTimingTest", :info)
    end

    it "records no queue duration for an entry it has not marked" do
      log = new_log

      assert_nil timing.stamp(log)
      assert_nil log.queue_duration
    end

    it "measures the wait from the mark to the stamp, in milliseconds" do
      log = new_log
      timing.mark(log)
      clock.advance(0.02) # 20 milliseconds

      assert_in_delta 20.0, timing.stamp(log)
      assert_in_delta 20.0, log.queue_duration
    end

    it "removes a reading once stamped, so a second stamp changes nothing" do
      log = new_log
      timing.mark(log)
      clock.advance(0.02)
      stamped = timing.stamp(log)
      clock.advance(10)

      assert_in_delta 20.0, stamped
      assert_nil timing.stamp(log), "a reading already removed must not be timed again"
      assert_in_delta 20.0, log.queue_duration
    end

    it "keeps the wait of each entry to itself" do
      first  = new_log
      second = new_log
      timing.mark(first)
      clock.advance(0.01)
      timing.mark(second)
      clock.advance(0.01)

      assert_in_delta 20.0, timing.stamp(first)
      assert_in_delta 10.0, timing.stamp(second)
    end

    it "tells identical entries apart, as the queue processor keys them by identity" do
      log_class = SemanticLogger::Log
      refute_same log_class.new("A", :info), log_class.new("A", :info)

      first  = new_log
      second = new_log
      timing.mark(first)
      clock.advance(0.005)
      timing.mark(second)
      clock.advance(0.005)

      # The second entry was marked 5 ms after the first; each wait is measured from its own mark.
      assert_in_delta 5.0, timing.stamp(second)
      clock.advance(0.005)
      assert_in_delta 15.0, timing.stamp(first)
    end

    it "forgets every reading, as after a fork" do
      log = new_log
      timing.mark(log)
      timing.forget

      assert_nil timing.stamp(log)
      assert_nil log.queue_duration
    end

    it "converts seconds to milliseconds" do
      assert_in_delta 1_000.0, SemanticLogger::QueueProcessor::EnqueueTiming.to_milliseconds(1)
      assert_in_delta 1.5, SemanticLogger::QueueProcessor::EnqueueTiming.to_milliseconds(0.0015)
    end
  end
end
