require 'spec_helper'

describe Datadog::Statsd::Forwarder do
  subject do
    described_class.new(**params)
  end

  let(:buffer_max_payload_size) do
    1_024
  end

  let(:buffer_max_pool_size) do
    50
  end

  let(:buffer_overflowing_stategy) do
    :anything
  end

  let(:buffer_flush_interval) do
    15
  end

  let(:telemetry_flush_interval) do
    42
  end

  let(:global_tags) do
    instance_double(Array)
  end

  let(:logger) do
    instance_double(Logger)
  end

  let(:serializer) do
    Datadog::Statsd::Serialization::Serializer.new
  end

  before do
    allow(Datadog::Statsd::MessageBuffer)
      .to receive(:new)
      .and_return(message_buffer)

    allow(Datadog::Statsd::Telemetry)
      .to receive(:new)
      .and_return(telemetry)

    allow(Datadog::Statsd::Sender)
      .to receive(:new)
      .and_return(sender)
  end

  let(:message_buffer) do
    instance_double(Datadog::Statsd::MessageBuffer)
  end

  let(:telemetry) do
    instance_double(Datadog::Statsd::Telemetry, would_fit_in?: true)
  end

  let(:sender) do
    instance_double(Datadog::Statsd::Sender, start: true)
  end

  context 'when using a host and a port' do
    before do
      allow(Datadog::Statsd::UDPConnection)
        .to receive(:new)
        .and_return(udp_connection)
    end

    let(:udp_connection) do
      instance_double(Datadog::Statsd::UDPConnection,
        host: host,
        port: port,
        logger: logger,
        close: true
      )
    end

    let(:host) do
      '127.0.0.1'
    end

    let(:port) do
      1234
    end

    let(:params) do
      {
        connection_cfg: Datadog::Statsd::ConnectionCfg.new(host: host, port: port),

        buffer_max_payload_size: buffer_max_payload_size,
        buffer_max_pool_size: buffer_max_pool_size,
        buffer_overflowing_stategy: buffer_overflowing_stategy,
        buffer_flush_interval: buffer_flush_interval,

        telemetry_flush_interval: telemetry_flush_interval,

        logger: logger,
        global_tags: global_tags,

        serializer: serializer,
      }
    end

    describe '#initialize' do
      it 'builds an UDP connection' do
        expect(Datadog::Statsd::UDPConnection)
          .to receive(:new)
          .with('127.0.0.1', 1234, logger: logger, telemetry: telemetry)

        subject
      end

      it 'builds the sender' do
        expect(Datadog::Statsd::Sender)
          .to receive(:new)
          .with(message_buffer,
                logger: logger,
                flush_interval: buffer_flush_interval,
                queue_size: Datadog::Statsd::UDP_DEFAULT_SENDER_QUEUE_SIZE,
                telemetry: telemetry)
          .exactly(1)

        subject
      end

      it 'starts the sender' do
        expect(sender)
          .to receive(:start)
          .exactly(1)

        subject
      end

      context 'when the telemetry is disabled' do
        let(:telemetry_flush_interval) do
          nil
        end

        it 'does not build a telemetry object' do
          expect(Datadog::Statsd::Telemetry)
            .not_to receive(:new)

          subject
        end

        it 'builds an UDP connection without telemetry' do
          expect(Datadog::Statsd::UDPConnection)
            .to receive(:new)
            .with('127.0.0.1', 1234, logger: logger, telemetry: nil)

          subject
        end
      end

      context 'when the buffer_max_payload_size is negative' do
        let(:buffer_max_payload_size) do
          -1
        end

        it 'raises an ArgumentError' do
          expect do
            subject
          end.to raise_error(ArgumentError, /buffer_max_payload_size cannot be <= 0/)
        end
      end

      context 'when the telemetry would not fit in provided size' do
        before do
          allow(telemetry)
            .to receive(:would_fit_in?)
            .with(1_024)
            .and_return(false)
        end

        it 'raises an ArgumentError' do
          expect do
            subject
          end.to raise_error(ArgumentError, /buffer_max_payload_size is not high enough to use telemetry/)
        end
      end

      it 'builds the message buffer with provided buffer_max_payload_size' do
        expect(Datadog::Statsd::MessageBuffer)
          .to receive(:new)
          .with(anything, hash_including(max_payload_size: 1_024))

        subject
      end

      context 'when no buffer_max_payload_size is not provided' do
        let(:buffer_max_payload_size) do
          nil
        end

        it 'builds the message buffer with max_payload_size=UDP_DEFAULT_BUFFER_SIZE' do
          expect(Datadog::Statsd::MessageBuffer)
            .to receive(:new)
            .with(anything, hash_including(max_payload_size: Datadog::Statsd::UDP_DEFAULT_BUFFER_SIZE))

          subject
        end
      end

      it 'builds the message buffer with provided buffer_max_pool_size' do
        expect(Datadog::Statsd::MessageBuffer)
          .to receive(:new)
          .with(anything, hash_including(max_pool_size: 50))

        subject
      end

      context 'when no buffer_max_pool_size is not provided' do
        let(:buffer_max_pool_size) do
          nil
        end

        it 'builds the message buffer with max_pool_size=DEFAULT_BUFFER_POOL_SIZE' do
          expect(Datadog::Statsd::MessageBuffer)
            .to receive(:new)
            .with(anything, hash_including(max_pool_size: Datadog::Statsd::DEFAULT_BUFFER_POOL_SIZE))

          subject
        end
      end
    end

    its(:transport_type) { is_expected.to eq :udp }
    its(:host) { is_expected.to eq '127.0.0.1' }
    its(:port) { is_expected.to eq 1234 }
    its(:socket_path) { is_expected.to be_nil }

    describe '#switch_connection' do
      let(:new_udp_connection) do
        instance_double(Datadog::Statsd::UDPConnection,
          close: true,
          logger: logger)
      end

      # only stub the connection factory for the connection created during the
      # switch, so that the forwarder keeps using udp_connection as its initial one
      before do
        allow_any_instance_of(Datadog::Statsd::ConnectionCfg)
          .to receive(:make_connection)
          .and_call_original
        allow(Datadog::Statsd::UDPConnection)
          .to receive(:new)
          .and_return(udp_connection, new_udp_connection)
      end

      it 'asks the sender to switch to a new connection with the new payload limit' do
        expect(sender)
          .to receive(:switch_connection)
          .with(new_udp_connection, max_payload_size: 512)

        subject.switch_connection('10.0.0.2', 9999, max_payload_size: 512)
      end

      it 'closes the old connection after the switch' do
        allow(sender).to receive(:switch_connection)
        allow(udp_connection).to receive(:close)

        expect(udp_connection).to receive(:close)

        subject.switch_connection('10.0.0.2', 9999, max_payload_size: 512)
      end

      it 'exposes the new endpoint through host/port' do
        allow(sender).to receive(:switch_connection)
        allow(new_udp_connection).to receive(:host).and_return('10.0.0.2')
        allow(new_udp_connection).to receive(:port).and_return(9999)

        subject.switch_connection('10.0.0.2', 9999, max_payload_size: 512)

        expect(subject.host).to eq '10.0.0.2'
        expect(subject.port).to eq 9999
      end

      it 'forwards a nil payload limit when none is provided' do
        expect(sender)
          .to receive(:switch_connection)
          .with(new_udp_connection, max_payload_size: nil)

        subject.switch_connection('10.0.0.2', 9999)
      end

      context 'with a non-positive payload size' do
        it 'raises an ArgumentError' do
          expect do
            subject.switch_connection('10.0.0.2', 9999, max_payload_size: 0)
          end.to raise_error(ArgumentError, /buffer_max_payload_size cannot be <= 0/)
        end
      end

      context 'when telemetry is enabled but does not fit' do
        let(:telemetry) do
          instance_double(Datadog::Statsd::Telemetry,
            would_fit_in?: false)
        end

        it 'raises an ArgumentError before touching the sender' do
          expect(sender).not_to receive(:switch_connection)

          expect do
            subject.switch_connection('10.0.0.2', 9999, max_payload_size: 10)
          end.to raise_error(ArgumentError, /buffer_max_payload_size is not high enough to use telemetry/)
        end
      end
    end

    describe '#close' do
      before do
        allow(sender).to receive(:stop)
        allow(udp_connection).to receive(:close)
      end

      it 'stops the sender' do
        expect(sender)
          .to receive(:stop)

        subject.close
      end

      it 'forwards the close message to the connection' do
        expect(udp_connection)
          .to receive(:close)

        subject.close
      end
    end
  end

  context 'when using a socket_path' do
    before do
      allow(Datadog::Statsd::UDSConnection)
        .to receive(:new)
        .and_return(uds_connection)
    end

    let(:uds_connection) do
      instance_double(Datadog::Statsd::UDSConnection,
        socket_path: socket_path,
      )
    end

    let(:socket_path) do
      '/tmp/dd_socket'
    end

    let(:params) do
      {
        connection_cfg: Datadog::Statsd::ConnectionCfg.new(socket_path: socket_path),

        buffer_max_payload_size: buffer_max_payload_size,
        buffer_max_pool_size: buffer_max_pool_size,
        buffer_overflowing_stategy: buffer_overflowing_stategy,
        buffer_flush_interval: buffer_flush_interval,

        telemetry_flush_interval: telemetry_flush_interval,

        logger: logger,
        global_tags: global_tags,

        serializer: serializer,
      }
    end

    describe '#initialize' do
      it 'builds an UDS connection' do
        expect(Datadog::Statsd::UDSConnection)
          .to receive(:new)
          .with('/tmp/dd_socket', logger: logger, telemetry: telemetry)

        subject
      end

      it 'builds the sender' do
        expect(Datadog::Statsd::Sender)
          .to receive(:new)
          .with(message_buffer,
                logger: logger,
                flush_interval: buffer_flush_interval,
                queue_size: Datadog::Statsd::UDS_DEFAULT_SENDER_QUEUE_SIZE,
                telemetry: telemetry)
          .exactly(1)

        subject
      end

      it 'starts the sender' do
        expect(sender)
          .to receive(:start)
          .exactly(1)

        subject
      end

      context 'when the telemetry is disabled' do
        let(:telemetry_flush_interval) do
          nil
        end

        it 'does not build a telemetry object' do
          expect(Datadog::Statsd::Telemetry)
            .not_to receive(:new)

          subject
        end

        it 'builds an UDP connection without telemetry' do
          expect(Datadog::Statsd::UDSConnection)
            .to receive(:new)
            .with('/tmp/dd_socket', logger: logger, telemetry: nil)

          subject
        end
      end

      context 'when the buffer_max_payload_size is negative' do
        let(:buffer_max_payload_size) do
          -1
        end

        it 'raises an ArgumentError' do
          expect do
            subject
          end.to raise_error(ArgumentError, /buffer_max_payload_size cannot be <= 0/)
        end
      end

      context 'when the telemetry would not fit in provided size' do
        before do
          allow(telemetry)
            .to receive(:would_fit_in?)
            .with(1_024)
            .and_return(false)
        end

        it 'raises an ArgumentError' do
          expect do
            subject
          end.to raise_error(ArgumentError, /buffer_max_payload_size is not high enough to use telemetry/)
        end
      end

      it 'builds the message buffer with provided buffer_max_payload_size' do
        expect(Datadog::Statsd::MessageBuffer)
          .to receive(:new)
          .with(anything, hash_including(max_payload_size: 1_024))

        subject
      end

      context 'when no buffer_max_payload_size is not provided' do
        let(:buffer_max_payload_size) do
          nil
        end

        it 'builds the message buffer with max_payload_size=UDS_DEFAULT_BUFFER_SIZE' do
          expect(Datadog::Statsd::MessageBuffer)
            .to receive(:new)
            .with(anything, hash_including(max_payload_size: Datadog::Statsd::UDS_DEFAULT_BUFFER_SIZE))

          subject
        end
      end

      it 'builds the message buffer with provided buffer_max_pool_size' do
        expect(Datadog::Statsd::MessageBuffer)
          .to receive(:new)
          .with(anything, hash_including(max_pool_size: 50))

        subject
      end

      context 'when no buffer_max_pool_size is not provided' do
        let(:buffer_max_pool_size) do
          nil
        end

        it 'builds the message buffer with max_pool_size=DEFAULT_BUFFER_POOL_SIZE' do
          expect(Datadog::Statsd::MessageBuffer)
            .to receive(:new)
            .with(anything, hash_including(max_pool_size: Datadog::Statsd::DEFAULT_BUFFER_POOL_SIZE))

          subject
        end
      end
    end

    its(:transport_type) { is_expected.to eq :uds }
    its(:host) { is_expected.to eq nil }
    its(:port) { is_expected.to eq nil }
    its(:socket_path) { is_expected.to eq '/tmp/dd_socket'  }

    describe '#close' do
      before do
        allow(sender).to receive(:stop)
        allow(uds_connection).to receive(:close)
      end

      it 'stops the sender' do
        expect(sender)
          .to receive(:stop)

        subject.close
      end

      it 'forwards the close message to the connection' do
        expect(uds_connection)
          .to receive(:close)

        subject.close
      end
    end

    describe '#switch_connection' do
      it 'raises because switching endpoints is only supported over UDP' do
        expect do
          subject.switch_connection('10.0.0.2', 9999, max_payload_size: 512)
        end.to raise_error(Datadog::Statsd::Error, /only supported for UDP/)
      end
    end
  end

  describe '#send_message' do
    before do
      allow(Datadog::Statsd::UDPConnection)
        .to receive(:new)
        .and_return(udp_connection)
    end

    let(:udp_connection) do
      instance_double(Datadog::Statsd::UDPConnection,
        host: host,
        port: port,
        logger: logger,
        close: true
      )
    end

    let(:host) do
      '127.0.0.1'
    end

    let(:port) do
      1234
    end

    let(:params) do
      {
        connection_cfg: Datadog::Statsd::ConnectionCfg.new(host: host, port: port),

        buffer_max_payload_size: buffer_max_payload_size,
        buffer_max_pool_size: buffer_max_pool_size,
        buffer_overflowing_stategy: buffer_overflowing_stategy,

        telemetry_flush_interval: telemetry_flush_interval,

        logger: logger,
        global_tags: global_tags,

        serializer: serializer,
      }
    end

    context 'when we should flush the telemetry' do
      before do
        allow(sender).to receive(:add)
      end

      let(:telemetry) do
        instance_double(Datadog::Statsd::Telemetry,
          would_fit_in?: true,
          should_flush?: true,
          flush: ['telemetry1', 'telemetry2'],
          reset: true
        )
      end

      it 'adds the message to the sender queue' do
        expect(sender)
          .to receive(:add)
          .with('toto')

        subject.send_message('toto')
      end

      it 'adds the telemetry message' do
        expect(sender)
          .to receive(:add)
          .with('toto')

        expect(sender)
          .to receive(:add)
          .with('telemetry1')

        expect(sender)
          .to receive(:add)
          .with('telemetry2')

        subject.send_message('toto')
      end
    end

    context 'when we do not have to flush the telemetry' do
      let(:telemetry) do
        instance_double(Datadog::Statsd::Telemetry, would_fit_in?: true, should_flush?: false)
      end

      it 'adds the message to the buffer' do
        expect(sender)
          .to receive(:add)
          .with('toto')

        subject.send_message('toto')
      end
    end
  end
end
