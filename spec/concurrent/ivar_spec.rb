require_relative 'ivar_shared'
require 'concurrent/ivar'
require 'concurrent/atomic/atomic_fixnum'
require 'concurrent/atomic/atomic_reference'
require 'concurrent/atomic/count_down_latch'
require 'weakref'

module Concurrent

  RSpec.describe IVar do

    let!(:value) { 10 }

    let!(:fulfilled_value) { 10 }
    let(:rejected_reason) { StandardError.new('Boom!') }

    subject { IVar.new(value) }

    let(:pending_subject) do
      ivar = IVar.new
      in_thread do
        sleep(0.1)
        ivar.set(fulfilled_value)
      end
      ivar
    end

    let(:fulfilled_subject) do
      IVar.new.set(fulfilled_value)
    end

    let(:rejected_subject) do
      IVar.new.fail(rejected_reason)
    end

    it_should_behave_like :ivar do
      subject{ IVar.new }

      def dereferenceable_subject(value, opts = {})
        IVar.new(value, opts)
      end

      def dereferenceable_observable(opts = {})
        IVar.new(NULL, opts)
      end

      def execute_dereferenceable(subject)
        subject.set('value')
      end

      def trigger_observable(observable)
        observable.set('value')
      end
    end

    context '#initialize' do

      it 'does not have to set an initial value' do
        i = IVar.new
        expect(i).to be_incomplete
      end

      it 'does not set an initial value if you pass NULL' do
        i = IVar.new(NULL)
        expect(i).to be_incomplete
      end

      it 'can set an initial value' do
        i = IVar.new(14)
        expect(i).to be_complete
        expect(i.value).to eq 14
      end

      it 'can set an initial value with a block' do
        i = IVar.new{ 42 }
        expect(i).to be_complete
        expect(i.value).to eq 42
      end

      it 'raises an exception if given both a value and a block' do
        expect {
          IVar.new(42){ 42 }
        }.to raise_error(ArgumentError)
      end
    end

    context 'observation' do

      let(:clazz) do
        Class.new do
          attr_reader :value
          attr_reader :reason
          attr_reader :count
          define_method(:update) do |time, value, reason|
            @count ||= 0
            @count = @count.to_i + 1
            @value = value
            @reason = reason
          end
        end
      end

      let(:observer) { clazz.new }

      it 'notifies all observers on #set' do
        i = IVar.new
        i.add_observer(observer)

        i.set(42)

        expect(observer.value).to eq(42)
        expect(observer.reason).to be_nil
      end

      context 'deadlock avoidance' do

        def reentrant_observer(i)
          obs = ::Object.new
          obs.define_singleton_method(:update) do |time, value, reason|
            @value = i.value
          end
          obs.define_singleton_method(:value) { @value }
          obs
        end

        it 'should notify observers outside mutex lock' do
          i = IVar.new
          obs = reentrant_observer(i)

          i.add_observer(obs)
          i.set(42)

          expect(obs.value).to eq 42
        end

        it 'should notify a new observer added after fulfillment outside lock' do
          i = IVar.new
          i.set(42)
          obs = reentrant_observer(i)

          i.add_observer(obs)

          expect(obs.value).to eq 42
        end
      end
    end

    context '#observe' do

      it 'returns a subscription handle' do
        ivar = IVar.new
        subscription = ivar.observe { }
        expect(subscription).to be_a IVar::Subscription
        expect(subscription).to be_pending
      end

      it 'raises when given both an observer and a block' do
        ivar = IVar.new
        expect {
          ivar.observe(Object.new) { }
        }.to raise_error(ArgumentError)
      end

      it 'raises when given neither an observer nor a block' do
        ivar = IVar.new
        expect { ivar.observe }.to raise_error(ArgumentError)
      end

      it 'notifies the observer with time, value and reason on fulfillment' do
        ivar = IVar.new
        args = nil
        ivar.observe { |time, value, reason| args = [time, value, reason] }

        ivar.set(42)

        expect(args[0]).to be_a Time
        expect(args[1]).to eq 42
        expect(args[2]).to be_nil
      end

      it 'notifies the observer with the reason on rejection' do
        expected = StandardError.new('Boom!')
        ivar = IVar.new
        args = nil
        ivar.observe { |time, value, reason| args = [time, value, reason] }

        ivar.fail(expected)

        expect(args[1]).to be_nil
        expect(args[2]).to eq expected
      end

      it 'supports the observer/func form' do
        observer = Class.new do
          attr_reader :value
          attr_reader :reason
          define_method(:update) do |time, value, reason|
            @value = value
            @reason = reason
          end
        end.new
        ivar = IVar.new
        subscription = ivar.observe(observer)

        ivar.set(42)

        expect(subscription).to be_a IVar::Subscription
        expect(observer.value).to eq 42
        expect(observer.reason).to be_nil
      end

      it 'delivers exactly one notification per subscription' do
        ivar = IVar.new
        counts = Array.new(10) { Concurrent::AtomicFixnum.new(0) }
        counts.each { |counter| ivar.observe { counter.increment } }

        ivar.set(1)

        counts.each { |counter| expect(counter.value).to eq 1 }
      end

      context 'when already completed' do

        it 'notifies immediately and synchronously when fulfilled' do
          ivar = IVar.new.set(42)
          args = nil
          subscription = ivar.observe { |time, value, reason| args = [time, value, reason] }

          expect(args[1]).to eq 42
          expect(args[2]).to be_nil
          expect(subscription).to be_delivered
        end

        it 'notifies immediately and synchronously when rejected' do
          expected = StandardError.new('Boom!')
          ivar = IVar.new.fail(expected)
          args = nil
          ivar.observe { |time, value, reason| args = [time, value, reason] }

          expect(args[1]).to be_nil
          expect(args[2]).to eq expected
        end

        it 'reports false when cancelling an already delivered subscription' do
          ivar = IVar.new.set(42)
          subscription = ivar.observe { }

          expect(subscription.cancel).to be false
          expect(subscription).not_to be_cancelled
        end
      end

      context 'cancellation' do

        it 'returns true and guarantees the callback never runs when it wins the race' do
          ivar = IVar.new
          calls = Concurrent::AtomicFixnum.new(0)
          subscription = ivar.observe { calls.increment }

          expect(subscription.cancel).to be true
          expect(subscription).to be_cancelled

          ivar.set(42)
          expect(calls.value).to eq 0
        end

        it 'returns false on a second cancel' do
          ivar = IVar.new
          subscription = ivar.observe { }

          expect(subscription.cancel).to be true
          expect(subscription.cancel).to be false
        end

        it 'returns false when the callback has already started' do
          ivar = IVar.new
          started = Concurrent::CountDownLatch.new(1)
          release = Concurrent::CountDownLatch.new(1)
          calls = Concurrent::AtomicFixnum.new(0)

          subscription = ivar.observe do
            calls.increment
            started.count_down
            release.wait(5)
          end

          setter = Thread.new { ivar.set(42) }
          started.wait(5)

          expect(subscription.cancel).to be false
          expect(subscription).not_to be_cancelled

          release.count_down
          setter.join(5)
          expect(calls.value).to eq 1
          expect(subscription).to be_delivered
        end

        it 'has consistent semantics when completion and cancellation race' do
          50.times do
            ivar = IVar.new
            calls = Concurrent::AtomicFixnum.new(0)
            done = Concurrent::CountDownLatch.new(1)
            subscription = ivar.observe { calls.increment; done.count_down }

            setter = Thread.new { ivar.set(1) }
            cancelled = subscription.cancel
            setter.join(5)
            done.wait(5) unless cancelled

            # true means the callback is guaranteed to never run,
            # false means it had already started (and must run exactly once)
            expect(calls.value).to eq(cancelled ? 0 : 1)
          end
        end

        it 'removes the cancelled subscription from the observer set' do
          ivar = IVar.new
          kept = ivar.observe { }
          cancelled = ivar.observe { }

          cancelled.cancel

          subscriptions = ivar.instance_variable_get(:@subscriptions)
          expect(subscriptions).to have_key(kept)
          expect(subscriptions).not_to have_key(cancelled)
        end

        it 'does not strongly reference a cancelled subscription' do
          ivar = IVar.new
          subscription = ivar.observe { }
          weak = WeakRef.new(subscription)

          subscription.cancel
          subscription = nil
          2.times { GC.start }

          expect(weak.weakref_alive?).to be_falsey
        end

        it 'does not retain subscriptions after delivery' do
          ivar = IVar.new
          ivar.observe { }
          ivar.observe { }

          ivar.set(1)

          expect(ivar.instance_variable_get(:@subscriptions)).to be_empty
        end
      end

      context 'callback safety' do

        it 'runs callbacks outside the internal lock' do
          ivar = IVar.new
          observed = Concurrent::AtomicReference.new(nil)
          ivar.observe { observed.set(ivar.value) }

          ivar.set(42)

          expect(observed.value).to eq 42
        end

        it 'notifies the remaining observers when one callback raises' do
          ivar = IVar.new
          calls = Concurrent::AtomicFixnum.new(0)
          ivar.observe { raise 'boom' }
          ivar.observe { calls.increment }

          expect { ivar.set(1) }.not_to raise_error
          expect(calls.value).to eq 1
        end

        it 'notifies a subscription created from within a callback immediately' do
          ivar = IVar.new
          outer = Concurrent::AtomicFixnum.new(0)
          inner = Concurrent::AtomicFixnum.new(0)
          ivar.observe do
            outer.increment
            ivar.observe { inner.increment }
          end

          ivar.set(1)

          expect(outer.value).to eq 1
          expect(inner.value).to eq 1
        end

        it 'returns false when a subscription cancels itself from within its own callback' do
          ivar = IVar.new
          result = Concurrent::AtomicReference.new(nil)
          subscription = nil
          subscription = ivar.observe { result.set(subscription.cancel) }

          ivar.set(1)

          expect(result.value).to be false
          expect(subscription).to be_delivered
        end

        it 'lets a callback cancel a subscription whose notification has not started' do
          ivar = IVar.new
          first_calls = Concurrent::AtomicFixnum.new(0)
          second_calls = Concurrent::AtomicFixnum.new(0)
          cancel_result = Concurrent::AtomicReference.new(nil)
          second = nil
          ivar.observe do
            first_calls.increment
            cancel_result.set(second.cancel)
          end
          second = ivar.observe { second_calls.increment }

          ivar.set(1)

          expect(first_calls.value).to eq 1
          expect(cancel_result.value).to be true
          expect(second_calls.value).to eq 0
        end
      end
    end
  end
end
