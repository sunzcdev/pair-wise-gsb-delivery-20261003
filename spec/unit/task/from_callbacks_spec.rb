# frozen_string_literal: true

require "thread"

RSpec.describe(Dry::Monads::Task) do
  # A fake stand-in for a legacy client that only supports registering
  # success/error callbacks and returns an opaque subscription handle.
  # It is scriptable so that tricky registration timings can be
  # reproduced deterministically, without any real network involved.
  class FakeLegacyClient
    # @return [Integer] how many times a handle was unsubscribed
    attr_reader :unsubscribed_count

    def initialize
      @mutex = Mutex.new
      @subscriptions = []
      @unsubscribed_count = 0
    end

    # Registers a pair of callbacks.
    #
    # @param before [#call,nil] invoked synchronously, right after the
    #   callbacks are registered but before the handle is returned
    # @return [Subscription]
    def submit(on_success:, on_error:, before: nil)
      subscription = Subscription.new(self)
      @mutex.synchronize { @subscriptions << [subscription, on_success, on_error] }

      before&.call(on_success, on_error)

      subscription
    end

    # @return [void]
    def deliver_success(value)
      each_active { |on_success,| on_success.call(value) }
    end

    # @return [void]
    def deliver_error(error)
      each_active { |_, on_error| on_error.call(error) }
    end

    # Fires success and error virtually simultaneously from two threads.
    #
    # @return [void]
    def deliver_racing(value, error)
      barrier = Queue.new

      threads = [
        Thread.new { barrier.pop; deliver_success(value) },
        Thread.new { barrier.pop; deliver_error(error) }
      ]

      2.times { barrier << :go }
      threads.each(&:join)
    end

    # @return [void]
    def unsubscribe(subscription)
      @mutex.synchronize do
        removed = @subscriptions.reject! { |(sub, *)| sub.equal?(subscription) }
        @unsubscribed_count += 1 if removed
      end
    end

    private

    # @return [void]
    def each_active
      callbacks = @mutex.synchronize do
        entry = @subscriptions.find { |(sub, *)| !sub.unsubscribed? }
        entry && entry[1..]
      end
      yield(callbacks) if callbacks
    end

    # Opaque handle: only the client knows how to release the subscription.
    class Subscription
      # @param client [FakeLegacyClient]
      def initialize(client)
        @client = client
        @mutex = Mutex.new
        @unsubscribed = false
      end

      # @return [Boolean]
      def unsubscribed?
        @mutex.synchronize { @unsubscribed }
      end

      # @return [void]
      def unsubscribe!
        @mutex.synchronize { @unsubscribed = true }
        @client.unsubscribe(self)
      end

      # @return [Method] the callable returned from the registration block
      def to_unsubscriber
        method(:unsubscribe!)
      end
    end
  end

  let(:client) { FakeLegacyClient.new }

  def from_callbacks(&block)
    described_class.from_callbacks(&block)
  end

  # Registers callbacks on the fake client the way real glue code would.
  #
  # @return [Proc]
  def register(client, **options)
    lambda do |on_success, on_error|
      handle = client.submit(on_success: on_success, on_error: on_error, **options)
      handle.to_unsubscriber
    end
  end

  def resolve(task)
    task.value!
  rescue StandardError => e
    e.message.to_sym
  end

  describe ".from_callbacks" do
    it "requires a block" do
      expect { described_class.from_callbacks }.to raise_error(
        ArgumentError, /a block is required/
      )
    end

    it "completes with the value of an asynchronous success callback" do
      task = from_callbacks(&register(client))
      Thread.new { client.deliver_success(:done) }

      expect(task.value!).to be(:done)
    end

    it "fails with the exception delivered to the error callback" do
      task = from_callbacks(&register(client))
      Thread.new { client.deliver_error(RuntimeError.new("boom")) }

      expect { task.value! }.to raise_error(RuntimeError, "boom")
    end

    it "unsubscribes exactly once after an asynchronous completion" do
      task = from_callbacks(&register(client))
      Thread.new { client.deliver_success(1) }
      task.wait

      expect(client.unsubscribed_count).to be(1)
    end

    context "when a callback fires synchronously while registering" do
      it "completes even though the handle is returned later" do
        task = from_callbacks do |on_success, on_error|
          handle = client.submit(
            on_success: on_success,
            on_error: on_error,
            before: ->(ok, _) { ok.call(:sync) }
          )
          handle.to_unsubscriber
        end

        expect(task.value!).to be(:sync)
        expect(client.unsubscribed_count).to be(1)
      end

      it "keeps the success when the block raises after firing it" do
        task = from_callbacks do |on_success, on_error|
          client.submit(
            on_success: on_success,
            on_error: on_error,
            before: ->(ok, _) { ok.call(:sync) }
          )
          raise "registered but blew up afterwards"
        end

        expect(task.value!).to be(:sync)
      end

      it "ignores an error following a synchronous success" do
        task = from_callbacks do |on_success, on_error|
          handle = client.submit(
            on_success: on_success,
            on_error: on_error,
            before: proc do |ok, err|
              ok.call(:sync)
              err.call(RuntimeError.new("late"))
            end
          )
          handle.to_unsubscriber
        end

        expect(task.value!).to be(:sync)
        expect(client.unsubscribed_count).to be(1)
      end

      it "still unsubscribes when the handle is returned well after the signal" do
        task = from_callbacks do |on_success, on_error|
          handle = client.submit(
            on_success: on_success,
            on_error: on_error,
            before: ->(ok, _) { ok.call(:sync) }
          )
          sleep 0.05
          handle.to_unsubscriber
        end

        expect(task.wait(5).value!).to be(:sync)
        expect(client.unsubscribed_count).to be(1)
      end
    end

    context "when registration fails" do
      it "fails the task rather than hanging when no callback ever fires" do
        task = from_callbacks do |_, _|
          raise "could not register at all"
        end

        expect { task.wait(1).value! }.to raise_error(RuntimeError, "could not register at all")
      end

      it "prefers the first signal over an exception raised afterwards" do
        task = from_callbacks do |on_success, _|
          on_success.call(42)
          raise "ignored because the result already arrived"
        end

        expect(task.value!).to be(42)
      end
    end

    context "when terminal signals compete from different threads" do
      it "never hangs and every task settles on one of the two outcomes" do
        outcomes = Array.new(100) do
          local_client = FakeLegacyClient.new
          task = from_callbacks(&register(local_client))
          local_client.deliver_racing(:good, RuntimeError.new("bad"))

          expect(task.wait(5)).to be_complete
          resolve(task)
        end

        expect(outcomes).to all(satisfy { |outcome| %i[good bad].include?(outcome) })
      end

      it "unsubscribes exactly once despite the racing signals" do
        task = from_callbacks(&register(client))
        client.deliver_racing(:good, RuntimeError.new("bad"))
        task.wait(5)

        expect(client.unsubscribed_count).to be(1)
      end

      it "unsubscribes exactly once when the signal wins the race with the handle" do
        # The success callback fires from another thread before this block
        # returns the handle, i.e. the handle "comes back late".
        task = from_callbacks do |on_success, on_error|
          handle = client.submit(on_success: on_success, on_error: on_error)
          client.deliver_success(:early)
          # give the signal a chance to be processed first
          sleep 0.02
          handle.to_unsubscriber
        end

        expect(task.wait(5).value!).to be(:early)
        expect(client.unsubscribed_count).to be(1)
      end

      it "ignores any signal delivered after the first terminal one" do
        task = from_callbacks do |on_success, on_error|
          on_success.call(:first)
          on_error.call(RuntimeError.new("second"))
          on_success.call(:third)
          nil
        end

        expect(task.value!).to be(:first)
      end
    end

    context "unsubscribe handles" do
      it "works when the block returns nil instead of a handle" do
        task = from_callbacks do |on_success, _|
          on_success.call(:ok)
          nil
        end

        expect(task.value!).to be(:ok)
      end

      it "swallows errors raised while unsubscribing" do
        task = from_callbacks do |on_success, _|
          on_success.call(:ok)
          -> { raise "unsubscribe failed" }
        end

        expect(task.value!).to be(:ok)
      end

      it "rejects handles that aren't callable" do
        expect {
          from_callbacks do |_, _|
            123
          end.wait
        }.to raise_error(TypeError, /must respond to #call/)
      end

      it "ignores a non-callable handle that follows a terminal signal" do
        task = from_callbacks do |on_success, _|
          on_success.call(:ok)
          123
        end

        expect(task.value!).to be(:ok)
      end
    end

    context "error reasons" do
      it "wraps non-exception reasons in StandardError" do
        task = from_callbacks do |_, on_error|
          on_error.call("plain string reason")
          nil
        end

        expect { task.value! }.to raise_error(StandardError, /plain string reason/)
      end
    end

    describe "composition" do
      it "chains fmap" do
        task = from_callbacks do |on_success, _|
          Thread.new { on_success.call(21) }
          nil
        end.fmap { |value| value * 2 }

        expect(task.value!).to be(42)
      end

      it "chains bind" do
        task = from_callbacks do |on_success, _|
          on_success.call(7)
          nil
        end.bind { |value| described_class.pure(value + 1) }

        expect(task.value!).to be(8)
      end

      it "recovers failures with or" do
        task = from_callbacks do |_, on_error|
          Thread.new { on_error.call(RuntimeError.new("nope")) }
          nil
        end.or { described_class.pure(:recovered) }

        expect(task.value!).to be(:recovered)
      end

      it "doesn't run the or-block on success" do
        task = from_callbacks do |on_success, _|
          on_success.call(:fine)
          nil
        end.or { described_class.pure(:ignored) }

        expect(task.value!).to be(:fine)
      end

      it "converts to Result::Success / Result::Failure" do
        success = from_callbacks { |ok, _| ok.call(1); nil }.to_result
        expect(success).to eql(Dry::Monads::Result::Success.new(1))

        error = RuntimeError.new("x")
        failure = from_callbacks { |_, err| err.call(error); nil }.to_result
        expect(failure).to be_a_failure
        expect(failure.failure).to be(error)
      end

      it "works inside Do notation" do
        operation = Class.new do
          include Dry::Monads::Do.for(:call)
          include Dry::Monads::Task::Mixin

          def initialize(source)
            @source = source
          end

          def call
            value = yield Dry::Monads::Task.from_callbacks { |ok, _| ok.call(@source); nil }
            Task { value * 2 }
          end
        end

        expect(operation.new(21).call.value!).to be(42)
      end
    end
  end
end
