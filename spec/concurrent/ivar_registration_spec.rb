require 'concurrent/ivar'
require 'concurrent/atomic/count_down_latch'

module Concurrent

  RSpec.describe IVar do

    let(:fulfilled_value) { 42 }
    let(:rejected_reason) { StandardError.new('Boom!') }

    # A recording observer with an optional failing behaviour.
    let(:observer_class) do
      Class.new do
        attr_reader :events

        def initialize(on_update = nil)
          @events    = []
          @on_update = on_update
        end

        def update(time, value, reason)
          @events << [time, value, reason]
          @on_update.call(time, value, reason) if @on_update
        end

        def count
          @events.size
        end
      end
    end

    context '#register_observer' do

      it 'returns an IVarObserverRegistration handle' do
        ivar = IVar.new
        handle = ivar.register_observer(observer_class.new)
        expect(handle).to be_a(Collection::IVarObserverRegistration)
      end

      it 'accepts a block as the observer and calls it with :call' do
        ivar   = IVar.new
        events = []
        ivar.register_observer { |time, value, reason| events << [value, reason] }
        ivar.set(fulfilled_value)

        expect(events).to contain_exactly([fulfilled_value, nil])
      end

      it 'raises when neither an observer nor a block is given' do
        expect { IVar.new.register_observer }.to raise_error(ArgumentError)
      end

      it 'raises when both an observer and a block are given' do
        expect {
          IVar.new.register_observer(observer_class.new) { nil }
        }.to raise_error(ArgumentError)
      end

      it 'exposes the observer and the notification function on the handle' do
        ivar     = IVar.new
        observer = observer_class.new
        handle   = ivar.register_observer(observer, :update)

        expect(handle.observer).to be(observer)
        expect(handle.func).to eq(:update)
      end

      it 'starts in the pending state while the IVar is pending' do
        handle = IVar.new.register_observer(observer_class.new)

        expect(handle).to be_pending
        expect(handle).not_to be_notified
        expect(handle).not_to be_deregistered
      end
    end

    context 'notification' do

      it 'notifies a registered observer once on fulfillment' do
        ivar     = IVar.new
        observer = observer_class.new
        ivar.register_observer(observer)

        ivar.set(fulfilled_value)

        expect(observer.count).to eq(1)
        time, value, reason = observer.events.first
        expect(time).to be_a(Time)
        expect(value).to eq(fulfilled_value)
        expect(reason).to be_nil
      end

      it 'notifies a registered observer once on rejection' do
        ivar     = IVar.new
        observer = observer_class.new
        ivar.register_observer(observer)

        ivar.fail(rejected_reason)

        expect(observer.count).to eq(1)
        _time, value, reason = observer.events.first
        expect(value).to be_nil
        expect(reason).to be(rejected_reason)
      end

      it 'notifies every registered observer exactly once' do
        ivar     = IVar.new
        first    = observer_class.new
        second   = observer_class.new
        ivar.register_observer(first)
        ivar.register_observer(second)

        ivar.set(fulfilled_value)

        expect(first.count).to eq(1)
        expect(second.count).to eq(1)
      end

      it 'does not notify an observer twice when the IVar fails to set again' do
        ivar     = IVar.new
        observer = observer_class.new
        ivar.register_observer(observer)
        ivar.set(fulfilled_value)

        expect { ivar.set(7) }.to raise_error(MultipleAssignmentError)

        expect(observer.count).to eq(1)
        expect(ivar.value).to eq(fulfilled_value)
      end

      it 'marks the handle notified after completion' do
        ivar   = IVar.new
        handle = ivar.register_observer(observer_class.new)

        ivar.set(fulfilled_value)

        expect(handle).to be_notified
        expect(handle).not_to be_pending
        expect(handle).not_to be_deregistered
      end
    end

    context 'deregistration racing completion' do

      it 'returns :deregistered and suppresses notification when removed first' do
        ivar     = IVar.new
        observer = observer_class.new
        handle   = ivar.register_observer(observer)

        expect(ivar.count_observers).to eq(1)
        expect(handle.deregister).to eq(:deregistered)

        expect(ivar.count_observers).to eq(0)
        expect(handle).to be_deregistered
        expect(handle).not_to be_notified

        ivar.set(fulfilled_value)

        expect(observer.count).to eq(0)
      end

      it 'returns :notified when the IVar completed first and still notifies' do
        ivar     = IVar.new
        observer = observer_class.new
        handle   = ivar.register_observer(observer)

        ivar.set(fulfilled_value)

        expect(handle.deregister).to eq(:notified)
        expect(handle).to be_notified
        expect(observer.count).to eq(1)
        expect(ivar.count_observers).to eq(0)
      end

      it 'is idempotent and keeps reporting the first deregistration outcome' do
        ivar   = IVar.new
        handle = ivar.register_observer(observer_class.new)

        expect(handle.deregister).to eq(:deregistered)
        expect(handle.deregister).to eq(:deregistered)
        expect(handle.unsubscribe).to eq(:deregistered)
      end

      it 'reports a stable :notified outcome when deregistering after completion twice' do
        ivar   = IVar.new
        handle = ivar.register_observer(observer_class.new)
        ivar.set(fulfilled_value)

        expect(handle.deregister).to eq(:notified)
        expect(handle.deregister).to eq(:notified)
      end

      it 'still notifies the other observers when one is deregistered' do
        ivar      = IVar.new
        kept      = observer_class.new
        removed   = observer_class.new
        ivar.register_observer(kept)
        removed_handle = ivar.register_observer(removed)
        removed_handle.deregister

        ivar.set(fulfilled_value)

        expect(kept.count).to eq(1)
        expect(removed.count).to eq(0)
      end

      it 'produces a definitive result under concurrent deregistration and completion' do
        25.times do
          ivar      = IVar.new
          observers = Array.new(4) { observer_class.new }
          handles   = observers.map { |observer| ivar.register_observer(observer) }

          start = CountDownLatch.new(1)

          threads = [
            in_thread { start.wait(1); ivar.set(fulfilled_value) },
            in_thread { start.wait(1); handles.each(&:deregister) }
          ]
          start.count_down
          threads.each { |thread| thread.join(5) }

          handles.each do |handle|
            expect([:deregistered, :notified]).to include(handle.deregister)
            if handle.notified?
              expect(handle.observer.count).to eq(1)
            else
              expect(handle.observer.count).to eq(0)
            end
          end

          # Whatever the race outcome, the IVar value and state never change.
          expect(ivar.value).to eq(fulfilled_value)
          expect(ivar).to be_fulfilled
        end
      end
    end

    context 'observers added after completion' do

      it 'notifies a new observer immediately, from the calling thread' do
        ivar     = IVar.new.set(fulfilled_value)
        observer = observer_class.new

        ivar.register_observer(observer)

        expect(observer.count).to eq(1)
        expect(observer.events.first[1]).to eq(fulfilled_value)
      end

      it 'returns an already notified handle for a late observer' do
        ivar   = IVar.new.set(fulfilled_value)
        handle = ivar.register_observer(observer_class.new)

        expect(handle).to be_notified
        expect(handle.deregister).to eq(:notified)
      end

      it 'does not notify existing observers again when a new one is added' do
        ivar   = IVar.new
        first  = observer_class.new
        ivar.register_observer(first)
        ivar.set(fulfilled_value)
        expect(first.count).to eq(1)

        ivar.register_observer(observer_class.new)

        expect(first.count).to eq(1)
        expect(ivar.count_observers).to eq(0)
      end

      it 'notifies a late observer of a rejection with the stored reason' do
        ivar     = IVar.new.fail(rejected_reason)
        observer = observer_class.new

        ivar.register_observer(observer)

        _time, value, reason = observer.events.first
        expect(value).to be_nil
        expect(reason).to be(rejected_reason)
      end
    end

    context 're-registration' do

      it 'replaces the previous handle and never notifies it' do
        ivar        = IVar.new
        observer    = observer_class.new
        old_handle  = ivar.register_observer(observer)
        new_handle  = ivar.register_observer(observer)

        expect(old_handle).to be_deregistered
        expect(new_handle).to be_pending
        expect(ivar.count_observers).to eq(1)

        ivar.set(fulfilled_value)

        expect(observer.count).to eq(1)
        expect(new_handle).to be_notified
      end
    end

    context 'legacy #add_observer / #delete_observer API' do

      it 'keeps returning the observer object from #add_observer' do
        ivar     = IVar.new
        observer = observer_class.new
        expect(ivar.add_observer(observer)).to be(observer)
      end

      it 'supports a block observer through #add_observer' do
        ivar   = IVar.new
        events = []
        ivar.add_observer { |_time, value, _reason| events << value }
        ivar.set(fulfilled_value)

        expect(events).to eq([fulfilled_value])
      end

      it 'removes an observer through #delete_observer and returns it' do
        ivar     = IVar.new
        observer = observer_class.new
        ivar.add_observer(observer)

        expect(ivar.delete_observer(observer)).to be(observer)
        expect(ivar.count_observers).to eq(0)

        ivar.set(fulfilled_value)
        expect(observer.count).to eq(0)
      end

      it 'returns the observer even when it was not registered' do
        observer = observer_class.new
        expect(IVar.new.delete_observer(observer)).to be(observer)
      end

      it 'removes all observers through #delete_observers' do
        ivar = IVar.new
        3.times { ivar.register_observer(observer_class.new) }

        expect(ivar.delete_observers).to be(ivar)
        expect(ivar.count_observers).to eq(0)
      end
    end

    context 'observer callback failures' do

      let(:failing_observer_class) do
        Class.new do
          attr_reader :notified

          def initialize(raise_on_update:)
            @raise_on_update = raise_on_update
            @notified        = 0
          end

          def update(_time, _value, _reason)
            @notified += 1
            raise 'observer failure' if @raise_on_update
          end
        end
      end

      it 'keeps notifying the remaining observers when one callback raises' do
        ivar    = IVar.new
        failing = failing_observer_class.new(raise_on_update: true)
        healthy = observer_class.new
        ivar.register_observer(failing)
        ivar.register_observer(healthy)

        # The error must not bubble out of #set.
        expect { ivar.set(fulfilled_value) }.not_to raise_error

        expect(failing.notified).to eq(1)
        expect(healthy.count).to eq(1)
      end

      it 'does not change the fulfilled value when a callback raises' do
        ivar = IVar.new
        ivar.register_observer(failing_observer_class.new(raise_on_update: true))

        ivar.set(fulfilled_value)

        expect(ivar).to be_fulfilled
        expect(ivar.value).to eq(fulfilled_value)
        expect(ivar.reason).to be_nil
      end

      it 'does not change the rejection reason when a callback raises' do
        ivar = IVar.new
        ivar.register_observer(failing_observer_class.new(raise_on_update: true))

        ivar.fail(rejected_reason)

        expect(ivar).to be_rejected
        expect(ivar.value).to be_nil
        expect(ivar.reason).to be(rejected_reason)
      end

      it 'isolates a failure of a directly notified late observer' do
        ivar = IVar.new.set(fulfilled_value)

        expect {
          ivar.register_observer(failing_observer_class.new(raise_on_update: true))
        }.not_to raise_error

        expect(ivar.value).to eq(fulfilled_value)
      end
    end

    context 'reentrancy and lock discipline' do

      # A reentrant observer queries the IVar from inside its own callback.
      let(:reentrant_observer_class) do
        Class.new do
          attr_reader :seen_value, :seen_state, :seen_reason

          def initialize(ivar)
            @ivar = ivar
          end

          def update(_time, _value, _reason)
            @seen_value  = @ivar.value
            @seen_state  = @ivar.state
            @seen_reason = @ivar.reason
          end
        end
      end

      it 'allows a completion callback to query the IVar reentrantly' do
        ivar     = IVar.new
        observer = reentrant_observer_class.new(ivar)
        ivar.register_observer(observer)

        ivar.set(fulfilled_value)

        expect(observer.seen_value).to eq(fulfilled_value)
        expect(observer.seen_state).to eq(:fulfilled)
        expect(observer.seen_reason).to be_nil
      end

      it 'allows a direct callback to query the IVar reentrantly' do
        ivar     = IVar.new.set(fulfilled_value)
        observer = reentrant_observer_class.new(ivar)

        ivar.register_observer(observer)

        expect(observer.seen_value).to eq(fulfilled_value)
      end

      it 'allows deregistering from inside a callback without deadlocking' do
        ivar   = IVar.new
        handle = nil
        observer = Class.new do
          attr_reader :ran

          def initialize(registration_holder)
            @holder = registration_holder
            @ran    = 0
          end

          def update(_time, _value, _reason)
            @ran += 1
            @holder.call.deregister
          end
        end.new(proc { handle })

        handle = ivar.register_observer(observer)
        expect { ivar.set(fulfilled_value) }.not_to raise_error
        expect(observer.ran).to eq(1)
        expect(handle).to be_notified
      end

      it 'does not hold the IVar lock while invoking callbacks' do
        ivar    = IVar.new
        entered = CountDownLatch.new(1)
        release = CountDownLatch.new(1)

        blocking = Class.new do
          def initialize(entered, release)
            @entered = entered
            @release = release
          end

          def update(_time, _value, _reason)
            @entered.count_down
            @release.wait(2)
          end
        end.new(entered, release)

        ivar.register_observer(blocking)
        in_thread { ivar.set(fulfilled_value) }

        expect(entered.wait(1)).to be_truthy

        # While the callback is still running the lock must be available.
        locked = ivar.send(:synchronize) { :lock_acquired }
        expect(locked).to eq(:lock_acquired)

        release.count_down
      end
    end
  end
end
