require 'spec_helper'

describe Datadog::Statsd::MessageBuffer do
  subject do
    described_class.new(connection,
      max_payload_size: max_payload_size,
      max_pool_size: max_pool_size,
      overflowing_stategy: overflowing_stategy,
      serializer: serializer,
      counter_aggregation: counter_aggregation,
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

  let(:counter_aggregation) do
    false
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

  describe 'counter aggregation' do
    let(:counter_aggregation) do
      true
    end

    let(:max_payload_size) do
      1_024
    end

    let(:max_pool_size) do
      Float::INFINITY
    end

    it 'flushes nothing when empty' do
      expect(connection).not_to receive(:write)

      subject.flush
    end

    it 'merges equivalent counters into a single message' do
      subject.add('mycounter:1|c|#env:prod')
      subject.add('mycounter:4|c|#env:prod')
      subject.add('mycounter:-2|c|#env:prod')

      expect(connection).to receive(:write).with('mycounter:3|c|#env:prod')

      subject.flush
    end

    it 'groups counters per tag set and sample rate, keeping insertion order' do
      subject.add('mycounter:1|c|#env:prod')
      subject.add('mycounter:1|c|@0.5|#env:prod')
      subject.add('mycounter:1|c|#env:staging')
      subject.add('mycounter:5|c|#env:prod')
      subject.add('mycounter:2|c|@0.5|#env:prod')
      subject.add('mycounter:3|c|#env:staging')

      expect(connection).to receive(:write).with(
        "mycounter:6|c|#env:prod\n" \
        "mycounter:3|c|@0.5|#env:prod\n" \
        "mycounter:4|c|#env:staging"
      )

      subject.flush
    end

    it 'never aggregates non-counter messages' do
      subject.add('mymetric:1|c')
      subject.add('mymetric:42|g')
      subject.add('mymetric:2|c')
      subject.add('mymetric:7|h')
      subject.add('mymetric:9|ms')
      subject.add('mymetric:5|s')
      subject.add('mymetric:3|d')

      expect(connection).to receive(:write).with(
        "mymetric:3|c\n" \
        "mymetric:42|g\n" \
        "mymetric:7|h\n" \
        "mymetric:9|ms\n" \
        "mymetric:5|s\n" \
        "mymetric:3|d"
      )

      subject.flush
    end

    it 'interleaves a same-named gauge without altering the counter groups' do
      subject.add('mymetric:1|c|#env:prod')
      subject.add('mymetric:99|g')
      subject.add('mymetric:2|c|#env:prod')

      expect(connection).to receive(:write).with(
        "mymetric:3|c|#env:prod\nmymetric:99|g"
      )

      subject.flush
    end

    it 'starts a new aggregation window after a flush' do
      subject.add('mycounter:1|c')
      expect(connection).to receive(:write).with('mycounter:1|c')
      subject.flush

      subject.add('mycounter:1|c')
      expect(connection).to receive(:write).with('mycounter:1|c')
      subject.flush
    end

    context 'when the pool size is reached' do
      let(:max_pool_size) do
        3
      end

      it 'counts each aggregation group only once towards the pool size' do
        expect(connection).to receive(:write).once

        # 3 distinct groups must not flush, the fourth one must trigger a flush
        subject.add('a:1|c')
        subject.add('a:1|c') # merged, does not grow the pool
        subject.add('b:1|c')
        subject.add('c:1|c')
        subject.add('d:1|c') # 4th group -> preemptive flush
      end
    end

    context 'when the payload size limit is reached' do
      let(:max_payload_size) do
        12
      end

      it 'splits the groups across packets once the preemptive size threshold is crossed' do
        # threshold = (12 - 5% * 12).to_i = 11 bytes
        # "a:2|c" is 5 bytes, "a:2|c\nb:1|c" is 11 bytes (at the threshold),
        # adding "c:1|c" crosses it and flushes the first window
        expect(connection).to receive(:write).with("a:2|c\nb:1|c").ordered
        expect(connection).to receive(:write).with('c:1|c').ordered

        subject.add('a:1|c')
        subject.add('a:1|c') # merged into "a:2|c"
        subject.add('b:1|c')
        subject.add('c:1|c') # crosses the threshold -> flush a/b, buffer c
        subject.flush
      end

      it 'never emits a payload larger than the configured limit' do
        allow(connection).to receive(:write) do |payload|
          expect(payload.bytesize).to be <= max_payload_size
        end

        100.times do |i|
          subject.add("counter#{i}:1|c")
          subject.add("counter#{i}:1|c") # merged into the same group
        end
        subject.flush
      end
    end

    context 'with the default (disabled) counter aggregation' do
      let(:counter_aggregation) do
        false
      end

      it 'sends every counter independently' do
        subject.add('mycounter:1|c')
        subject.add('mycounter:1|c')

        expect(connection).to receive(:write).with("mycounter:1|c\nmycounter:1|c")

        subject.flush
      end
    end
  end
end
