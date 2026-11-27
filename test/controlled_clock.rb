# A deterministic monotonic clock for tests that need to reason about elapsed time without sleeping.
#
# It stands in for SemanticLogger.monotonic_clock, returning a reading in seconds that only moves
# when a test advances it, so the duration a measure block takes and the time an entry waits on a
# queue are exact and reproducible. It is an ordinary object, not thread-local, so the worker
# threads of the async appenders read the same clock as the test thread. Mutex-synchronized, since a
# worker thread and the test thread can advance and read it concurrently.
#
# Wall-clock time (Time.now, Log#time) is separate and keeps running for real: this clock drives only
# the monotonic elapsed-time measurements.
class ControlledClock
  def initialize(start = 1_000.0)
    @mutex = Mutex.new
    @now   = start
  end

  # Returns the current monotonic reading in seconds.
  def call
    @mutex.synchronize { @now }
  end

  # Moves the clock forward by the given number of seconds.
  def advance(seconds)
    @mutex.synchronize { @now += seconds }
  end

  # Installs this clock as the clock Semantic Logger measures with, restoring the real one after the
  # block. Use this rather than Minitest's #stub, which is thread-local and so invisible to the
  # background worker threads.
  def use
    original = SemanticLogger.monotonic_clock
    SemanticLogger.monotonic_clock = self
    yield
  ensure
    SemanticLogger.monotonic_clock = original
  end
end
