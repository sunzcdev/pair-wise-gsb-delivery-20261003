# frozen_string_literal: true

require "thread"

RSpec.describe(Dry::Monads::Task) do
  task = described_class
  result = Dry::Monads::Result
  success = result::Success.method(:new)
  failure = result::Failure.method(:new)
  unit = Dry::Monads::Unit

  # A stand-in for a legacy client that only knows how to register
  # a success and an error callback and how to unsubscribe later.
  # No real network client is involved.
  class FakeCallbackSource
    attr_reader :subscribed, :unsubscribed

    def initialize
      @mutex = Mutex.new
      @subscribed = 0
      @unsubscribed = 0
      @callbacks = {}
    end

    def register(on_success:, on_error:)
      @mutex.synchronize do
        @subscribed += 1
        @callbacks = { on_success: on_success, on_error: on_error }
      end
      -> { @mutex.synchronize { @unsubscribed += 1 } }
    end

    def succeed!(value)
      callbacks = @mutex.synchronize { @callbacks.dup }
      callbacks[:on_success]&.call(value)
    end

    def fail!(error)
      callbacks = @mutex.synchronize { @callbacks.dup }
      callbacks[:on_error]&.call(error)
    end

    def unsubscribe_count
      @mutex.synchronize { @unsubscribed }
    end

    def subscription_count
      @mutex.synchronize { @subscribed }
    end
  end

  def build(source = FakeCallbackSource.new, &registration)
    registration ||= ->(resolve, reject) {
      source.register(on_success: resolve, on_error: reject)
    }
    [described_class.from_callbacks(&registration), source]
  end

  describe ".from_callbacks" do
    it "returns a Task instance composable with fmap" do
      t = task.from_callbacks { nil }
      expect(t).to be_a(described_class)
    end

    it "requires a block" do
      expect { task.from_callbacks }.to raise_error(ArgumentError, /block/)
    end

    describe "async terminal results" do
      it "completes with the value delivered to the success callback" do
        t, source = build
        source.succeed!(42)

        expect(t.wait(5).value!).to be(42)
      end

      it "fails with the exception delivered to the error callback" do
        error = RuntimeError.new("boom")
        t, source = build
        source.fail!(error)

        expect(t.wait(5).to_result).to eql(failure[error])
      end

      it "coerces a non-exception error payload into a RuntimeError" do
        t, source = build
        source.fail!("something broke")

        result = t.wait(5).to_result
        expect(result).to be_a_failure
        expect(result.failure).to be_a(RuntimeError)
        expect(result.failure.message).to eql("something broke")
      end

      it "defaults the success value to Unit when called with no arguments" do
        source = FakeCallbackSource.new
        t = task.from_callbacks do |resolve, reject|
          source.register(on_success: resolve, on_error: reject).tap { resolve.call }
        end

        expect(t.wait(5).value!).to be(unit)
      end
    end

    describe "synchronous callbacks fired during registration" do
      it "resolves immediately when the success callback fires synchronously" do
        source = FakeCallbackSource.new
        t = task.from_callbacks do |resolve, _reject|
          source.register(on_success: resolve, on_error: -> {})
          resolve.call(:sync)
          nil
        end

        expect(t).to be_complete
        expect(t.value!).to be(:sync)
      end

      it "rejects immediately when the error callback fires synchronously" do
        error = RuntimeError.new("sync boom")
        source = FakeCallbackSource.new
        t = task.from_callbacks do |_resolve, reject|
          source.register(on_success: -> {}, on_error: reject)
          reject.call(error)
          nil
        end

        expect(t).to be_complete
        expect(t.to_result).to eql(failure[error])
      end

      it "unsubscribes after the synchronous result was delivered" do
        source = FakeCallbackSource.new
        t = task.from_callbacks do |resolve, _reject|
          handle = source.register(on_success: resolve, on_error: -> {})
          resolve.call(:sync)
          handle
        end

        expect(t.value!).to be(:sync)
        expect(source.unsubscribe_count).to be(1)
      end
    end

    describe "first terminal state wins" do
      it "ignores later results following a synchronous success" do
        source = FakeCallbackSource.new
        t = task.from_callbacks do |resolve, reject|
          handle = source.register(on_success: resolve, on_error: reject)
          resolve.call(:first)
          reject.call(RuntimeError.new("too late"))
          handle
        end

        expect(t.wait(5).value!).to be(:first)
      end

      it "keeps the synchronous success when registration raises afterwards" do
        source = FakeCallbackSource.new
        t = task.from_callbacks do |resolve, reject|
          source.register(on_success: resolve, on_error: reject)
          resolve.call(:won)
          raise "registration blew up after the result arrived"
        end

        expect(t.wait(5).value!).to be(:won)
      end

      it "keeps the synchronous error when registration raises afterwards" do
        error = RuntimeError.new("first error")
        source = FakeCallbackSource.new
        t = task.from_callbacks do |resolve, reject|
          source.register(on_success: resolve, on_error: reject)
          reject.call(error)
          raise "second, irrelevant error"
        end

        expect(t.wait(5).to_result).to eql(failure[error])
      end

      it "resolves when two threads race with different results" do
        outcomes = Array.new(30) do
          source = FakeCallbackSource.new
          t = task.from_callbacks do |resolve, reject|
            source.register(on_success: resolve, on_error: reject)
          end

          start = Queue.new
          threads = [
            Thread.new { start.pop; source.succeed!(:ok) },
            Thread.new { start.pop; source.fail!(RuntimeError.new("nope")) }
          ]
          2.times { start << :go }
          threads.each(&:join)

          t.wait(5).to_result
        end

        outcomes.each do |result|
          if result.success?
            expect(result.value!).to be(:ok)
          else
            expect(result.failure).to be_a(RuntimeError)
            expect(result.failure.message).to eql("nope")
          end
        end
      end

      it "always settles exactly once under a success/error race" do
        50.times do
          source = FakeCallbackSource.new
          t = task.from_callbacks do |resolve, reject|
            source.register(on_success: resolve, on_error: reject)
          end

          start = Queue.new
          threads = [
            Thread.new { start.pop; source.succeed!(:ok) },
            Thread.new { start.pop; source.fail!(RuntimeError.new("bad")) }
          ]
          2.times { start << :go }
          threads.each(&:join)

          expect(t.wait(5)).to be_complete
          expect(source.unsubscribe_count).to be(1)
        end
      end
    end

    describe "registration failures" do
      it "fails the task when registration raises before any callback fires" do
        error = RuntimeError.new("cannot register")
        t = task.from_callbacks { raise error }

        expect(t.wait(5).to_result).to eql(failure[error])
      end

      it "does not hang waiters on a raising registration" do
        t = task.from_callbacks { raise "boom" }
        expect { t.wait(5).value! }.to raise_error(RuntimeError, "boom")
      end

      it "swallows an exception raised after the terminal state arrived" do
        t = task.from_callbacks do |resolve, _reject|
          resolve.call(:fine)
          raise "ignored"
        end

        expect(t.value!).to be(:fine)
      end
    end

    describe "unsubscribe handle" do
      it "returns nil and subscribes/unsubscribes exactly once on success" do
        t, source = build
        source.succeed!(1)
        t.wait(5)

        expect(source.subscription_count).to be(1)
        expect(source.unsubscribe_count).to be(1)
      end

      it "unsubscribes even when the task fails" do
        t, source = build
        source.fail!(RuntimeError.new("x"))
        t.wait(5)

        expect(source.unsubscribe_count).to be(1)
      end

      it "does not miss a handle returned after the terminal state already fired" do
        order = []
        handle = -> { order << :unsubscribed }

        t = task.from_callbacks do |resolve, _reject|
          resolve.call(:ok)
          order << :settled
          # Simulate a client that hands back the unsubscribe handle only
          # after it has already fired the success callback synchronously.
          order << :handle
          handle
        end

        expect(t.value!).to be(:ok)
        expect(order).to eql(%i[settled handle unsubscribed])
      end

      it "invokes a late handle right after it is returned for an async result" do
        source = FakeCallbackSource.new
        handle_returned = Queue.new

        t = task.from_callbacks do |resolve, reject|
          h = source.register(on_success: resolve, on_error: reject)
          Thread.new do
            source.succeed!(:async)
            # Handle is "returned" to the registrar only after the result
            # already fired on another thread.
            handle_returned << h
          end
          handle_returned.pop
        end

        expect(t.wait(5).value!).to be(:async)
        expect(source.unsubscribe_count).to be(1)
      end

      it "tolerates a handle raising while cleaning up" do
        source = FakeCallbackSource.new
        bad_handle = -> { raise "cleanup failed" }

        t = task.from_callbacks do |resolve, _reject|
          source.register(on_success: resolve, on_error: -> {})
          resolve.call(:ok)
          bad_handle
        end

        expect(t.value!).to be(:ok)
      end

      it "accepts nil as a no-op handle" do
        t = task.from_callbacks { |resolve, _| resolve.call(:ok); nil }
        expect(t.value!).to be(:ok)
      end

      it "rejects a handle that cannot be called" do
        expect {
          task.from_callbacks { |_, _| 123 }
        }.to raise_error(ArgumentError, /unsubscribe handle/)
      end
    end

    describe "composition with the rest of the Task API" do
      it "chains with fmap" do
        t, source = build
        chained = t.fmap { |v| v + 1 }.fmap { |v| v * 2 }
        source.succeed!(10)

        expect(chained.wait(5).value!).to be(22)
      end

      it "chains with bind" do
        t, source = build
        chained = t.bind { |v| described_class.new { v + 5 } }
        source.succeed!(1)

        expect(chained.wait(5).value!).to be(6)
      end

      it "recovers with or" do
        t, source = build
        chained = t.or { described_class.pure(:recovered) }
        source.fail!(RuntimeError.new("down"))

        expect(chained.wait(5).value!).to be(:recovered)
      end

      it "keeps or from running on success" do
        t, source = build
        chained = t.or { described_class.pure(:recovered) }
        source.succeed!(:ok)

        expect(chained.wait(5).value!).to be(:ok)
      end

      it "works with or_fmap" do
        t, source = build
        chained = t.or_fmap { :fixed }
        source.fail!(RuntimeError.new("down"))

        expect(chained.wait(5).to_result).to eql(success[:fixed])
      end

      it "converts to a Result::Success" do
        t, source = build
        source.succeed!(7)
        expect(t.wait(5).to_result).to eql(success[7])
      end
    end
  end
end
