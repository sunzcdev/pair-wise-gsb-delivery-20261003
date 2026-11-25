# frozen_string_literal: true

require 'spec_helper'

RSpec.shared_examples 'tag reload generation attribution' do |single_thread|
  let(:receiver) do
    socket = UDPSocket.new
    socket.bind('127.0.0.1', 0)
    socket
  end

  let(:receiver_port) { receiver.addr[1] }
  let(:entered_first_write) { Queue.new }
  let(:release_first_write) { Queue.new }
  let(:first_payloads) { Queue.new }
  let(:write_attempts) { Hash.new(0) }

  let(:client) do
    Datadog::Statsd.new(
      '127.0.0.1',
      receiver_port,
      tags: ['deploy:old'],
      namespace: 'reload_test',
      origin_detection: false,
      single_thread: single_thread,
      telemetry_flush_interval: 60,
      buffer_max_pool_size: Float::INFINITY
    )
  end

  before do
    receiver
    client

    allow(UDPSocket).to receive(:new).and_wrap_original do |original_new, *args|
      socket = original_new.call(*args)
      native_send = socket.method(:send)

      allow(socket).to receive(:send) do |payload, flags = 0|
        if payload.include?('first.commit')
          write_attempts['first'] += 1

          if write_attempts['first'] == 1
            first_payloads << payload.dup
            entered_first_write << true
            release_first_write.pop
          end

          raise Errno::ECONNREFUSED, 'injected first packet write failure'
        end

        native_send.call(payload, flags)
      end

      socket
    end
  end

  after do
    release_first_write << :release if release_first_write.num_waiting > 0
    client.close
    receiver.close
  end

  it 'keeps the blocked packet and writer failure telemetry on the old generation' do
    old_telemetry = client.telemetry

    client.increment('first.commit')

    flush_thread = nil
    if single_thread
      flush_thread = Thread.new { client.flush(sync: true) }
    else
      client.flush
    end

    Timeout.timeout(5) { entered_first_write.pop }
    first_payload = Timeout.timeout(1) { first_payloads.pop }
    expect(first_payload).to include('deploy:old')

    client.reload_tags(tags: ['deploy:new'])

    client.increment('second.commit')
    client.flush(sync: true)

    ready = IO.select([receiver], nil, nil, 5)
    expect(ready).not_to be_nil
    second_packet = receiver.recv(65_536)

    expect(second_packet).to include('second.commit')
    expect(second_packet).to include('deploy:new')
    expect(second_packet).not_to include('deploy:old')

    release_first_write << :go

    Timeout.timeout(5) do
      sleep 0.01 until old_telemetry.packets_dropped_writer == 1
    end

    flush_thread.join(5) if flush_thread
    expect(flush_thread).not_to be_alive if flush_thread

    expect(old_telemetry.metrics).to eq(1)
    expect(old_telemetry.packets_dropped_writer).to eq(1)
    expect(old_telemetry.bytes_dropped_writer).to be > 0
    expect(old_telemetry.flush.join).to include('deploy:old')
    expect(old_telemetry.flush.join).not_to include('deploy:new')

    expect(client.telemetry).not_to eq(old_telemetry)
    expect(client.previous_telemetry).to eq(old_telemetry)
    expect(client.telemetry.metrics).to eq(1)
    expect(client.telemetry.packets_dropped_writer).to eq(0)
    expect(client.telemetry.packets_sent).to eq(1)
    expect(client.telemetry.flush.join).to include('deploy:new')
    expect(client.telemetry.flush.join).not_to include('deploy:old')
  end
end

RSpec.describe 'Datadog::Statsd tag reload generations, background sender' do
  include_examples 'tag reload generation attribution', false
end

RSpec.describe 'Datadog::Statsd tag reload generations, single-thread sender' do
  include_examples 'tag reload generation attribution', true
end

RSpec.describe Datadog::Statsd::Serialization::TagSerializer do
  it 'can replace its global tags without mutating the replacement input' do
    serializer = described_class.new(['old:one'], {})
    replacement = ['new:two']

    serializer.global_tags = replacement
    replacement << 'later:true'

    expect(serializer.format(nil)).to eq('new:two')
  end
end

RSpec.describe Datadog::Statsd, 'failed tag reload' do
  it 'keeps serving the previous generation when the replacement tags are invalid' do
    client = described_class.new(
      telemetry_enable: false,
      origin_detection: false,
      tags: ['deploy:old']
    )
    old_generation = client.generation

    expect { client.reload_tags(123) }.to raise_error(ArgumentError, /tags must be/)
    expect(client.generation).to equal(old_generation)
    expect(client.tags).to eq(['deploy:old'])

    client.close(flush: false)
  end
end
