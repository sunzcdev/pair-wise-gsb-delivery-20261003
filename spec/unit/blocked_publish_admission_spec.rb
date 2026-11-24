# frozen_string_literal: true

# Self-contained on purpose: this scenario needs no RabbitMQ node and avoids
# spec_helper's Bundler.require so it also runs with a minimal rspec loader.
$LOAD_PATH.unshift(File.expand_path("../../lib", __dir__)) unless $LOAD_PATH.include?(File.expand_path("../../lib", __dir__))

require "bunny"
require "logger"

# connection.blocked must only park new publish admissions. Consumer acks and
# heartbeats keep flowing, cancelled/timed out waits never send after unblock,
# and publishes already handed to the transport are not replayed.
describe "publish admission under connection.blocked" do
  def wait_until(timeout: 3, &probe)
    deadline = Bunny::Timestamp.monotonic + timeout
    loop do
      result = probe.call
      return result if result

      raise "timed out waiting for condition" if Bunny::Timestamp.monotonic > deadline

      sleep 0.01
    end
  end

  def allocated_channel(connection)
    c = Bunny::Channel.allocate
    c.instance_variable_set(:@id, 1)
    c.instance_variable_set(:@frame_max, 4096)
    c.instance_variable_set(:@connection, connection)
    c.instance_variable_set(:@next_publish_seq_no, 0)
    c.instance_variable_set(:@consumer_delivery_tag_offset, 0)
    c.instance_variable_set(:@publishing_mutex, Mutex.new)
    c.define_singleton_method(:raise_if_no_longer_open!) { nil }
    c
  end

  # Transport/connection double that records control frames and publish
  # framesets. The first frameset can be parked on a barrier to model an
  # in-flight publish that has already been handed to the transport.
  class FakeConnection
    attr_reader :gate

    def initialize(gate, block_first_send: false)
      @gate             = gate
      @block_first_send = block_first_send
      @entered_first    = Queue.new
      @release_first    = Queue.new
      @mutex            = Mutex.new
      @events           = []
      @send_count       = 0
    end

    def publish_gate
      @gate
    end

    def blocked?
      @gate.blocked?
    end

    def entered_first_send
      @entered_first
    end

    def release_first_send!
      @release_first.push(:go)
    end

    def send_frameset(frames, channel)
      blob = frames.map(&:encode).join
      channel.synchronize do
        park = false
        @mutex.synchronize do
          @send_count += 1
          @events << [:publish, blob]
          park = @block_first_send && @send_count == 1
        end
        if park
          @entered_first.push(true)
          @release_first.pop
        end
      end
    end

    def send_frame(frame, *)
      @mutex.synchronize { @events << [:frame, frame] }
    end

    # HeartbeatSender transport target
    def write_without_timeout(data, *)
      @mutex.synchronize { @events << [:raw, data] }
    end

    def events
      @mutex.synchronize { @events.dup }
    end

    def publish_blobs
      events.select { |kind, _| kind == :publish }.map { |_, blob| blob }
    end

    def frame_count
      events.count { |kind, _| kind == :frame }
    end

    def raw_writes
      events.select { |kind, _| kind == :raw }
    end
  end

  describe Bunny::PublishGate do
    it "starts open and admits immediately" do
      gate = Bunny::PublishGate.new
      expect(gate).not_to be_blocked
      expect(gate.wait_for_admission(timeout: 0.1)).to be true
    end

    it "releases all live waiters on unblock" do
      gate = Bunny::PublishGate.new
      gate.block!("disk alarm")

      results = Queue.new
      threads = 3.times.map do
        Thread.new { results << gate.wait_for_admission(timeout: 2) }
      end

      wait_until { gate.waiting_count == 3 }
      gate.unblock!

      threads.each { |t| expect(t.join(2)).to be_truthy }
      expect(Array.new(3) { results.pop(true) rescue nil }).to all(eq(true))
      expect(gate.waiting_count).to eq(0)
    end

    it "removes a cancelled ticket so unblock never admits it" do
      gate = Bunny::PublishGate.new
      gate.block!
      token = Bunny::CancellationToken.new

      worker = Thread.new do
        gate.wait_for_admission(timeout: 2, cancellation: token)
      rescue Bunny::PublishCancelled
        :cancelled
      end

      wait_until { gate.waiting_count == 1 }
      expect(token.cancel).to be true
      expect(worker.value).to eq(:cancelled)
      expect(gate.waiting_count).to eq(0)

      gate.unblock!
      expect(gate.waiting_count).to eq(0)
    end

    it "rejects a pre-cancelled token without registering a waiter" do
      gate = Bunny::PublishGate.new
      gate.block!
      token = Bunny::CancellationToken.new
      token.cancel

      expect do
        gate.wait_for_admission(cancellation: token)
      end.to raise_error(Bunny::PublishCancelled)
      expect(gate.waiting_count).to eq(0)
    end

    it "times out at its deadline and then cannot be admitted by unblock" do
      gate = Bunny::PublishGate.new
      gate.block!

      started = Bunny::Timestamp.monotonic
      expect do
        gate.wait_for_admission(timeout: 0.1)
      end.to raise_error(Bunny::PublishWaitTimeout)
      expect(Bunny::Timestamp.monotonic - started).to be >= 0.08
      expect(gate.waiting_count).to eq(0)

      gate.unblock!
      expect(gate.waiting_count).to eq(0)
    end

    it "fails waiters with the supplied error and rejects future admissions until reset" do
      gate = Bunny::PublishGate.new
      gate.block!

      worker = Thread.new do
        gate.wait_for_admission(timeout: 3)
      rescue StandardError => e
        e
      end

      wait_until { gate.waiting_count == 1 }
      gate.fail_all!(IOError.new("transport gone"))
      expect(worker.value).to be_a(IOError)

      expect { gate.wait_for_admission }.to raise_error(IOError)

      gate.reset!
      expect(gate).not_to be_blocked
      expect(gate.wait_for_admission(timeout: 0.1)).to be true
    end
  end

  describe "Session frame handling" do
    it "drives the publish gate from connection.blocked/unblocked" do
      session = Bunny.new(host: "127.0.0.1", port: 65_534, logger: Logger.new(IO::NULL))

      session.handle_frame(0, AMQ::Protocol::Connection::Blocked.new("memory"))
      expect(session).to be_blocked
      expect(session.publish_gate).to be_blocked

      worker = Thread.new { session.wait_while_blocked(timeout: 2) }
      wait_until { session.publish_gate.waiting_count == 1 }

      session.handle_frame(0, AMQ::Protocol::Connection::Unblocked.new)
      expect(session).not_to be_blocked
      expect(worker.value).to be true
    end
  end

  describe Bunny::Channel do
    it "keeps an in-flight publish, a pending publish and an ack ordered as described" do
      gate = Bunny::PublishGate.new
      gate.block!("resource alarm")
      connection = FakeConnection.new(gate, block_first_send: true)
      channel = allocated_channel(connection)

      # One publish has already been handed to the transport and is stuck there.
      in_flight = Thread.new { channel.basic_publish("in-flight-body", "", "q") }
      connection.entered_first_send.pop

      # A second publish chooses to wait because the connection is blocked.
      token = Bunny::CancellationToken.new
      pending_result = Queue.new
      pending = Thread.new do
        channel.basic_publish("pending-body", "", "q",
                              wait_when_blocked: true, blocked_timeout: 3, cancellation: token)
        pending_result.push(:sent)
      rescue Bunny::PublishCancelled
        pending_result.push(:cancelled)
      end

      wait_until { gate.waiting_count == 1 }

      # The control path is independent: a consumer ack goes out immediately,
      # while blocked and while the in-flight frameset is parked.
      expect { channel.basic_ack(7) }.not_to raise_error
      expect(connection.frame_count).to eq(1)

      # Heartbeats also bypass the publish gate.
      heartbeat = Bunny::HeartbeatSender.new(connection, Logger.new(IO::NULL))
      heartbeat.instance_variable_set(:@interval, 0.4)
      heartbeat.instance_variable_set(:@last_activity_time, Bunny::Timestamp.monotonic - 10)
      heartbeat.send(:beat)
      expect(connection.raw_writes.size).to eq(1)
      expect(connection.raw_writes.first.last).to eq(AMQ::Protocol::HeartbeatFrame.encode)

      # Cancel the pending publish before the broker unblocks.
      expect(token.cancel).to be true
      expect(pending_result.pop).to eq(:cancelled)
      pending.join(2)

      # Unblock changes nothing for the cancelled publish, then release the
      # already in-flight write.
      gate.unblock!
      connection.release_first_send!
      expect(in_flight.join(2)).to be_truthy

      wait_until { connection.publish_blobs.size == 1 }
      sleep 0.05
      blobs = connection.publish_blobs
      expect(blobs.size).to eq(1)
      expect(blobs.first).to include("in-flight-body")
      expect(blobs.first).not_to include("pending-body")
    ensure
      connection&.release_first_send! rescue nil
      in_flight.join(2) if defined?(in_flight) && in_flight
    end

    it "after unblock transmits only the publish that is still genuinely waiting" do
      gate = Bunny::PublishGate.new
      gate.block!
      connection = FakeConnection.new(gate)
      channel = allocated_channel(connection)

      live_token = Bunny::CancellationToken.new
      dead_token = Bunny::CancellationToken.new
      outcomes = Queue.new

      live = Thread.new do
        channel.basic_publish("live-body", "", "q",
                              wait_when_blocked: true, cancellation: live_token)
        outcomes.push(:live_sent)
      rescue StandardError => e
        outcomes.push([:live_error, e.class])
      end

      dead = Thread.new do
        channel.basic_publish("dead-body", "", "q",
                              wait_when_blocked: true, cancellation: dead_token, blocked_timeout: 3)
        outcomes.push(:dead_sent)
      rescue Bunny::PublishCancelled
        outcomes.push(:dead_cancelled)
      end

      wait_until { gate.waiting_count == 2 }
      expect(dead_token.cancel).to be true
      expect(outcomes.pop).to eq(:dead_cancelled)
      dead.join(2)

      gate.unblock!
      expect(outcomes.pop).to eq(:live_sent)
      live.join(2)

      blobs = connection.publish_blobs
      expect(blobs.size).to eq(1)
      expect(blobs.first).to include("live-body")
      expect(blobs.first).not_to include("dead-body")
    end

    it "raises a timeout and never sends a publish whose deadline expired" do
      gate = Bunny::PublishGate.new
      gate.block!
      connection = FakeConnection.new(gate)
      channel = allocated_channel(connection)

      expect do
        channel.basic_publish("late-body", "", "q",
                              wait_when_blocked: true, blocked_timeout: 0.1)
      end.to raise_error(Bunny::PublishWaitTimeout)

      gate.unblock!
      sleep 0.05
      expect(connection.publish_blobs).to be_empty
    end

    it "keeps the default publish path immediate even when the broker is blocked" do
      calls = []
      double = Object.new
      double.define_singleton_method(:blocked?) { true }
      double.define_singleton_method(:send_frameset) { |_, *| calls << :publish }
      double.define_singleton_method(:send_frame) { |_, *| calls << :ack }

      channel = allocated_channel(double)
      channel.basic_publish("message", "", "queue")
      channel.basic_ack(1)

      expect(calls).to eq(%i[publish ack])
    end

    it "supports cancellation and unblock against a minimal blocked? double" do
      blocked = true
      publishes = []
      double = Object.new
      double.define_singleton_method(:blocked?) { blocked }
      double.define_singleton_method(:send_frameset) { |frames, _| publishes << frames.map(&:encode).join }

      channel = allocated_channel(double)
      token = Bunny::CancellationToken.new

      cancelled = Thread.new do
        channel.basic_publish("cancelled-body", "", "q",
                              wait_when_blocked: true, cancellation: token)
      rescue Bunny::PublishCancelled
        :cancelled
      end
      wait_until { true } # let the thread enter the polling wait
      sleep 0.05
      expect(token.cancel).to be true
      expect(cancelled.value).to eq(:cancelled)

      released = Thread.new do
        channel.basic_publish("released-body", "", "q", wait: true, timeout: 2)
        :sent
      end
      sleep 0.05
      blocked = false
      expect(released.value).to eq(:sent)

      expect(publishes.size).to eq(1)
      expect(publishes.first).to include("released-body")
    end

    it "can be driven through the channel-local publish gate without a session" do
      publishes = []
      double = Object.new
      double.define_singleton_method(:send_frameset) { |frames, _| publishes << frames.map(&:encode).join }
      channel = allocated_channel(double)

      channel.publish_gate.block!
      worker = Thread.new do
        channel.basic_publish("local-body", "", "q", wait_when_blocked: true, blocked_timeout: 2)
        :sent
      end
      wait_until { channel.publish_gate.waiting_count == 1 }
      channel.publish_gate.unblock!

      expect(worker.value).to eq(:sent)
      expect(publishes.size).to eq(1)
      expect(publishes.first).to include("local-body")
    end

    it "is released by real Session connection.unblocked frame handling" do
      session = Bunny.new(host: "127.0.0.1", port: 65_534, logger: Logger.new(IO::NULL))
      sent = Queue.new
      session.define_singleton_method(:send_frameset) { |frames, _channel| sent << frames }

      session.handle_frame(0, AMQ::Protocol::Connection::Blocked.new("memory alarm"))
      channel = allocated_channel(session)

      worker = Thread.new do
        channel.basic_publish("wired-body", "", "q", wait_when_blocked: true, blocked_timeout: 2)
      end
      wait_until { session.publish_gate.waiting_count == 1 }
      expect(sent.size).to eq(0)

      session.handle_frame(0, AMQ::Protocol::Connection::Unblocked.new)
      expect(worker.value).to equal(channel)
      expect(sent.size).to eq(1)
    end

    it "allocates the confirm sequence only after admission" do
      gate = Bunny::PublishGate.new
      connection = FakeConnection.new(gate)
      channel = allocated_channel(connection)
      channel.instance_variable_set(:@next_publish_seq_no, 1)
      channel.instance_variable_set(:@unconfirmed_set, Set.new)
      channel.instance_variable_set(:@unconfirmed_set_mutex, Mutex.new)
      channel.instance_variable_set(:@confirms_tracking_enabled, false)
      channel.instance_variable_set(:@throttle_publishes, false)

      gate.block!
      token = Bunny::CancellationToken.new
      worker = Thread.new do
        channel.basic_publish("seq-body", "", "q",
                              wait_when_blocked: true, cancellation: token)
      end
      wait_until { gate.waiting_count == 1 }

      # while still waiting no sequence number was consumed
      expect(channel.unconfirmed_set).to be_empty
      expect(channel.instance_variable_get(:@next_publish_seq_no)).to eq(1)

      gate.unblock!
      worker.join(2)
      expect(worker.value).to equal(channel)
      expect(channel.unconfirmed_set.to_a).to eq([1])
      expect(channel.instance_variable_get(:@next_publish_seq_no)).to eq(2)
    end
  end
end

