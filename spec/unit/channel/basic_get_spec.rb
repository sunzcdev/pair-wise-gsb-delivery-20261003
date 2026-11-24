# frozen_string_literal: true
require "spec_helper"

describe Bunny::Channel, "basic.get mixed with consumer deliveries" do
  # A channel can carry both a long-running consumer and occasional synchronous
  # basic.get calls. Frames for the two flows arrive on the same connection and
  # are demultiplexed by the reader loop, so their continuations must never be
  # cross-wired.
  let(:connection) { FakeBunnyConnection.new }
  let(:channel) do
    ch = Bunny::Channel.new(connection, 1)
    ch.instance_variable_set(:@status, :open)
    ch
  end

  let(:properties) { {content_type: "application/octet-stream"} }

  after do
    channel.work_pool.shutdown if channel.work_pool&.running?
  end

  describe "frame attribution" do
    it "routes a basic.get-ok frameset to the get continuation" do
      get_ok = AMQ::Protocol::Basic::GetOk.new(1, false, "amq.direct", "rk", 0)

      channel.handle_basic_get_ok(get_ok, properties, "fetched")

      expect(channel.instance_variable_get(:@basic_get_continuations).size).to eq 1
      delivery_info, props, content = channel.wait_on_basic_get_continuations
      expect(delivery_info).to equal(get_ok)
      expect(props).to equal(properties)
      expect(content).to eq("fetched")
    end

    it "keeps object identity of every frameset member" do
      get_ok = AMQ::Protocol::Basic::GetOk.new(2, true, "amq.topic", "rk", 3)
      payload = "payload"

      channel.handle_basic_get_ok(get_ok, properties, payload)
      delivery_info, props, content = channel.wait_on_basic_get_continuations

      expect(delivery_info).to be(get_ok)
      expect(props).to be(properties)
      expect(content).to be(payload)
    end

    it "routes a basic.get-empty frame as a triple of nils" do
      channel.handle_basic_get_empty(AMQ::Protocol::Basic::GetEmpty.new(""))

      expect(channel.wait_on_basic_get_continuations).to eq([nil, nil, nil])
    end

    it "routes a basic.deliver frameset to the registered consumer" do
      delivered = Queue.new
      consumer = register_consumer(channel, "consumer.tag") do |info, props, content|
        delivered << [info, props, content]
      end
      deliver = AMQ::Protocol::Basic::Deliver.new(consumer.consumer_tag, 1, false, "amq.direct", "rk")

      channel.handle_frameset(deliver, properties, "pushed")

      info, props, content = delivered.pop
      expect(info).to be_a(Bunny::DeliveryInfo)
      expect(info.consumer).to equal(consumer)
      expect(props).to be_a(Bunny::MessageProperties)
      expect(content).to eq("pushed")
      expect(channel.instance_variable_get(:@basic_get_continuations)).to be_empty
    end

    it "keeps interleaved get-ok, get-empty and consumer deliveries apart" do
      delivered = Queue.new
      consumer = register_consumer(channel, "consumer.tag") { |_, _, content| delivered << content }

      get_ok_one = AMQ::Protocol::Basic::GetOk.new(1, false, "amq.direct", "rk", 1)
      get_ok_two = AMQ::Protocol::Basic::GetOk.new(3, false, "amq.direct", "rk", 0)
      deliver_one = AMQ::Protocol::Basic::Deliver.new(consumer.consumer_tag, 2, false, "amq.direct", "rk")
      deliver_two = AMQ::Protocol::Basic::Deliver.new(consumer.consumer_tag, 4, false, "amq.direct", "rk")

      channel.handle_basic_get_ok(get_ok_one, properties, "get-1")
      channel.handle_frameset(deliver_one, properties, "push-1")
      channel.handle_basic_get_empty(AMQ::Protocol::Basic::GetEmpty.new(""))
      channel.handle_frameset(deliver_two, properties, "push-2")
      channel.handle_basic_get_ok(get_ok_two, properties, "get-2")

      triples = 3.times.map { channel.wait_on_basic_get_continuations }
      expect(triples.map { |triple| triple.last }).to eq(["get-1", nil, "get-2"])
      expect([delivered.pop, delivered.pop]).to contain_exactly("push-1", "push-2")
    end

    it "matches concurrent get waiters with their own responses under interleaved deliveries" do
      delivered = Queue.new
      consumer = register_consumer(channel, "consumer.tag") { |_, _, content| delivered << content }

      results = Array.new(4) { nil }
      threads = Array.new(4) do |i|
        Thread.new do
          begin
            results[i] = channel.basic_get("bunny.test.queue")
          rescue => e
            results[i] = e
          end
        end
      end
      wait_until { channel.instance_variable_get(:@threads_waiting_on_basic_get_continuations).size == 4 }

      get_oks = 4.times.map { |i| AMQ::Protocol::Basic::GetOk.new(i + 1, false, "amq.direct", "rk", 0) }
      deliver = AMQ::Protocol::Basic::Deliver.new(consumer.consumer_tag, 9, false, "amq.direct", "rk")
      channel.handle_frameset(deliver, properties, "pushed")
      get_oks.each_with_index { |get_ok, i| channel.handle_basic_get_ok(get_ok, properties, "get-#{i}") }

      threads.each { |t| expect(t.join(2)).to eq(t) }
      payloads = results.map { |triple| triple.last }.sort
      expect(payloads).to eq(4.times.map { |i| "get-#{i}" }.sort)
      expect(delivered.pop).to eq("pushed")
    end
  end

  describe "delivery tags" do
    it "counts get-ok and consumer deliveries through the same monotonic sequence" do
      register_consumer(channel, "consumer.tag")
      get_ok = AMQ::Protocol::Basic::GetOk.new(1, false, "amq.direct", "rk", 0)
      deliver = AMQ::Protocol::Basic::Deliver.new("consumer.tag", 1, false, "amq.direct", "rk")

      channel.handle_basic_get_ok(get_ok, properties, "get")
      channel.handle_frameset(deliver, properties, "push")

      info, = channel.wait_on_basic_get_continuations
      expect(info.delivery_tag).to eq(1)
      expect(channel.send(:next_consumer_delivery_tag, 2)).to eq(2)
    end
  end

  describe "synchronous basic.get" do
    it "completes when the get-ok frameset arrives from another thread" do
      get_ok = AMQ::Protocol::Basic::GetOk.new(1, false, "amq.direct", "rk", 0)
      Thread.new do
        sleep 0.1
        channel.handle_basic_get_ok(get_ok, properties, "fetched")
      end

      info, props, content = channel.basic_get("bunny.test.queue")
      expect(info).to equal(get_ok)
      expect(props).to equal(properties)
      expect(content).to eq("fetched")
    end

    it "returns a triple of nils for an empty queue" do
      Thread.new do
        sleep 0.1
        channel.handle_basic_get_empty(AMQ::Protocol::Basic::GetEmpty.new(""))
      end

      expect(channel.basic_get("bunny.test.queue")).to eq([nil, nil, nil])
    end

    it "raises a timeout when no frameset arrives and leaves no waiter behind" do
      expect { channel.basic_get("bunny.test.queue") }.to raise_error(Timeout::Error)

      waiting = channel.instance_variable_get(:@threads_waiting_on_basic_get_continuations)
      expect(waiting).to be_empty
    end

    it "queues a get-ok arriving after a timeout instead of attributing it to a later get" do
      expect { channel.basic_get("bunny.test.queue") }.to raise_error(Timeout::Error)

      late_get_ok = AMQ::Protocol::Basic::GetOk.new(9, false, "amq.direct", "rk", 0)
      channel.handle_basic_get_ok(late_get_ok, properties, "late")

      # the stale response stays attached to the timed-out get, the next get
      # simply consumes it in FIFO order rather than silently dropping it
      info, _, content = channel.wait_on_basic_get_continuations
      expect(info).to equal(late_get_ok)
      expect(content).to eq("late")
      expect(channel.instance_variable_get(:@basic_get_continuations)).to be_empty
    end

    it "is unblocked by a server-sent channel.close and surfaces the channel error" do
      thread_result = nil
      thread = Thread.new do
        begin
          channel.basic_get("bunny.test.queue")
        rescue => e
          thread_result = e
        end
      end

      wait_until { channel.instance_variable_get(:@threads_waiting_on_basic_get_continuations).any? }
      channel.handle_method(AMQ::Protocol::Channel::Close.new(404, "no queue", 60, 70))
      expect(thread.join(2)).to eq(thread)

      expect(thread_result).to be_a(Bunny::NotFound)
      expect(channel).to be_closed
    end

    it "is unblocked when the channel is closed locally" do
      thread_result = nil
      thread = Thread.new do
        begin
          channel.basic_get("bunny.test.queue")
        rescue => e
          thread_result = e
        end
      end

      wait_until { channel.instance_variable_get(:@threads_waiting_on_basic_get_continuations).any? }
      channel.close
      expect(thread.join(2)).to eq(thread)

      expect(thread_result).to be_a(Bunny::ChannelAlreadyClosed)
      expect(channel).to be_closed
    end

    it "is unblocked when the connection goes down" do
      thread_result = nil
      thread = Thread.new do
        begin
          channel.basic_get("bunny.test.queue")
        rescue => e
          thread_result = e
        end
      end

      wait_until { channel.instance_variable_get(:@threads_waiting_on_basic_get_continuations).any? }
      channel.connection_closed!
      expect(thread.join(2)).to eq(thread)

      expect(thread_result).to be_a(Bunny::ChannelAlreadyClosed)
    end

    it "wakes up promptly instead of sleeping through the continuation timeout when closed" do
      thread_result = nil
      thread = Thread.new do
        begin
          channel.basic_get("bunny.test.queue")
        rescue => e
          thread_result = e
        end
      end

      wait_until { channel.instance_variable_get(:@threads_waiting_on_basic_get_continuations).any? }
      started = Bunny::Timestamp.monotonic
      channel.connection_closed!
      thread.join(2)
      elapsed = Bunny::Timestamp.monotonic - started

      expect(thread_result).to be_a(Bunny::ChannelAlreadyClosed)
      expect(elapsed).to be < 1
    end

    it "unblocks every concurrent get waiter when the channel closes" do
      results = Array.new(3)
      threads = Array.new(3) do |i|
        Thread.new do
          begin
            channel.basic_get("bunny.test.queue")
          rescue => e
            results[i] = e
          end
        end
      end
      wait_until { channel.instance_variable_get(:@threads_waiting_on_basic_get_continuations).size == 3 }

      channel.connection_closed!
      threads.each { |t| expect(t.join(2)).to eq(t) }

      expect(results).to all(be_a(Bunny::ChannelAlreadyClosed))
    end

    it "does not leave close sentinels in the continuation queue" do
      thread = Thread.new { channel.basic_get("bunny.test.queue") rescue nil }
      wait_until { channel.instance_variable_get(:@threads_waiting_on_basic_get_continuations).any? }

      channel.connection_closed!
      expect(thread.join(2)).to eq(thread)
      expect(channel.instance_variable_get(:@basic_get_continuations)).to be_empty
    end
  end

  describe "pausing consumer callbacks" do
    it "buffers a delivery while paused and delivers it after resume" do
      delivered = Queue.new
      consumer = register_consumer(channel, "consumer.tag") { |_, _, content| delivered << content }
      deliver = AMQ::Protocol::Basic::Deliver.new(consumer.consumer_tag, 1, false, "amq.direct", "rk")

      channel.maybe_pause_consumer_work_pool!
      channel.handle_frameset(deliver, properties, "while-paused")
      expect(delivered).to be_empty

      channel.work_pool.resume
      expect(delivered.pop).to eq("while-paused")
    end

    it "alternates a pushed delivery and an empty get without cross-delivery" do
      pushed = Queue.new
      consumer = register_consumer(channel, "consumer.tag") { |_, _, content| pushed << content }

      channel.maybe_pause_consumer_work_pool!
      deliver = AMQ::Protocol::Basic::Deliver.new(consumer.consumer_tag, 1, false, "amq.direct", "rk")
      channel.handle_frameset(deliver, properties, "pushed")

      thread = Thread.new do
        sleep 0.05
        channel.handle_basic_get_empty(AMQ::Protocol::Basic::GetEmpty.new(""))
      end
      expect(channel.basic_get("bunny.test.queue")).to eq([nil, nil, nil])
      thread.join

      expect(pushed).to be_empty
      channel.work_pool.resume
      expect(pushed.pop).to eq("pushed")
    end
  end

  describe "recovery" do
    it "releases a blocked get when the channel recovers from a network failure" do
      thread_result = nil
      thread = Thread.new do
        begin
          channel.basic_get("bunny.test.queue")
        rescue => e
          thread_result = e
        end
      end
      wait_until { channel.instance_variable_get(:@threads_waiting_on_basic_get_continuations).any? }

      allow(channel).to receive(:recover_prefetch_setting)
      allow(channel).to receive(:recover_confirm_mode)
      allow(channel).to receive(:recover_tx_mode)
      channel.recover_from_network_failure
      expect(thread.join(2)).to eq(thread)

      expect(thread_result).to be_a(Bunny::ChannelAlreadyClosed)
    end

    it "keeps delivering reopened-channel pushes through the same consumer" do
      delivered = Queue.new
      consumer = register_consumer(channel, "consumer.tag") { |_, _, content| delivered << content }

      channel.send(:closed!)
      allow(channel).to receive(:recover_prefetch_setting)
      allow(channel).to receive(:recover_confirm_mode)
      allow(channel).to receive(:recover_tx_mode)
      channel.reopen

      deliver = AMQ::Protocol::Basic::Deliver.new(consumer.consumer_tag, 1, false, "amq.direct", "rk")
      channel.handle_frameset(deliver, properties, "after-reopen")

      expect(delivered.pop).to eq("after-reopen")
    end
  end

  def register_consumer(channel, tag, &block)
    consumer = Bunny::Consumer.new(channel, "bunny.test.queue", tag)
    consumer.on_delivery(&block)
    channel.register_consumer(tag, consumer)
    channel.maybe_start_consumer_work_pool!
    consumer
  end

  def wait_until
    deadline = Bunny::Timestamp.monotonic + 2
    loop do
      break if yield
      raise "condition was not met in time" if Bunny::Timestamp.monotonic > deadline
      sleep 0.01
    end
  end

  # Minimal stand-in for Bunny::Session: only the bits Bunny::Channel touches.
  class FakeBunnyConnection
    def initialize
      @logger = Logger.new(IO::NULL)
    end

    def logger; @logger; end
    def mutex_impl; Monitor; end
    def next_channel_id; 1; end
    def register_channel(_channel); end
    def unregister_channel(_channel); end
    def release_channel_id(_id); end
    def continuation_timeout; 300; end
    def transport_write_timeout; 300; end
    def threaded; true; end
    def frame_max; 131_072; end
    def open?; true; end
    def send_frame(_frame); end
    def close_channel(_channel); end
    def server_has_capability?(_name); false; end
    def record_consumer_with(*); end
    def open_channel(_channel); end
    def synchronised_find_channel(_id); end
  end
end
