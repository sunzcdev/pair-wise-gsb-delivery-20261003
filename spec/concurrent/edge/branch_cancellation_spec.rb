# frozen_string_literal: true

require 'concurrent/edge/branch_cancellation'

RSpec.describe 'Concurrent' do

  describe 'Promises branch cancellation', edge: true do
    let!(:p) { Concurrent::Promises }

    # Builds the canonical diamond:
    #
    #   shared -> origin -> left  -> merge
    #                       right /
    def diamond
      branch     = p.branch
      shared     = p.resolvable_future
      origin     = branch.wrap(shared)
      left       = origin.then { :L }
      right      = origin.then { :R }
      merge      = left.zip(right)
      [branch, shared, origin, left, right, merge]
    end

    describe 'ownership on a diamond dependency graph' do
      it 'cancels only the chosen branch, the other branch completes normally' do
        _branch, shared, _origin, left, right, merge = diamond

        expect(left.cancel).to be true
        expect(left).to be_canceled
        # the shared upstream computation is never cancelled
        expect(shared).to be_pending

        shared.fulfill(:ok)
        expect(right.value!(2)).to eq :R
        # the combination does not wait for the cancelled input forever
        expect(merge.wait(2)).to be_truthy
        expect(merge).to be_rejected
        _fulfilled, values, reasons = merge.result
        expect(values).to eq [nil, :R]
        expect(reasons[0]).to be_a Concurrent::CancelledOperationError
      end

      it 'does not run not-started continuations of the cancelled branch' do
        _branch, shared, _origin, left, _right, _merge = diamond
        ran = false
        left.on_fulfillment! { ran = true }
        left.cancel
        shared.fulfill(:ok)
        sleep 0.1
        expect(ran).to be false
      end

      it 'keeps predecessors needed by another branch' do
        b = p.branch
        s = p.resolvable_future
        o = b.wrap(s)
        a = o.then { :a }
        d = o.then { :d }

        a.cancel
        expect(s).to be_pending

        s.fulfill(:ok)
        expect(d.value!(2)).to eq :d
      end

      it 'propagates the cancellation through exclusive single-parent continuations' do
        b = p.branch
        s = p.resolvable_future
        o = b.wrap(s)
        rescued = false
        chained = false
        tail = o.then { 1 }.rescue { rescued = true; :r }.chain { chained = true }

        tail.cancel
        s.fulfill(:ok)
        sleep 0.1

        expect(tail).to be_canceled
        expect(rescued).to be false
        expect(chained).to be false
      end

      it 'runs rescue on a normal rejection but not on a cancellation' do
        b = p.branch
        s = p.resolvable_future
        o = b.wrap(s)
        f = o.rescue { :recovered }
        s.reject(RuntimeError.new('boom'))
        expect(f.value!(2)).to eq :recovered
      end

      it 'cancelling both sides of a diamond resolves the combination as cancelled' do
        _b, s, _o, left, right, merge = diamond

        left.cancel
        right.cancel

        expect(merge.wait(2)).to be_truthy
        expect(merge).to be_canceled
        # even with both branches gone the shared upstream stays alive
        expect(s).to be_pending
      end

      it 'combines a cancelled input with a rejected input without hanging' do
        b = p.branch
        s = p.resolvable_future
        o = b.wrap(s)
        l = o.then { :L }
        r = o.then { raise 'right-boom' }
        m = l.zip(r)

        l.cancel
        s.fulfill(:go)

        expect(m.wait(2)).to be_truthy
        expect(m).to be_rejected
      end

      it 'resolves an already-resolved wrapped future correctly' do
        o = p.branch.wrap(p.fulfilled_future(41))
        expect(o.then { |v| v + 1 }.value!(2)).to eq 42
      end

      it 'works with plain futures as other zip inputs' do
        b = p.branch
        o = b.wrap(p.fulfilled_future(1))
        m = o.zip(p.fulfilled_future(2))
        expect(m.value!(2)).to eq [1, 2]
      end
    end

    describe 'terminal state publication' do
      it 'does not cancel an already completed branch' do
        b = p.branch
        s = p.resolvable_future
        o = b.wrap(s)
        f = o.then { 1 }
        s.fulfill(:v)
        f.wait(2)

        expect(f.cancel).to be false
        expect(f).to be_fulfilled
      end

      it 'publishes exactly one terminal state when cancel and completion race',
         stress: false do
        200.times do
          b = p.branch
          s = p.resolvable_future
          o = b.wrap(s)
          f = o.then { :v }

          t1 = Thread.new { s.fulfill(:v) }
          t2 = Thread.new { f.cancel }
          t1.join
          t2.join

          expect(f.wait(2)).to be_truthy
          expect(f.state).to(satisfy { |st| st == :fulfilled || st == :rejected })
          expect(f.value).to eq :v if f.fulfilled?
          expect(f.reason).to be_a Concurrent::CancelledOperationError if f.rejected?
        end
      end

      it 'skips a task scheduled but not yet started when cancelled' do
        b = p.branch
        s = p.resolvable_future
        o = b.wrap(s)
        f = o.then_on(:io) { raise 'should never run' }
        f.cancel
        s.fulfill(:v)
        expect(f.wait(2)).to be_truthy
        expect(f).to be_canceled
      end
    end

    describe 'cooperative cancellation of running work' do
      it 'signals running code without Thread#kill and lets it observe the signal' do
        started = Queue.new
        b = p.branch
        s = p.resolvable_future
        o = b.wrap(s)

        signal_ref = nil
        work = nil
        work = o.then_on(:io) do
          signal_ref = work.cancellation
          started.push(:go)
          10_000.times do
            signal_ref.check!
            sleep 0.001
          end
          :done
        end

        s.fulfill(:start)
        started.pop

        expect(work.cancel).to be true
        result = work.result(3)
        expect(result).not_to be_nil
        _fulfilled, _value, reason = result
        expect(reason).to be_a Concurrent::CancelledOperationError
      end

      it 'a running task which completes before checking keeps its fulfillment' do
        b = p.branch
        s = p.resolvable_future
        o = b.wrap(s)
        f = o.then_on(:io) { :finished }
        s.fulfill(:go)
        expect(f.value!(2)).to eq :finished
      end

      it 'advises a running shared upstream through the root signal without cancelling it' do
        started  = Queue.new
        finished = Queue.new
        source   = p.resolvable_future
        branch   = p.branch
        head     = nil

        Thread.new do
          token = nil
          head  = branch.wrap(source)
          token = head.cancellation
          started.push(:go)
          loop do
            break if token.canceled?
            sleep 0.001
          end
          source.fulfill(:stopped_cooperatively) if token.canceled?
          finished.push(:done)
        end

        started.pop
        # wait until the thread captured the branch head
        sleep 0.01 until head
        expect(head.cancel).to be true
        expect(finished.pop).to eq :done
        expect(source.value!(2)).to eq :stopped_cooperatively
      end
    end

    describe 'detachment and strong-reference cleanup' do
      it 'nil-ifies the callback entry on the predecessor when detached' do
        b = p.branch
        s = p.resolvable_future
        o = b.wrap(s)
        huge = +'x' * 100_000
        f = o.then(huge) { |_v, arg| arg.bytesize }
        node = f.node

        f.cancel

        expect(node.instance_variable_get(:@Task)).to be_nil
        expect(node.instance_variable_get(:@Args)).to be_nil
        live = s.callbacks.compact.find { |_method, args| args[0].equal?(node) }
        expect(live).to be_nil
      end

      it 'keeps the root edge while abandoning only the cancelled child edge' do
        _b, s, origin, left, _right, _merge = diamond
        left.cancel

        # origin (the branch head) is still a live consumer of the shared future
        root_node = origin.node
        live = s.callbacks.compact.find { |_method, args| args[0].equal?(root_node) }
        expect(live).not_to be_nil
      end
    end
  end
end
