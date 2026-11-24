# frozen_string_literal: true
require "spec_helper"

describe Bunny::Session do
  # These examples exercise channel id reuse without a running RabbitMQ node.
  # A local protocol double stands in for the broker: it records the frames
  # Bunny sends and lets the test decide when channel.close-ok and
  # channel.open-ok frames are delivered back to the session.

  let(:logger) { Logger.new(IO::NULL) }

  # Minimal in-process stand-in for the broker side of the connection.
  let(:protocol_double) do
    Class.new do
      attr_reader :sent_frames

      def initialize
        @sent_frames = []
        @mutex       = Mutex.new
      end

      def send_frame(frame)
        @mutex.synchronize { @sent_frames << frame }
      end

      def open?
        true
      end

      def method_classes_sent
        @mutex.synchronize { @sent_frames.map(&:method_class) }
      end
    end.new
  end

  def new_session
    session = Bunny.new(host: "127.0.0.1", port: 65_534,
                        continuation_timeout: 5_000,
                        threaded: false,
                        logger: logger)
    session.instance_variable_set(:@transport, protocol_double)
    session.instance_variable_set(:@channel_id_allocator, Bunny::ChannelIdAllocator.new(8))
    session.instance_variable_set(:@status, :open)
    session
  end

  def pending_operations(session)
    session.instance_variable_get(:@pending_channel_operations)
  end

  def wait_until_pending(session, id)
    Thread.pass until pending_operations(session).pending?(id)
  end

  def open_channel(session, id = nil)
    ch = Bunny::Channel.new(session, id, work_pool: Bunny::ConsumerWorkPool.new(1))
    thread = Thread.new { ch.open }
    wait_until_pending(session, ch.number)
    session.handle_frame(ch.number, AMQ::Protocol::Channel::OpenOk.new(AMQ::Protocol::EMPTY_STRING))
    thread.join
    [ch, thread]
  end

  def begin_close(session, ch)
    thread = Thread.new { ch.close }
    wait_until_pending(session, ch.number)
    thread
  end

  describe "channel id reuse" do
    it "keeps an id allocated until the broker acknowledges the close" do
      session = new_session
      ch, = open_channel(session)

      expect(ch.number).to eq 1
      expect(ch).to be_open
      expect(session.channel_id_allocator.allocated_channel_id?(1)).to be true

      closer = begin_close(session, ch)

      # closing is in flight: the id must not have been returned to the pool yet
      expect(session.channel_id_allocator.allocated_channel_id?(1)).to be true
      expect(session.find_channel(1)).to equal(ch)

      session.handle_frame(1, AMQ::Protocol::Channel::CloseOk.new)
      closer.join

      expect(ch).to be_closed
      expect(session.channel_id_allocator.allocated_channel_id?(1)).to be false
      expect(session.find_channel(1)).to be_nil
    end

    it "hands a closed id out again only after the close-ok was processed" do
      session = new_session
      old_channel, = open_channel(session)

      closer = begin_close(session, old_channel)

      # the close has not been acknowledged yet, a fresh allocation must skip id 1
      interim_id = session.next_channel_id
      expect(interim_id).to eq 2
      session.release_channel_id(interim_id)

      session.handle_frame(1, AMQ::Protocol::Channel::CloseOk.new)
      closer.join

      expect(session.next_channel_id).to eq 1
    end

    it "does not let a delayed close-ok settle the channel.open that reused the id" do
      session = new_session
      old_channel, = open_channel(session)

      # the broker holds back the close-ok for the first channel generation
      closed = Queue.new
      closer = Thread.new do
        old_channel.close
        closed.push(:closed)
      end
      wait_until_pending(session, 1)

      # while that close is still unacknowledged, the id must remain reserved
      expect(session.channel_id_allocator.allocated_channel_id?(1)).to be true

      # deliver the (delayed) close-ok: the old close settles and returns id 1
      session.handle_frame(1, AMQ::Protocol::Channel::CloseOk.new)
      expect(closed.pop).to eq :closed
      expect(session.channel_id_allocator.allocated_channel_id?(1)).to be false

      # a new channel now obtains the same id from the allocator and opens it
      expect(session.next_channel_id).to eq 1
      new_channel = Bunny::Channel.new(session, 1, work_pool: Bunny::ConsumerWorkPool.new(1))
      new_opener = Thread.new { new_channel.open }
      wait_until_pending(session, 1)

      # a second, stale close-ok belonging to the previous generation must not open the new channel
      session.handle_frame(1, AMQ::Protocol::Channel::CloseOk.new)
      sleep 0.05
      expect(new_opener).to be_alive
      expect(new_channel).not_to be_open

      # the matching open-ok completes the new channel
      session.handle_frame(1, AMQ::Protocol::Channel::OpenOk.new(AMQ::Protocol::EMPTY_STRING))
      new_opener.join
      expect(new_channel).to be_open
      expect(session.find_channel(1)).to equal(new_channel)
      expect(session.channel_id_allocator.allocated_channel_id?(1)).to be true
    end

    it "ignores a frame for an id without a pending operation instead of waking a waiter" do
      session = new_session
      expect(logger).to receive(:warn).with(/without a pending channel.close/)
      session.handle_frame(9, AMQ::Protocol::Channel::CloseOk.new)
    end

    it "fails pending operations with a terminal error when the connection shuts down" do
      session = new_session
      ch = Bunny::Channel.new(session, 1, work_pool: Bunny::ConsumerWorkPool.new(1))

      error = nil
      waiter = Thread.new do
        begin
          ch.open
        rescue => e
          error = e
        end
      end
      wait_until_pending(session, 1)

      session.clean_up_on_shutdown
      waiter.join

      expect(error).to be_a(Bunny::InconsistentDataError)
      expect(pending_operations(session)).to be_empty
    end

    it "sends the expected request frames through the transport and returns the protocol response" do
      session = new_session
      ch = Bunny::Channel.new(session, 3, work_pool: Bunny::ConsumerWorkPool.new(1))

      opened = Queue.new
      opener = Thread.new do
        opened.push(session.open_channel(ch))
      end
      wait_until_pending(session, 3)
      session.handle_frame(3, AMQ::Protocol::Channel::OpenOk.new(AMQ::Protocol::EMPTY_STRING))

      expect(opened.pop).to be_a(AMQ::Protocol::Channel::OpenOk)
      expect(protocol_double.method_classes_sent).to include(AMQ::Protocol::Channel::Open)

      closed = Queue.new
      closer = Thread.new { closed.push(session.close_channel(ch)) }
      wait_until_pending(session, 3)
      session.handle_frame(3, AMQ::Protocol::Channel::CloseOk.new)

      expect(closed.pop).to be_a(AMQ::Protocol::Channel::CloseOk)
      expect(protocol_double.method_classes_sent).to include(AMQ::Protocol::Channel::Close)
    end

    it "counts a new channel generation for the same channel number" do
      session = new_session
      expect(session.send(:channel_generation, 5)).to eq 0

      first = session.send(:reserve_channel_number, 5)
      second = session.send(:reserve_channel_number, 5)

      expect(first).to eq 1
      expect(second).to eq 2
      expect(session.send(:current_channel_generation?, 5, first)).to be false
      expect(session.send(:current_channel_generation?, 5, second)).to be true
    end

    it "removes the routing entry on a timed out open but keeps the id reserved" do
      session = new_session
      session.instance_variable_set(:@continuation_timeout, 20)
      id = session.next_channel_id
      ch = Bunny::Channel.new(session, id, work_pool: Bunny::ConsumerWorkPool.new(1))

      expect { session.open_channel(ch) }.to raise_error(Timeout::Error)

      expect(session.find_channel(id)).to be_nil
      expect(pending_operations(session)).to be_empty
      # the broker never confirmed anything, so the id must stay out of the pool
      expect(session.channel_id_allocator.allocated_channel_id?(id)).to be true
    end

    it "removes the routing entry on a timed out close but keeps the id reserved" do
      session = new_session
      session.instance_variable_set(:@continuation_timeout, 20)
      ch, = open_channel(session)

      expect { session.close_channel(ch) }.to raise_error(Timeout::Error)

      expect(session.find_channel(1)).to be_nil
      expect(pending_operations(session)).to be_empty
      # no close-ok was ever received, so the id must not be handed out again
      expect(session.channel_id_allocator.allocated_channel_id?(1)).to be true
    end
  end
end
