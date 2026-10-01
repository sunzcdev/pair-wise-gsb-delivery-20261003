require 'weakref'
require 'concurrent/atomic/cyclic_barrier'
require 'concurrent/ivar'

module Concurrent

  RSpec.describe IVar do

    let(:ivar) { IVar.new }

    # Observer whose invocation count and arguments are protected by its own
    # mutex, because the callback may run on a thread other than the test
    # thread.
    let(:counting_observer_class) do
      Class.new do
        attr_reader :events
        def initialize(&block)
          @mutex = Mutex.new
          @block = block
          @events = []
        end
        def count
          @mutex.synchronize { @events.size }
        end
        def last
          @mutex.synchronize { @events.last }
        end
        def update(time, value, reason)
          @mutex.synchronize { @events << [value, reason] }
          @block.call(time, value, reason) if @block
        end
      end
    end

    # GC helper. The block must return a WeakRef and must not itself hold any
    # other strong reference to the object (it runs in an isolated method
    # scope so its locals die immediately).
    def assert_collectable!
      ref = yield
      40.times do
        GC.start(full_mark: true, immediate_sweep: true)
        return unless ref.weakref_alive?
        sleep 0.005
      end
      raise 'object was still strongly referenced after garbage collection'
    end

    describe '#subscribe' do

      it 'returns a Subscription handle' do
        expect(ivar.subscribe {}).to be_a IVar::Subscription
      end

      it 'raises when given neither an observer nor a block' do
        expect { ivar.subscribe }.to raise_error(ArgumentError)
      end

      it 'raises when given both an observer and a block' do
        expect { ivar.subscribe(Object.new) {} }.to raise_error(ArgumentError)
      end

      it 'invokes an observer object once with (time, value, nil) on fulfillment' do
        observer = Class.new {
          attr_reader :args
          def update(*a); @args = a; end
        }.new
        ivar.subscribe(observer)
        ivar.set(42)

        expect(observer.args.size).to eq 3
        expect(observer.args[0]).to be_a Time
        expect(observer.args[1]).to eq 42
        expect(observer.args[2]).to be_nil
      end

      it 'invokes a block once with (time, nil, reason) on rejection' do
        reason = RuntimeError.new('boom')
        got = nil
        ivar.subscribe { |_time, value, err| got = [value, err] }
        ivar.fail(reason)
        expect(got).to eq [nil, reason]
      end

      it 'invokes every subscription exactly once and independently' do
        observers = Array.new(4) { counting_observer_class.new }
        handles = observers.map { |o| ivar.subscribe(o) }

        ivar.set(1)
        begin
          ivar.set(2)
        rescue MultipleAssignmentError
        end

        observers.each_with_index do |o, i|
          expect(o.count).to eq(1), "observer #{i} was called #{o.count} times"
          expect(o.last).to eq [1, nil]
        end
        expect(handles).to all(be_notified)
      end

      it 'allows the same observer object to be subscribed several times' do
        observer = counting_observer_class.new
        s1 = ivar.subscribe(observer)
        s2 = ivar.subscribe(observer)
        expect(s1).not_to eq s2
        expect(ivar.count_observers).to eq 2

        ivar.set(9)
        expect(observer.count).to eq 2
      end

      context 'when the IVar has already completed' do

        it 'delivers immediately and synchronously to the block' do
          ivar.set(7)
          value = nil
          subscription = ivar.subscribe { |_t, v, _r| value = v }
          expect(value).to eq 7
          expect(subscription).to be_notified
        end

        it 'delivers the rejection reason immediately' do
          reason = RuntimeError.new('x')
          ivar.fail(reason)
          got = nil
          ivar.subscribe { |_t, _v, r| got = r }
          expect(got).to eq reason
        end

        it 'delivers again on every subsequent subscription, once per call' do
          ivar.set(3)
          observer = counting_observer_class.new
          3.times { ivar.subscribe(observer) }
          expect(observer.count).to eq 3
        end

        it 'does not store the subscription in the observer set' do
          ivar.set(3)
          ivar.subscribe {}
          expect(ivar.count_observers).to eq 0
        end

        it 'runs the immediate callback without holding the IVar lock' do
          ivar.set(3)
          # Re-entering #value would deadlock if the lock were held.
          expect { ivar.subscribe { |_t, _v, _r| ivar.value } }.not_to raise_error
        end
      end
    end

    describe IVar::Subscription do
      describe '#cancel' do

        it 'returns true and prevents the callback while pending' do
          observer = counting_observer_class.new
          subscription = ivar.subscribe(observer)

          expect(subscription.cancel).to eq true
          expect(subscription).to be_cancelled
          ivar.set(1)
          expect(observer.count).to eq 0
        end

        it 'removes the subscription from the observer set immediately' do
          s1 = ivar.subscribe {}
          s2 = ivar.subscribe {}
          expect(ivar.count_observers).to eq 2
          expect(s1.cancel).to eq true
          expect(ivar.count_observers).to eq 1
          ivar.set(1)
          expect(s2).to be_notified
        end

        it 'is idempotent: only the first winning call returns true' do
          subscription = ivar.subscribe {}
          expect(subscription.cancel).to eq true
          expect(subscription.cancel).to eq false
          expect(subscription.cancel).to eq false
        end

        it 'returns false once the callback has already run' do
          ivar.set(1)
          subscription = ivar.subscribe {}
          expect(subscription).to be_notified
          expect(subscription.cancel).to eq false
        end

        it 'returns false when called from inside its own callback' do
          handle = nil
          result = nil
          handle = ivar.subscribe { result = handle.cancel }
          ivar.set(1)
          expect(result).to eq false
          expect(handle).to be_notified
        end

        it 'can cancel a different subscription from inside a callback' do
          late_ran = false
          other = nil
          first_block_started = Queue.new
          allow_first_to_finish = Queue.new

          ivar.subscribe do
            first_block_started << 1
            allow_first_to_finish.pop
            other.cancel
          end
          other = ivar.subscribe { late_ran = true }

          # Complete on another thread. The notification loop delivers the
          # first observer (which blocks), the second observer is still
          # waiting in the loop. Cancelling it from within the first callback
          # must make the loop skip it.
          completer = in_thread { ivar.set(1) }
          first_block_started.pop
          allow_first_to_finish << 1
          expect(completer.join(5)).not_to be_nil

          expect(late_ran).to eq false
          expect(other).to be_cancelled
        end

        it 're-subscribing from inside a callback delivers immediately once' do
          seen = []
          ivar.subscribe do |_t, v, _r|
            seen << v
            ivar.subscribe { |_t2, v2, _r2| seen << [:again, v2] }
          end
          ivar.set(11)
          expect(seen).to eq [11, [:again, 11]]
        end
      end

      describe '#notified?' do

        it 'is false for a waiting subscription' do
          expect(ivar.subscribe {}).not_to be_notified
        end

        it 'is true while the callback is running and after it finishes' do
          handle = nil
          states = []
          entered = Queue.new
          release = Queue.new
          handle = ivar.subscribe do
            states << handle.notified?
            entered << 1
            release.pop
          end

          notifier = in_thread { ivar.set(1) }
          entered.pop
          expect(handle.notified?).to eq true
          release << 1
          expect(notifier.join(5)).not_to be_nil
          expect(states).to eq [true]
          expect(handle.notified?).to eq true
        end
      end

      describe '#cancelled?' do

        it 'is false for waiting and notified subscriptions' do
          s = ivar.subscribe {}
          expect(s).not_to be_cancelled
          ivar.set(1)
          expect(s).not_to be_cancelled
        end
      end

      describe 'completion versus cancellation race' do

        it 'has exactly one outcome per subscription across many races' do
          delivered = 0
          cancelled = 0
          iterations = 500

          iterations.times do
            raced_ivar = IVar.new
            fired = Queue.new
            subscription = raced_ivar.subscribe { fired << 1 }
            barrier = CyclicBarrier.new(2)

            completer = in_thread { barrier.wait; raced_ivar.set(1) }
            canceller = in_thread { barrier.wait; subscription.cancel }

            expect(completer.join(5)).not_to be_nil
            expect(canceller.join(5)).not_to be_nil

            if fired.empty?
              cancelled += 1
              expect(subscription).to be_cancelled
            else
              delivered += 1
              expect(fired.size).to eq 1
            end
          end

          expect(delivered + cancelled).to eq iterations
        end

        it 'cancelling a later subscription does not affect an earlier one' do
          first_ran = Queue.new
          first_can_finish = Queue.new
          other = nil

          ivar.subscribe do
            first_ran << 1
            first_can_finish.pop
          end
          other = ivar.subscribe { }

          # Complete on a background thread. The first callback announces
          # itself and then blocks, so it has demonstrably started while the
          # second subscription is still waiting inside the notification
          # loop. Cancelling the second one must not undo the first delivery.
          completer = in_thread { ivar.set(1) }
          first_ran.pop
          expect(other.cancel).to eq true
          first_can_finish << 1
          expect(completer.join(5)).not_to be_nil
        end
      end

      describe 'failure isolation' do

        it 'does not let one raising observer stop the others' do
          order = Queue.new
          ivar.subscribe { order << :before; raise 'observer failure' }
          ivar.subscribe { |_t, v, _r| order << v }
          ivar.subscribe { order << :after }

          expect { ivar.set(42) }.not_to raise_error
          expect([order.pop, order.pop, order.pop]).to eq [:before, 42, :after]
        end

        it 'still completes the IVar when an observer raises' do
          ivar.subscribe { raise 'observer failure' }
          expect { ivar.set(1) }.not_to raise_error
          expect(ivar).to be_complete
          expect(ivar.value).to eq 1
        end
      end

      describe 'strong references' do

        let(:silent_observer_class) do
          Class.new { def update(*); end }
        end

        it 'releases the observer immediately after cancel while pending' do
          assert_collectable! do
            observer = silent_observer_class.new
            ref = WeakRef.new(observer)
            ivar.subscribe(observer).cancel
            ref
          end
        end

        it 'releases observers once completion has delivered them' do
          assert_collectable! do
            observer = silent_observer_class.new
            ref = WeakRef.new(observer)
            ivar.subscribe(observer)
            ivar.set(1)
            ref
          end
        end

        it 'releases an immediately delivered observer' do
          ivar.set(1)
          assert_collectable! do
            observer = silent_observer_class.new
            ref = WeakRef.new(observer)
            ivar.subscribe(observer)
            ref
          end
        end

        it 'releases a captured request object when cancelled while pending' do
          assert_collectable! do
            request = Object.new
            ref = WeakRef.new(request)
            subscription = ivar.subscribe { |_t, _v, _r| request }
            expect(subscription.cancel).to eq true
            ref
          end
        end
      end
    end
  end
end
