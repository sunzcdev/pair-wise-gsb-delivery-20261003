# frozen_string_literal: true

require 'spec_helper'

# A UDP socket fake that routes sent messages into per-endpoint buffers
# based on the host/port it has been connected to.
class RoutingFakeUDPSocket
  def initialize(endpoint_buffers)
    @endpoint_buffers = endpoint_buffers
  end

  def connect(host, port)
    @endpoint = [host, port]
  end

  def send(message, *_args)
    @endpoint_buffers[@endpoint] << message.dup
    true
  end

  def close
    # no-op: packets are retained in the shared endpoint buffers
  end
end

RSpec.shared_examples 'Connection switch integration testing' do |single_thread|
  subject do
    Datadog::Statsd.new('localhost', old_port,
      telemetry_enable: false,
      single_thread: single_thread,
      buffer_max_pool_size: Float::INFINITY,
      buffer_max_payload_size: old_max_payload_size,
      origin_detection: false,
    )
  end

  let(:old_port) { 1234 }
  let(:new_port) { 5678 }

  let(:old_max_payload_size) { 100 }
  let(:new_max_payload_size) { 40 }

  let(:old_endpoint) { ['localhost', old_port] }
  let(:new_endpoint) { ['localhost', new_port] }

  let(:endpoint_buffers) { Hash.new { |hash, key| hash[key] = [] } }
  let(:old_packets) { endpoint_buffers[old_endpoint] }
  let(:new_packets) { endpoint_buffers[new_endpoint] }

  before do
    fake = RoutingFakeUDPSocket.new(endpoint_buffers)
    allow(UDPSocket).to receive(:new).and_return(fake)
  end

  after do
    subject.close
  end

  describe 'switching to a smaller UDP endpoint' do
    it 'keeps already sent packets on the old endpoint and re-groups queued lines for the new one' do
      # each serialized line is 20 bytes (16 char name + ":1|c"); two lines
      # make a 41 byte packet that fits the old 100 byte limit but not the new 40 one
      metric_name = 'c' * 16
      serialized_line = "#{metric_name}:1|c"
      expect(serialized_line.bytesize).to eq(20)

      # Two lines are packed together: the 41 byte packet is flushed through
      # the old endpoint (limit 100).
      subject.increment(metric_name)
      subject.increment(metric_name)
      subject.flush(sync: true)

      expect(old_packets).to eq(["#{serialized_line}\n#{serialized_line}"])

      # Three more lines are buffered but not sent yet when the switch happens.
      3.times { subject.increment(metric_name) }

      subject.switch_connection('localhost', new_port,
        buffer_max_payload_size: new_max_payload_size)

      subject.flush(sync: true)

      # the old endpoint receives nothing else: no double send
      expect(old_packets.size).to eq(1)

      # the new endpoint (limit 40) only receives single-line compliant packets
      expect(new_packets.size).to eq(3)
      expect(new_packets).to all(eq(serialized_line))
      expect(new_packets).to all(satisfy { |payload| payload.bytesize <= new_max_payload_size })

      # the client now reports the new endpoint
      expect(subject.port).to eq(new_port)
    end

    it 'drops a single line that exceeds the new limit with an explicit reason' do
      log = StringIO.new
      logger = Logger.new(log)
      logger.level = Logger::ERROR

      statsd = Datadog::Statsd.new('localhost', old_port,
        telemetry_enable: false,
        single_thread: single_thread,
        buffer_max_pool_size: Float::INFINITY,
        buffer_max_payload_size: old_max_payload_size,
        origin_detection: false,
        logger: logger,
      )

      long_name = 'x' * 60
      short_name = 'ok'

      statsd.increment(long_name)
      statsd.increment(short_name)

      statsd.switch_connection('localhost', new_port,
        buffer_max_payload_size: new_max_payload_size)

      statsd.flush(sync: true)

      # the oversized line is explicitly reported as dropped
      expect(log.string).to match(/dropping message of \d+ bytes/)
      expect(log.string).to match(/exceeds the max payload size of #{new_max_payload_size} bytes/)

      # the compliant line still goes through as a whole-line packet
      expect(endpoint_buffers[new_endpoint]).to eq(['ok:1|c'])
      expect(endpoint_buffers[new_endpoint])
        .to all(satisfy { |payload| payload.bytesize <= new_max_payload_size })
    ensure
      statsd&.close
    end

    it 're-groups lines that fit together in the old packet but not in the new smaller one' do
      # each serialized line is 14 bytes ("mN.counter:1|c"); two lines would
      # make a 29 byte packet which exceeds the new 20 byte limit
      names = %w[m1.counter m2.counter m3.counter]
      names.each { |name| subject.increment(name) }

      subject.switch_connection('localhost', new_port,
        buffer_max_payload_size: 20)

      subject.flush(sync: true)

      expect(new_packets.size).to eq(3)
      expect(new_packets).to contain_exactly(
        'm1.counter:1|c',
        'm2.counter:1|c',
        'm3.counter:1|c',
      )
      expect(new_packets).to all(satisfy { |payload| payload.bytesize <= 20 })
      expect(old_packets).to be_empty
    end

    context 'with delay_serialization enabled' do
      subject do
        Datadog::Statsd.new('localhost', old_port,
          telemetry_enable: false,
          single_thread: single_thread,
          delay_serialization: true,
          sender_queue_size: 10,
          buffer_max_pool_size: Float::INFINITY,
          buffer_max_payload_size: old_max_payload_size,
          origin_detection: false,
        )
      end

      it 'serializes queued arrays and re-groups them on the new endpoint' do
        subject.increment('d1.counter')
        subject.increment('d2.counter')

        subject.switch_connection('localhost', new_port,
          buffer_max_payload_size: 20)

        subject.flush(sync: true)

        expect(new_packets).to contain_exactly(
          'd1.counter:1|c',
          'd2.counter:1|c',
        )
        expect(new_packets).to all(satisfy { |payload| payload.bytesize <= 20 })
      end
    end

    context 'with telemetry enabled' do
      subject do
        Datadog::Statsd.new('localhost', old_port,
          telemetry_enable: true,
          telemetry_flush_interval: 60,
          single_thread: single_thread,
          buffer_max_pool_size: Float::INFINITY,
          buffer_max_payload_size: 8_192,
          origin_detection: false,
        )
      end

      it 'keeps emitting telemetry through the new endpoint after the switch' do
        subject.increment('t.counter')

        subject.switch_connection('localhost', new_port,
          buffer_max_payload_size: 4_096)

        subject.flush(flush_telemetry: true, sync: true)

        metric_packets = new_packets.select { |packet| packet.include?('t.counter:1|c') }
        telemetry_packets = new_packets.select { |packet| packet.include?('datadog.dogstatsd.client.') }

        expect(metric_packets).not_to be_empty
        expect(telemetry_packets).not_to be_empty
        expect(new_packets).to all(satisfy { |payload| payload.bytesize <= 4_096 })
      end
    end
  end
end

describe 'Connection switch - single threaded mode' do
  it_behaves_like 'Connection switch integration testing', "true"
end

describe 'Connection switch - multi threaded mode' do
  it_behaves_like 'Connection switch integration testing', "false"
end
