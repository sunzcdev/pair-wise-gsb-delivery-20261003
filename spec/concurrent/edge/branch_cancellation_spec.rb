require 'concurrent/edge/branch_cancellation'
require 'weakref'
require 'rbconfig'

RSpec.describe 'Concurrent::Promises::Branch', edge: true do
  # Builds:
  #
  #   root
  #   ├── left branch  ── l1 ── l2
  #   └── right branch ── r1 ── r2
  #             └──────── join (zip)
  def diamond
    root = Concurrent::Promises.resolvable_future

    left_calls  = Concurrent::AtomicFixnum.new(0)
    right_calls = Concurrent::AtomicFixnum.new(0)

    left  = root.branch
    right = root.branch

    l1 = left.then  { |v| left_calls.increment; "L1:#{v}" }
    l2 = l1.then    { |v| left_calls.increment; "L2:#{v}" }

    r1 = right.then { |v| right_calls.increment; "R1:#{v}" }
    r2 = r1.then    { |v| right_calls.increment; "R2:#{v}" }

    join = Concurrent::Promises.zip_futures(l2.future, r2.future)

    [root, left, right, l1, l2, r1, r2, join, left_calls, right_calls]
  end

  specify 'cancelling the left branch leaves the right branch and the shared root intact' do
    root, left, _right, _l1, l2, _r1, r2, join, left_calls, right_calls = diamond

    expect(left.cancel).to eq true

    # the shared upstream is never pulled down with the branch
    expect(root).to be_pending
    root.fulfill(7)

    # right chain completes normally
    expect(r2.future.wait(2)).to be_truthy
    expect(r2.future).to be_fulfilled
    expect(r2.future.value!).to eq 'R2:R1:7'
    expect(right_calls.value).to eq 2

    # left chain never executes and settles as cancelled
    expect(l2.future.wait(2)).to be_truthy
    expect(l2.future).to be_cancelled
    expect(left_calls.value).to eq 0

    # root completed normally
    expect(root).to be_fulfilled
    expect(root.value!).to eq 7

    # the combining node never waits forever on the cancelled input
    expect(join.wait(2)).to be_truthy
    expect(join).to be_rejected
    fulfilled, values, reasons = join.result
    expect(fulfilled).to eq false
    expect(values).to eq [nil, 'R2:R1:7']
    expect(reasons.first).to be_a Concurrent::CancelledFutureError
    expect(reasons.last).to eq nil
  end

  specify 'the branch reports cancellation and idempotency' do
    root, left, _right, _l1, _l2, _r1, _r2, _join, = diamond
    expect(left.cancelled?).to eq false
    expect(left.cancel).to eq true
    expect(left.cancelled?).to eq true
    expect(left.cancel).to eq false
    root.fulfill(1)
  end

  specify 'cancelling after the branch completed is a no-op' do
    root = Concurrent::Promises.resolvable_future
    b = root.branch
    t = b.then { |v| v + 1 }
    root.fulfill(1)
    expect(t.future.wait(2)).to be_truthy
    expect(b.cancel).to eq false
    expect(t.future.value!).to eq 2
  end

  specify 'the predecessor the branch forked from is never cancelled' do
    root = Concurrent::Promises.resolvable_future
    b = root.branch
    b.cancel
    expect(root).to be_pending
    root.fulfill(1)
    expect(root).to be_fulfilled
  end

  specify 'continuation tasks attached after cancellation detach immediately' do
    root = Concurrent::Promises.resolvable_future
    b = root.branch
    t = b.then { 1 }.future
    b.cancel
    root.fulfill(1)
    t.wait(2)

    ran = Concurrent::AtomicFixnum.new(0)
    late = b.then { ran.increment }.future
    expect(late.wait(2)).to be_truthy
    expect(late).to be_cancelled
    expect(ran.value).to eq 0
  end

  specify 'already running code is stopped only cooperatively, without Thread.kill' do
    root      = Concurrent::Promises.resolvable_future
    started   = Concurrent::Event.new
    finished  = Concurrent::Event.new
    observed  = Concurrent::AtomicBoolean.new(false)

    b = root.branch
    running = b.then do |_v, token|
      started.set
      # cooperative polling instead of being force-terminated
      200.times do
        break if token.canceled?
        sleep 0.005
      end
      observed.make_true if token.canceled?
      finished.set
      :done
    end.future

    root.fulfill(1)
    expect(started.wait(2)).to be_truthy
    b.cancel
    expect(finished.wait(2)).to be_truthy
    expect(observed.true?).to eq true
    running.wait(2)
    expect(running).to be_cancelled
  end

  specify 'running code that ignores the token keeps running and cancelling other branches is unaffected' do
    root     = Concurrent::Promises.resolvable_future
    started  = Concurrent::Event.new
    finishing = Concurrent::Event.new

    left  = root.branch
    right = root.branch

    stubborn = left.then do
      started.set
      finishing.wait(2)
      :ignored_cancellation
    end.future
    r = right.then { :ok }.future

    root.fulfill(1)
    started.wait(2)
    left.cancel
    finishing.set
    stubborn.wait(2)
    # the stubborn task completed; branch is settled (fulfilled - task won,
    # or cancelled - cancellation won), never left pending
    expect(stubborn.pending?).to eq false
    expect(r.wait(2)).to be_truthy
    expect(r.value!).to eq :ok
  end

  specify 'cancellation and completion publish exactly one terminal state' do
    100.times do
      root = Concurrent::Promises.resolvable_future
      b = root.branch
      t = b.then { 1 }.future

      go = Concurrent::Event.new
      threads = [
        Thread.new { go.wait; sleep(rand * 0.0005); b.cancel },
        Thread.new { go.wait; sleep(rand * 0.0005); root.fulfill(1, false) }
      ]
      go.set
      threads.each(&:join)

      t.wait(2)
      expect(t.fulfilled? ^ t.cancelled?).to be_truthy
      expect(t.rejected?).to eq false
    end
  end

  specify 'cancelling right leaves left fully working (symmetric ownership)' do
    root = Concurrent::Promises.resolvable_future
    lb   = root.branch
    rb   = root.branch
    lt   = lb.then { |v| v * 10 }
    rt   = rb.then { :right }

    rb.cancel
    root.fulfill(5)

    expect(lt.future.wait(2)).to be_truthy
    expect(lt.future.value!).to eq 50
    expect(rt.future.wait(2)).to be_truthy
    expect(rt.future).to be_cancelled
    expect(root).to be_fulfilled
  end

  specify 'left callbacks and the strong references they hold are cleaned up' do
    root = Concurrent::Promises.resolvable_future
    b = root.branch
    payload = 'x' * 4096
    t = b.then(payload) { |_v, p| p }.future

    # sanity: before cancellation the promise strongly holds task arguments
    promise = t.promise
    expect(promise.instance_variable_get(:@Args).first).to eq payload

    b.cancel
    root.fulfill(1)
    expect(t.wait(2)).to be_truthy
    expect(t).to be_cancelled

    # the detached continuation releases task and args (the executor is a
    # cheap shared object and remains available for inspection)
    expect(promise.instance_variable_get(:@Task)).to eq nil
    expect(promise.instance_variable_get(:@Args)).to eq nil

    # the future's callback stack only retains wiring notifications
    t.callbacks.each do |method, _args|
      expect(method).to eq :callback_notify_blocked
    end
  end

  specify 'a detached branch allows captured payloads to be garbage collected' do
    code = <<~RUBY
      $LOAD_PATH.unshift *#{[File.expand_path('lib/concurrent-ruby'),
                              File.expand_path('lib/concurrent-ruby-edge')].inspect}
      require 'concurrent'
      require 'concurrent/edge/branch_cancellation'
      require 'weakref'

      def run
        root = Concurrent::Promises.resolvable_future
        b = root.branch
        payload = 'x' * 8192
        t = b.then(payload) { |_v, p| p }.future
        weak = WeakRef.new(payload)
        payload = nil
        b.cancel
        root.fulfill(1)
        t.wait(2)
        root = b = t = nil
        weak
      end

      weak = run
      collected = 20.times.any? { GC.start; sleep 0.02; !weak.weakref_alive? }
      exit(collected ? 0 : 1)
    RUBY

    system(RbConfig.ruby, '-e', code)
    expect($?.exitstatus).to eq(0)
  end

  specify 'on_cancellation! observes the branch cancellation reason' do
    root = Concurrent::Promises.resolvable_future
    b = root.branch
    t = b.then { 1 }.future
    seen = nil
    t.on_cancellation! { |reason| seen = reason }
    b.cancel(RuntimeError.new('custom'))
    root.fulfill(1)
    expect(t.wait(2)).to be_truthy
    expect(seen).to be_a RuntimeError
    expect(seen.message).to eq 'custom'
  end

  specify 'the branch cancellation token is the cooperative signal' do
    root = Concurrent::Promises.resolvable_future
    b = root.branch
    expect(b.cancellation).to be_a Concurrent::Cancellation
    expect(b.cancellation.canceled?).to eq false
    b.cancel
    expect(b.cancellation.canceled?).to eq true
    root.fulfill(1)
  end

  specify 'a three-way fan-out cancels one without disturbing the others' do
    root = Concurrent::Promises.resolvable_future
    branches = 3.times.map { root.branch }
    tails = branches.map { |br| br.then { |v| v } }
    join  = Concurrent::Promises.zip_futures(*tails.map(&:future))

    branches.first.cancel
    root.fulfill(9)

    tails.map { |t| t.future.wait(2) }
    expect(tails.first.future).to be_cancelled
    tails[1..].each { |t| expect(t.future.value!).to eq 9 }
    expect(join.wait(2)).to be_truthy
    expect(join).to be_rejected
  end

  specify 'plain combinators downstream of the owned tip still detach via core semantics' do
    root = Concurrent::Promises.resolvable_future
    b = root.branch
    owned    = b.then { 1 }
    detached = owned.future.then { |v| v + 1 } # not tracked by the branch
    b.cancel
    root.fulfill(1)
    detached.wait(2)
    # the core cascade, not branch bookkeeping, settles the unowned descendant
    expect(detached).to be_cancelled
  end
end
