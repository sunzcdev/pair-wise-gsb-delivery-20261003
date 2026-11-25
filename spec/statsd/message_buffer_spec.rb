require 'spec_helper'

describe Datadog::Statsd::MessageBuffer do
  subject do
    described_class.new(connection,
      max_payload_size: max_payload_size,
      max_pool_size: max_pool_size,
      overflowing_stategy: overflowing_stategy,
      serializer: serializer,
    )
  end

  let(:connection) do
    instance_double(Datadog::Statsd::UDPConnection)
  end

  let(:max_payload_size) do
    100
  end

  let(:max_pool_size) do
    3
  end

  let(:overflowing_stategy) do
    :drop
  end

  let(:serializer) do
    Datadog::Statsd::Serialization::Serializer.new
  end

  describe '#add' do
    context 'when the message is empty' do
      it 'returns nil' do
        expect(subject.add('')).to be nil
      end

      it 'never flushes (never adds only \n)' do
        expect(connection).not_to receive(:write)

        1000.times { subject.add('') }
      end
    end

    context 'when the buffer is empty' do
      context 'when the message is lesser than the max size - send tolerance' do
        it 'does not flush' do
          expect(connection).not_to receive(:write)

          subject.add('a' * 95)
        end
      end

      context 'when the message is equal to the max size (- tolerance)' do
        it 'flushes the message' do
          expect(connection)
            .to receive(:write)
            .with('a' * 96)

          subject.add('a' * 96)
        end
      end

      context 'when the message is equal to the max size (- tolerance)' do
        it 'flushes the message' do
          expect(connection)
            .to receive(:write)
            .with('a' * 100)

          subject.add('a' * 100)
        end
      end

      context 'when the message is bigger than the max size' do
        context 'in buffer overflow mode :drop' do
          it 'does not flush' do
            expect(connection).not_to receive(:write)

            subject.add('a' * 101)
          end
        end

        context 'in buffer overflow mode :raise' do
          let(:overflowing_stategy) do
            :raise
          end

          it 'raises a specific error' do
            expect do
              subject.add('a' * 101)
            end.to raise_error(Datadog::Statsd::Error, 'Message too big for payload limit')
          end
        end
      end
    end

    context 'when the buffer already has messages' do
      before do
        subject.add('aaaaaa')
      end

      context 'when reaching the maximum pool size' do
        before do
          subject.add('a')
        end

        it 'flushes the messages' do
          expect(connection)
            .to receive(:write)
            .with("aaaaaa\na\na")

          subject.add('a')
        end
      end

      context 'when we get the max size' do
        it 'flushes the previous messages as there is not enough space to bufferize new message' do
          expect(connection)
            .to receive(:write)
            .with("aaaaaa")

          subject.add('a' * 94)
        end
      end

      context 'when we overflow the max size' do
        context 'in buffer overflow mode :drop' do
          it 'does not flush' do
            expect(connection).not_to receive(:write)

            subject.add('a' * 105)
          end
        end

        context 'in buffer overflow mode :raise' do
          let(:overflowing_stategy) do
            :raise
          end

          it 'raises a specific error' do
            expect do
              subject.add('a' * 105)
            end.to raise_error(Datadog::Statsd::Error, 'Message too big for payload limit')
          end
        end
      end

      context "when the don't overflow the max size" do
        it 'does not flush' do
          expect(connection).not_to receive(:write)

          subject.add('a' * 20)
        end
      end
    end
  end

  describe '#switch_connection' do
    let(:new_connection) do
      instance_double(Datadog::Statsd::UDPConnection,
        reset_telemetry: true,
        logger: nil,
        write: true)
    end

    context 'when buffered lines fit in the new (smaller) limit' do
      before do
        # two lines buffered together on the old connection, no packet sent yet
        subject.add('a' * 10)
        subject.add('b' * 10)
      end

      it 're-groups the buffered lines into compliant packets on the new connection' do
        # combined packet is 21 bytes; the new limit is 20 so it must be split
        expect(new_connection).to receive(:write).with('a' * 10)
        expect(new_connection).to receive(:write).with('b' * 10)

        subject.switch_connection(new_connection, max_payload_size: 20)
        subject.flush
      end

      it 'does not write anything to the old connection during the switch' do
        expect(connection).not_to receive(:write)

        subject.switch_connection(new_connection, max_payload_size: 20)
      end

      it 'keeps applying the new payload limit to messages added after the switch' do
        subject.switch_connection(new_connection, max_payload_size: 20)

        expect(new_connection).to receive(:write).with('b' * 10)
        expect(new_connection).to receive(:write).with('c' * 10)

        subject.add('c' * 10)
        subject.flush
      end
    end

    context 'when buffered lines together exceed the new limit but individually fit' do
      before do
        subject.add('a' * 12)
        subject.add('b' * 12)
      end

      it 'splits the previously combined packet and flushes each line separately' do
        expect(new_connection).to receive(:write).with('a' * 12)
        expect(new_connection).to receive(:write).with('b' * 12)

        subject.switch_connection(new_connection, max_payload_size: 24)
        subject.flush
      end

      it 'only emits packets smaller than or equal to the new limit' do
        written_payloads = []
        allow(new_connection).to receive(:write) { |payload| written_payloads << payload }

        subject.switch_connection(new_connection, max_payload_size: 24)
        subject.flush

        expect(written_payloads).to all(satisfy { |payload| payload.bytesize <= 24 })
      end
    end

    context 'when a single buffered line exceeds the new limit' do
      before do
        subject.add('a' * 60)
        subject.add('b' * 10)
      end

      context 'in buffer overflow mode :drop' do
        it 'drops the oversized line with an explicit reason and keeps the other line' do
          drop_message = nil
          logger = instance_double(Logger)
          allow(logger).to receive(:error) { |&block| drop_message = block.call }
          allow(new_connection).to receive(:logger).and_return(logger)

          expect(new_connection).to receive(:write).with('b' * 10)

          subject.switch_connection(new_connection, max_payload_size: 50)
          subject.flush

          expect(drop_message).to match(/dropping message of 60 bytes/)
          expect(drop_message).to match(/max payload size of 50 bytes/)
        end

        it 'does not raise' do
          expect do
            subject.switch_connection(new_connection, max_payload_size: 50)
          end.not_to raise_error
        end
      end

      context 'in buffer overflow mode :raise' do
        let(:overflowing_stategy) do
          :raise
        end

        it 'raises a specific error' do
          expect do
            subject.switch_connection(new_connection, max_payload_size: 50)
          end.to raise_error(Datadog::Statsd::Error, 'Message too big for payload limit')
        end
      end
    end

    context 'when some packets were already flushed before the switch' do
      it 'keeps already sent packets on the old connection and never sends them twice' do
        # fill the buffer so the first packet gets flushed through the old connection
        expect(connection).to receive(:write).with('a' * 90).once

        subject.add('a' * 90) # preemptive flush threshold is 95 bytes, so this stays buffered
        subject.add('z' * 25) # 90 + 1 + 25 >= 100: triggers a flush of the first packet

        # only the second line is still buffered when switching
        expect(new_connection).to receive(:write).with('z' * 25).once
        expect(connection).not_to receive(:write)

        subject.switch_connection(new_connection, max_payload_size: 50)
        subject.flush
      end
    end

    context 'when called without a new payload size' do
      it 'switches the connection but keeps the current payload limit' do
        subject.add('a' * 10)
        subject.add('b' * 10)

        subject.switch_connection(new_connection)

        expect(new_connection).to receive(:write).with(('a' * 10) + "\n" + ('b' * 10))
        subject.flush
      end
    end

    context 'with an invalid payload size' do
      it 'raises an ArgumentError on zero' do
        expect do
          subject.switch_connection(new_connection, max_payload_size: 0)
        end.to raise_error(ArgumentError, /max_payload_size cannot be <= 0/)
      end

      it 'raises an ArgumentError on negative values' do
        expect do
          subject.switch_connection(new_connection, max_payload_size: -1)
        end.to raise_error(ArgumentError, /max_payload_size cannot be <= 0/)
      end
    end
  end
end
