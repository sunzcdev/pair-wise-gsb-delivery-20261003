require 'concurrent/edge/promises'
require 'concurrent/atomic/count_down_latch'
require_relative '../../support/controlled_executor'

RSpec.describe 'Concurrent::Promises cancellation' do

  include Concurrent::Promises::FactoryMethods

  # builds a future whose block waits for the latch, so the test controls when
  # the user block completes (proving running blocks are never killed)
  def gated_future(latch, executor = :io)
    future_on(executor) { latch.wait(5); :done }
  end

  describe 'independent terminal state' do
    it 'exposes :cancelled state distinct from :rejected and :fulfilled' do
      latch = Concurrent::CountDownLatch.new
      scope, future = cancellation_scope { gated_future(latch) }
      scope.cancel

      expect(future.state).to eq :cancelled
      expect(future).to be_cancelled
      expect(future).to be_resolved
      expect(future).not_to be_rejected
      expect(future).not_to be_fulfilled
      latch.count_down
    end

    it 'uses a dedicated CancelledFutureError, not a business exception' do
      latch = Concurrent::CountDownLatch.new
      scope, future = cancellation_scope { gated_future(latch) }
      scope.cancel

      expect(future.reason).to be_a Concurrent::CancelledFutureError
      expect(future.reason).not_to be_a RuntimeError
      expect { future.value! }.to raise_error Concurrent::CancelledFutureError
      latch.count_down
    end

    it 'a business rejection is never reported as cancellation' do
      _scope, future = cancellation_scope { future { raise 'boom' } }
      future.wait

      expect(future).to be_rejected
      expect(future).not_to be_cancelled
      expect(future.state).to eq :rejected
      expect(future.reason).to be_a RuntimeError
    end

    it 'does not trigger rejection callbacks, but triggers resolution callbacks' do
      latch = Concurrent::CountDownLatch.new
      scope, future = cancellation_scope { gated_future(latch) }
      events = Queue.new
      future.on_rejection! { events.push :rejected }
      future.on_fulfillment! { events.push :fulfilled }
      future.on_resolution! { |fulfilled, _value, reason| events.push [:resolved, fulfilled, reason.class] }
      scope.cancel

      expect(future).to be_cancelled
      expect(events.size).to eq 1
      expect(events.pop).to eq [:resolved, false, Concurrent::CancelledFutureError]
      latch.count_down
    end

    it 'rescue does not swallow the independent cancelled state' do
      latch = Concurrent::CountDownLatch.new
      scope, future = cancellation_scope { gated_future(latch).rescue { :recovered } }
      scope.cancel
      expect(future).to be_cancelled
      expect(future.reason).to be_a Concurrent::CancelledFutureError
      latch.count_down
    end

    it 'the cancelled future can be raised like a rejected one' do
      latch = Concurrent::CountDownLatch.new
      scope, future = cancellation_scope { gated_future(latch) }
      scope.cancel
      raised = (raise future rescue $!)
      expect(raised).to be_a Concurrent::CancelledFutureError
      latch.count_down
    end
  end

  describe 'explicit cancellation relationships' do
    it 'registers created futures as owned members of the active scope' do
      scope, future = cancellation_scope { future { 1 } }
      expect(scope.owned_nodes).to include future
      expect(future.owned_by).to include scope
    end

    it 'a derived node joins the scope through its blocker' do
      scope, derived = cancellation_scope { future { 1 }.then(&:succ) }
      derived.wait
      expect(derived.owned_by).to include scope
    end

    it 'exposes borrowed and owned relationships for inspection' do
      scope, (a, b) = cancellation_scope do
        head = future { 1 }
        [head.then(owned: false, &:succ), head.then(&:succ)]
      end
      a.wait; b.wait
      expect(a.borrowed_by).to include scope
      expect(b.owned_by).to include scope
      expect(scope.borrowed_nodes).to include a
    end
  end

  describe 'shared upstream reused by two branches' do
    it 'cancelling one derived result keeps the still-needed borrowed upstream alive' do
      latch = Concurrent::CountDownLatch.new
      _scope, (branch_a, branch_b) = cancellation_scope do
        shared = future { latch.wait(5); :shared }
        [shared.then { |v| [:a, v] }, shared.then { |v| [:b, v] }]
      end
      sleep 0.05
      branch_b.cancel

      expect(branch_b).to be_cancelled
      expect(branch_a).to be_pending

      latch.count_down
      expect(branch_a.value!).to eq [:a, :shared]
    end

    it 'scope cancellation cancels both branches and their now unneeded chain' do
      latch = Concurrent::CountDownLatch.new
      scope, (a, b) = cancellation_scope do
        shared = future { latch.wait(5); :shared }
        [shared.then { |v| [:a, v] }, shared.then { |v| [:b, v] }]
      end
      sleep 0.05
      scope.cancel

      expect(a).to be_cancelled
      expect(b).to be_cancelled
      latch.count_down
    end

    it 'an explicitly borrowed member survives scope cancellation' do
      latch = Concurrent::CountDownLatch.new
      scope, borrowed = cancellation_scope { gated_future(latch).then(owned: false, &:itself) }
      sleep 0.05
      scope.cancel
      expect(borrowed).to be_pending
      latch.count_down
      expect(borrowed.value!).to eq :done
    end
  end

  describe 'zip and flat relationships' do
    it 'keeps accurate ownership through a zip' do
      latch = Concurrent::CountDownLatch.new
      scope, zipped = cancellation_scope do
        a = future { latch.wait(5); 1 }
        b = future { 2 }
        zip(a, b)
      end
      sleep 0.05
      expect(zipped.owned_by).to include scope
      scope.cancel
      expect(zipped).to be_cancelled
      latch.count_down
    end

    it 'produces the cancelled state when a zipped input is cancelled' do
      latch = Concurrent::CountDownLatch.new
      scope, zipped = cancellation_scope do
        a = future { latch.wait(5); 1 }
        b = future { 2 }
        zip(a, b)
      end
      sleep 0.05
      scope.cancel
      sleep 0.05
      expect(zipped.state).to eq :cancelled
      latch.count_down
    end

    it 'keeps accurate relationships through flat flattening' do
      latch = Concurrent::CountDownLatch.new
      scope, flattened = cancellation_scope do
        future { latch.wait(5); future { 99 } }.flat
      end
      sleep 0.05
      expect(flattened.owned_by).to include scope
      scope.cancel
      expect(flattened).to be_cancelled
      latch.count_down
    end

    it 'a rejection inside a zip stays a rejection, not a cancellation' do
      _scope, zipped = cancellation_scope do
        zip(future { 1 }, future { raise 'boom' })
      end
      zipped.wait
      expect(zipped).to be_rejected
      expect(zipped).not_to be_cancelled
    end
  end

  describe 'already running user blocks' do
    it 'is not forcibly killed; it runs to completion but the future stays cancelled' do
      latch = Concurrent::CountDownLatch.new
      ran = false
      scope, future = cancellation_scope do
        future do
          latch.wait(5)
          ran = true
          :finished
        end
      end
      sleep 0.05 # make sure the block has started on the global executor
      scope.cancel
      expect(future).to be_cancelled
      latch.count_down
      sleep 0.2
      expect(ran).to eq true
      expect(future).to be_cancelled
    end
  end

  describe 'controlled executor acceptance and execution counts' do
    let(:executor) { Concurrent::ControlledExecutor.new }

    after { executor.release }

    it 'accepts exactly one task per node and executes each once when not cancelled' do
      head    = future_on(executor.labeled(:a)) { 1 }
      derived = head.then_on(executor.labeled(:b)) { |v| v + 1 }
      # the head is accepted immediately but the gate blocks its execution, so
      # the derived node cannot be accepted yet
      expect(executor.wait_for_accepted 1).to eq true
      expect(executor.accepted_by_label[:a].value).to eq 1
      expect(executor.accepted_by_label[:b].value).to eq 0

      executor.release
      expect(derived.value!).to eq 2
      expect(executor.wait_for_idle).to eq true
      expect(executor.accepted_count).to eq 2
      expect(executor.executed_count).to eq 2
      expect(executor.executed_by_label[:a].value).to eq 1
      expect(executor.executed_by_label[:b].value).to eq 1
    end

    it 'never accepts the task of a node cancelled before it becomes runnable' do
      latch = Concurrent::CountDownLatch.new
      scope, derived = cancellation_scope do
        # the head is gated on the io executor, its then node is posted to the
        # controlled executor only after the head resolves
        head = future { latch.wait(5); 1 }
        head.then_on(executor.labeled(:derived)) { |v| v + 1 }
      end
      scope.cancel
      expect(derived).to be_cancelled
      latch.count_down
      sleep 0.1
      expect(executor.accepted_count).to eq 0
      expect(executor.executed_count).to eq 0
    end

    it 'cancels a whole owned graph without accepting downstream tasks' do
      latch = Concurrent::CountDownLatch.new
      scope, tail = cancellation_scope do
        head = future { latch.wait(5); 1 }
        head.then_on(executor.labeled(:one)) { |v| v + 1 }.
             then_on(executor.labeled(:two)) { |v| v + 1 }
      end
      scope.cancel
      latch.count_down
      sleep 0.1
      expect(tail).to be_cancelled
      expect(executor.accepted_count).to eq 0
    end

    it 'a borrowed branch still gets its single accepted and executed task' do
      latch = Concurrent::CountDownLatch.new
      _scope, (kept, dropped) = cancellation_scope do
        shared = future { latch.wait(5); 1 }
        [shared.then_on(executor.labeled(:kept)) { |v| v + 1 },
         shared.then_on(executor.labeled(:dropped)) { |v| v + 2 }]
      end
      dropped.cancel
      latch.count_down
      # the surviving branch's task must be accepted exactly once and run
      expect(executor.wait_for_accepted 1).to eq true
      expect(executor.accepted_by_label[:kept].value).to eq 1
      expect(executor.accepted_by_label[:dropped].value).to eq 0

      executor.release
      expect(executor.wait_for_idle).to eq true
      expect(kept.value!).to eq 2
      expect(executor.executed_by_label[:kept].value).to eq 1
      expect(executor.executed_by_label[:dropped].value).to eq 0
    end

    it 'an accepted but not-yet-running task is not force-killed; it runs as a no-op' do
      head_latch = Concurrent::CountDownLatch.new
      scope, derived = cancellation_scope do
        head = future { head_latch.wait(5); 1 }
        head.then_on(executor.labeled(:gated)) { |v| v + 1 }
      end
      head_latch.count_down
      # once the head resolves the derived node is accepted, but the gate keeps
      # its block from actually running
      expect(executor.wait_for_accepted 1).to eq true
      scope.cancel
      expect(derived).to be_cancelled

      executor.release
      # the executor still runs the accepted task exactly once; it observes the
      # cancelled terminal state and performs no user work, without killing a
      # thread
      expect(executor.wait_for_idle).to eq true
      expect(executor.executed_by_label[:gated].value).to eq 1
      expect(derived).to be_cancelled
    end
  end

  describe 'CancellationScope' do
    it 'returns the scope and the block value' do
      scope, value = cancellation_scope { future { 41 }.then(&:succ) }
      expect(scope).to be_a Concurrent::Promises::CancellationScope
      expect(value.value!).to eq 42
    end

    it 'does not cancel events, which have no cancelled terminal state' do
      scope, event = cancellation_scope { resolvable_event }
      scope.cancel
      expect(event).not_to be_cancelled
      expect(event).to be_pending
      event.resolve
      expect(event).to be_resolved
    end

    it 'is idempotent on cancel' do
      latch = Concurrent::CountDownLatch.new
      scope, future = cancellation_scope { gated_future(latch) }
      expect(scope.cancel).to equal scope
      expect(scope.cancel).to equal scope
      expect(future).to be_cancelled
      latch.count_down
    end

    it 'reports cancelled? and remembers the reason' do
      reason = StandardError.new('custom cancel reason')
      latch = Concurrent::CountDownLatch.new
      scope, future = cancellation_scope { gated_future(latch) }
      scope.cancel reason
      expect(scope).to be_cancelled
      expect(scope.reason).to eq reason
      expect(future.reason).to eq reason
      latch.count_down
    end

    it 'does not affect futures created outside the scope' do
      outside = future { 7 }
      scope, _inside = cancellation_scope { future { 1 } }
      scope.cancel
      expect(outside.value!).to eq 7
      expect(outside.cancellation_edges).to be_empty
    end
  end
end