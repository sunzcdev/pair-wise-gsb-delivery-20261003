# frozen_string_literal: true
require "spec_helper"

describe Bunny::Concurrent::PendingChannelOperationRegistry do
  subject(:registry) { described_class.new }

  let(:open_ok)  { AMQ::Protocol::Channel::OpenOk.new("") }
  let(:close_ok) { AMQ::Protocol::Channel::CloseOk.new }

  it "delivers a response to the operation waiting for that channel id" do
    result = nil
    started = Queue.new
    thread = Thread.new do
      result = registry.await(1, AMQ::Protocol::Channel::OpenOk, 1000) { started.push(true) }
    end
    started.pop

    expect(registry.deliver(1, open_ok)).to be true
    thread.join

    expect(result).to be_a(AMQ::Protocol::Channel::OpenOk)
    expect(registry).to be_empty
  end

  it "ignores frames for ids without a pending operation" do
    expect(registry.deliver(7, close_ok)).to be false
    expect(registry).to be_empty
  end

  it "keeps separate waiters per channel id so one response cannot settle another" do
    first  = Queue.new
    second = Queue.new

    t1 = Thread.new { first << registry.await(1, AMQ::Protocol::Channel::OpenOk, 1000) { } }
    Thread.pass until registry.pending?(1)
    t2 = Thread.new { second << registry.await(2, AMQ::Protocol::Channel::CloseOk, 1000) { } }
    Thread.pass until registry.pending?(2)

    registry.deliver(2, close_ok)
    registry.deliver(1, open_ok)

    expect(first.pop).to be_a(AMQ::Protocol::Channel::OpenOk)
    expect(second.pop).to be_a(AMQ::Protocol::Channel::CloseOk)
    expect(registry).to be_empty
  end

  it "routes a frame to the waiter expecting its method class across reused ids" do
    # models a delayed channel.close racing with a channel.open that reuses the id
    old_close_result = Queue.new
    new_open_result  = Queue.new

    closer = Thread.new { old_close_result << registry.await(1, AMQ::Protocol::Channel::CloseOk, 1000) { } }
    Thread.pass until registry.pending?(1)
    opener = Thread.new { new_open_result << registry.await(1, AMQ::Protocol::Channel::OpenOk, 1000) { } }
    Thread.pass until registry.size == 2

    # the late close-ok settles only the close, leaving the open untouched
    expect(registry.deliver(1, close_ok)).to be true
    expect(old_close_result.pop).to be_a(AMQ::Protocol::Channel::CloseOk)
    expect(registry.pending?(1)).to be true

    # an unrelated close-ok cannot open the new channel generation
    expect(registry.deliver(1, close_ok)).to be false
    expect(new_open_result).to be_empty

    expect(registry.deliver(1, open_ok)).to be true
    expect(new_open_result.pop).to be_a(AMQ::Protocol::Channel::OpenOk)
    expect(registry).to be_empty
  end

  it "unblocks every waiter when fail_all is used" do
    result = Queue.new
    started = Queue.new
    thread = Thread.new do
      result << registry.await(1, AMQ::Protocol::Channel::OpenOk, 1000) { started.push(true) }
    end
    started.pop

    failure = Bunny::ConnectionClosedError.new(nil)
    registry.fail_all(failure)

    expect(result.pop).to equal(failure)
    expect(registry).to be_empty
  end

  it "times out a waiter without leaving a stale registration behind" do
    expect do
      Timeout.timeout(2) do
        registry.await(1, AMQ::Protocol::Channel::OpenOk, 10) { }
      end
    end.to raise_error(Timeout::Error)

    expect(registry).not_to be_pending(1)
  end

  it "serves many concurrent operations on distinct ids without losing a response" do
    thread_count = 32
    results      = Array.new(thread_count)
    start_barrier = Queue.new

    threads = Array.new(thread_count) do |i|
      Thread.new do
        start_barrier.pop
        method = registry.await(i + 1, AMQ::Protocol::Channel::OpenOk, 5_000) { }
        results[i] = method
      end
    end

    thread_count.times { start_barrier.push(:go) }
    Thread.pass until registry.size == thread_count

    # every waiter receives exactly its own response
    thread_count.times { |i| registry.deliver(i + 1, open_ok) }
    threads.each(&:join)

    expect(results).to all(be_a(AMQ::Protocol::Channel::OpenOk))
    expect(registry).to be_empty
  end
end

