require_relative "../test_helper"

class ReopenerTest < Minitest::Test
  describe SemanticLogger::QueueProcessor::Reopener do
    let(:internal) { SemanticLogger::Test::CaptureLogEvents.new }

    let(:appender) do
      appender = Class.new(SemanticLogger::Test::CaptureLogEvents) do
        attr_accessor :reopen_error

        def reopens = @reopens ||= 0

        def reopen
          @reopens = reopens + 1
          raise reopen_error if reopen_error
        end
      end.new
      appender.logger = internal
      appender
    end

    let(:reporter) { SemanticLogger::QueueProcessor::Reporter.new(appender) }
    let(:reopener) { SemanticLogger::QueueProcessor::Reopener.new(appender, reporter) }

    it "does not reopen the appender until asked" do
      reopener.reopen_if_asked

      assert_equal 0, appender.reopens
    end

    it "reopens the appender once for the times it was asked since it was last reopened" do
      3.times { reopener.ask }
      2.times { reopener.reopen_if_asked }
      reopener.ask
      reopener.reopen_if_asked

      assert_equal 2, appender.reopens
    end

    it "counts each time it reopens the appender, so a switch of destination is checkable" do
      assert_equal 0, reopener.reopen_count

      reopener.ask
      reopener.reopen_if_asked

      assert_equal 1, reopener.reopen_count

      # Reopens asked for before it runs again are one reopen, and one count.
      3.times { reopener.ask }
      reopener.reopen_if_asked

      assert_equal 2, reopener.reopen_count
      assert_equal 2, appender.reopens
    end

    it "counts a reopen that fails, which still tried to change the destination" do
      appender.reopen_error = SocketError.new("getaddrinfo: nodename nor servname provided, or not known")
      reopener.ask
      reopener.reopen_if_asked

      assert_equal 1, reopener.reopen_count
    end

    it "reports a reopen that fails, rather than raising it" do
      appender.reopen_error = SocketError.new("getaddrinfo: nodename nor servname provided, or not known")
      reopener.ask
      reopener.reopen_if_asked

      assert_equal ["Async: Failed to reopen the appender"], internal.events.map(&:message)
      assert_same appender.reopen_error, internal.events.first.exception
    end

    it "reports a reopen that raises an exception that is not a StandardError, rather than raising it" do
      appender.reopen_error = LoadError.new("cannot load such file -- missing_client")
      reopener.ask

      assert_nil reopener.reopen_if_asked
      assert_same appender.reopen_error, internal.events.first&.exception
    end

    it "reports a reopen that overflows the stack, and returns the error for the worker thread to hand over" do
      appender.reopen_error = SystemStackError.new("stack level too deep")
      reopener.ask

      assert_same appender.reopen_error, reopener.reopen_if_asked
      assert_same appender.reopen_error, internal.events.first&.exception
    end

    it "does nothing for an appender that cannot be reopened" do
      reopener = SemanticLogger::QueueProcessor::Reopener.new(SemanticLogger::Test::CaptureLogEvents.new, reporter)
      reopener.ask

      assert_nil reopener.reopen_if_asked
    end
  end
end
