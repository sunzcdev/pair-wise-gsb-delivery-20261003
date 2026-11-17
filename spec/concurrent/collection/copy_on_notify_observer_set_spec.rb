require 'concurrent/collection/copy_on_notify_observer_set'
require_relative 'observer_set_shared'

module Concurrent
  module Collection
    RSpec.describe CopyOnNotifyObserverSet do
      it_behaves_like 'an observer set'

      describe '#notify_observers_safely' do
        let(:observer_set) { described_class.new }
        let(:observer) { double('observer') }
        let(:another_observer) { double('another observer') }
        let(:failing_observer) { double('failing observer') }
        let(:third_observer) { double('third observer') }

        it 'notifies every observer with the given arguments' do
          expect(observer).to receive(:update).with(1, 2, 3)
          expect(another_observer).to receive(:update).with(1, 2, 3)

          observer_set.add_observer(observer)
          observer_set.add_observer(another_observer)

          expect(observer_set.notify_observers_safely(1, 2, 3)).to eq([])
        end

        it 'keeps notifying after an observer raises and returns the failures' do
          expect(failing_observer).to receive(:update).and_raise(StandardError, 'boom')
          expect(another_observer).to receive(:update).with(:arg)
          expect(third_observer).to receive(:update).with(:arg)

          observer_set.add_observer(failing_observer)
          observer_set.add_observer(another_observer)
          observer_set.add_observer(third_observer)

          errors = observer_set.notify_observers_safely(:arg)

          expect(errors.length).to eq 1
          expect(errors.first.first).to eq failing_observer
          expect(errors.first.last.message).to eq 'boom'
        end

        it 'calls each observer exactly once' do
          expect(observer).to receive(:update).once
          observer_set.add_observer(observer)

          observer_set.notify_observers_safely
        end

        it 'does not remove observers' do
          allow(observer).to receive(:update)
          observer_set.add_observer(observer)
          observer_set.notify_observers_safely

          expect(observer_set.count_observers).to eq 1
        end

        it 'honours the function registered for each observer' do
          expect(observer).to receive(:notify).with(:arg)
          observer_set.add_observer(observer, :notify)

          observer_set.notify_observers_safely(:arg)
        end

        it 'computes the arguments per observer from the given block' do
          expect(observer).to receive(:update).with(:one)
          expect(another_observer).to receive(:update).with(:two)

          observer_set.add_observer(observer)
          observer_set.add_observer(another_observer)

          observer_set.notify_observers_safely do |observed, _function|
            observed.equal?(observer) ? [:one] : [:two]
          end
        end

        it 'collects a failure raised while building the arguments' do
          expect(another_observer).to receive(:update).with(:arg)
          observer_set.add_observer(observer)
          observer_set.add_observer(another_observer)

          errors = observer_set.notify_observers_safely(:arg) do |observed, _function|
            raise 'builder failed' if observed.equal?(observer)
            [:arg]
          end

          expect(errors.length).to eq 1
          expect(errors.first.last.message).to eq 'builder failed'
        end

        it 'does not raise when there are no observers' do
          expect { observer_set.notify_observers_safely(:arg) }.not_to raise_error
        end
      end
    end
  end
end
