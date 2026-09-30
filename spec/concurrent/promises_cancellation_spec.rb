require 'concurrent/promises'

RSpec.describe 'Concurrent::Promises future cancellation primitive', edge: true do
  def resolvable_future
    Concurrent::Promises.resolvable_future
  end

  specify 'cancelled is a distinct terminal state' do
    f = resolvable_future
    expect(f.cancel).to eq true
    expect(f.state).to eq :cancelled
    expect(f).to be_cancelled
    expect(f).to be_resolved
    expect(f).not_to be_pending
    expect(f).not_to be_fulfilled
    expect(f).not_to be_rejected
    expect(f.reason).to be_a Concurrent::CancelledFutureError
    expect(f.result).to eq [false, nil, f.reason]
    expect { f.value! }.to raise_error(Concurrent::CancelledFutureError)
  end

  specify 'only one terminal state is published in the cancel/complete race' do
    outcomes = Hash.new(0)
    400.times do
      f = resolvable_future
      barrier = Concurrent::Event.new
      t1 = Thread.new do
        barrier.wait
        sleep(rand * 0.0005)
        f.cancel
      end
      t2 = Thread.new do
        barrier.wait
        sleep(rand * 0.0005)
        f.fulfill(:v, false)
      end
      barrier.set
      t1.join
      t2.join
      expect(f.fulfilled? ^ f.cancelled?).to be_truthy
      expect(f.rejected?).to eq false
      outcomes[f.state] += 1
    end
    # both winners should be observed over enough iterations
    expect(outcomes.keys.sort).to eq [:cancelled, :fulfilled]
  end

  specify 'cancel loses silently against a prior completion' do
    f = Concurrent::Promises.fulfilled_future(1)
    expect(f.cancel).to eq false
    expect(f).to be_fulfilled
    expect(f.value!).to eq 1

    r = Concurrent::Promises.rejected_future(RuntimeError.new('boom'))
    expect(r.cancel).to eq false
    expect(r).to be_rejected
  end

  specify 'double cancel is a no-op' do
    f = resolvable_future
    expect(f.cancel).to eq true
    expect(f.cancel).to eq false
    expect(f.state).to eq :cancelled
  end

  specify 'then/rescue/chain continuations detach from a cancelled predecessor' do
    root = resolvable_future
    ran  = Concurrent::AtomicFixnum.new(0)
    a    = root.then { ran.increment; :a }
    b    = a.then { ran.increment; :b }
    c    = a.rescue { ran.increment; :c }
    d    = a.chain { ran.increment; :d }

    a.cancel
    root.fulfill(1)

    [b, c, d].each { |f| f.wait(2) }
    expect(ran.value).to eq 0
    expect(a.state).to eq :cancelled
    expect(b.state).to eq :cancelled
    expect(c.state).to eq :cancelled
    expect(d.state).to eq :cancelled
  end

  specify 'continuations registered after cancellation also detach' do
    root = resolvable_future
    a    = root.then { 1 }
    a.cancel
    root.fulfill(1)
    expect(a.wait(2)).to be_truthy

    ran = Concurrent::AtomicFixnum.new(0)
    late = a.then { ran.increment }
    expect(late.wait(2)).to be_truthy
    expect(late).to be_cancelled
    expect(ran.value).to eq 0
  end

  specify 'user callbacks skip but on_cancellation fires' do
    root = resolvable_future
    a    = root.then { 1 }
    user = Concurrent::AtomicFixnum.new(0)
    seen_reason = nil

    a.on_fulfillment!  { user.increment }
    a.on_rejection!    { user.increment }
    a.on_resolution!   { user.increment }
    a.on_cancellation! { |reason| seen_reason = reason }

    a.cancel
    root.fulfill(1)
    a.wait(2)

    expect(user.value).to eq 0
    expect(seen_reason).to be_a Concurrent::CancelledFutureError
  end

  specify 'zip join never waits forever for a cancelled input' do
    root = resolvable_future
    left  = root.then { :l }
    right = root.then { :r }
    join  = Concurrent::Promises.zip_futures(left, right)

    left.cancel
    root.fulfill(1)
    right.wait(2)

    expect(join.wait(2)).to be_truthy
    expect(join).to be_rejected
    _fulfilled, _values, reasons = join.result
    expect(reasons.first).to be_a Concurrent::CancelledFutureError
  end

  specify 'a join of only cancelled inputs cancels itself' do
    root = resolvable_future
    a = root.then { 1 }
    b = root.then { 2 }
    join = Concurrent::Promises.zip_futures(a, b)
    a.cancel
    b.cancel
    root.fulfill(1)
    expect(join.wait(2)).to be_truthy
    expect(join).to be_cancelled
  end

  specify 'tangled event/future always reaches a terminal state' do
    root = resolvable_future
    a = root.then { 1 }
    event = a.to_event
    a.cancel
    root.fulfill(1)
    expect(event.wait(2)).to be_truthy
    expect(event).to be_resolved

    root2 = resolvable_future
    b = root2.then { 2 }
    tangled = Concurrent::Promises.resolvable_future
    b.chain_resolvable(tangled)
    b.cancel
    root2.fulfill(1)
    expect(tangled.wait(2)).to be_truthy
    expect(tangled).to be_cancelled
  end

  specify 'already running work is never force-killed and may win the race' do
    root = resolvable_future
    started = Concurrent::Promises.resolvable_event
    task = root.then do
      started.resolve
      sleep 0.05
      :done
    end
    root.fulfill(1)
    started.wait(2)
    sleep 0.01
    # cancellation cannot stop an already scheduled task
    expect(root.cancel).to eq false
    task.wait(2)
    expect(task).to be_fulfilled
    expect(task.value!).to eq :done
  end

  specify 'skipped user callbacks are pruned from the callback stack' do
    root = resolvable_future
    a = root.then { 1 }
    a.on_resolution! { :unused }
    a.then { :also_unused }
    a.cancel
    root.fulfill(1)
    a.wait(2)
    methods = a.callbacks.map(&:first)
    expect(methods).not_to include(:callback_on_resolution)
  end
end

