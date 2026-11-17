# frozen_string_literal: true

require_relative 'helper'

separate_testing do
  require_relative '../lib/rack/lock'
  require_relative '../lib/rack/mock_request'
  require_relative '../lib/rack/lint'
end

class Lock
  attr_reader :synchronized

  def initialize
    @synchronized = false
  end

  def lock
    @synchronized = true
  end

  def unlock
    @synchronized = false
  end
end

module LockHelpers
  def lock_app(app, lock = Lock.new)
    app = if lock
      Rack::Lock.new app, lock
    else
      Rack::Lock.new app
    end
    Rack::Lint.new app
  end
end

describe Rack::Lock do
  include LockHelpers

  describe 'Proxy' do
    include LockHelpers

    it 'delegate each' do
      env      = Rack::MockRequest.env_for("/")
      response = Class.new {
        attr_accessor :close_called
        def initialize; @close_called = false; end
        def each; %w{ hi mom }.each { |x| yield x }; end
      }.new

      app = lock_app(lambda { |inner_env| [200, { "content-type" => "text/plain" }, response] })
      response = app.call(env)[2]
      list = []
      response.each { |x| list << x }
      list.must_equal %w{ hi mom }
    end

    it 'delegate to_path' do
      lock = Lock.new
      env  = Rack::MockRequest.env_for("/")

      res = ['Hello World']
      def res.to_path ; "/tmp/hello.txt" ; end

      app = Rack::Lock.new(lambda { |inner_env| [200, { "content-type" => "text/plain" }, res] }, lock)
      body = app.call(env)[2]

      body.must_respond_to :to_path
      body.to_path.must_equal "/tmp/hello.txt"
    end

    it 'not delegate to_path if body does not implement it' do
      env = Rack::MockRequest.env_for("/")

      res = ['Hello World']

      app = lock_app(lambda { |inner_env| [200, { "content-type" => "text/plain" }, res] })
      body = app.call(env)[2]

      body.wont_respond_to :to_path
    end
  end

  it 'call super on close' do
    env      = Rack::MockRequest.env_for("/")
    response = Class.new do
      attr_accessor :close_called
      def initialize; @close_called = false; end
      def close; @close_called = true; end
    end.new

    app = lock_app(lambda { |inner_env| [200, { "content-type" => "text/plain" }, response] })
    app.call(env)
    response.close_called.must_equal false
    response.close
    response.close_called.must_equal true
  end

  it "not unlock until body is closed" do
    lock     = Lock.new
    env      = Rack::MockRequest.env_for("/")
    response = Object.new
    app      = lock_app(lambda { |inner_env| [200, { "content-type" => "text/plain" }, response] }, lock)
    lock.synchronized.must_equal false
    response = app.call(env)[2]
    lock.synchronized.must_equal true
    response.close
    lock.synchronized.must_equal false
  end

  it "return value from app" do
    env  = Rack::MockRequest.env_for("/")
    body = [200, { "content-type" => "text/plain" }, %w{ hi mom }]
    app  = lock_app(lambda { |inner_env| body })

    res = app.call(env)
    res[0].must_equal body[0]
    res[1].must_equal body[1]
    res[2].to_enum.to_a.must_equal ["hi", "mom"]
  end

  it "call synchronize on lock" do
    lock = Lock.new
    env = Rack::MockRequest.env_for("/")
    app = lock_app(lambda { |inner_env| [200, { "content-type" => "text/plain" }, %w{ a b c }] }, lock)
    lock.synchronized.must_equal false
    app.call(env)
    lock.synchronized.must_equal true
  end

  it "unlock if the app raises" do
    lock = Lock.new
    env = Rack::MockRequest.env_for("/")
    app = lock_app(lambda { raise Exception }, lock)
    lambda { app.call(env) }.must_raise Exception
    lock.synchronized.must_equal false
  end

  it "unlock if the app throws" do
    lock = Lock.new
    env = Rack::MockRequest.env_for("/")
    app = lock_app(lambda {|_| throw :bacon }, lock)
    lambda { app.call(env) }.must_throw :bacon
    lock.synchronized.must_equal false
  end

  it 'not unlock if an error is raised before the mutex is locked' do
    lock = Class.new do
      def initialize() @unlocked = false end
      def unlocked?() @unlocked end
      def lock() raise Exception end
      def unlock() @unlocked = true end
    end.new
    env = Rack::MockRequest.env_for("/")
    app = lock_app(proc { [200, { "content-type" => "text/plain" }, []] }, lock)
    lambda { app.call(env) }.must_raise Exception
    lock.unlocked?.must_equal false
  end

  it "unlock if an exception occurs before returning" do
    lock = Lock.new
    env  = Rack::MockRequest.env_for("/")
    app  = lock_app(proc { [].freeze }, lock)
    lambda { app.call(env) }.must_raise Exception
    lock.synchronized.must_equal false
  end

  it "not replace the environment" do
    env  = Rack::MockRequest.env_for("/")
    app  = lock_app(lambda { |inner_env| [200, { "content-type" => "text/plain" }, [inner_env.object_id.to_s]] })

    _, _, body = app.call(env)

    body.to_enum.to_a.must_equal [env.object_id.to_s]
  end
end

describe Rack::Lock::Permit do
  it 'can be released from a different thread than the one that acquired it' do
    permit = Rack::Lock::Permit.new
    lease  = permit.acquire

    permit.locked?.must_equal true
    permit.owned?.must_equal true

    released = Thread.new { lease.release }.value

    released.must_equal true
    permit.locked?.must_equal false
    lease.released?.must_equal true
  end

  it 'release a lease at most once' do
    permit = Rack::Lock::Permit.new
    lease  = permit.acquire

    lease.release.must_equal true
    lease.release.must_equal false
    lease.released?.must_equal true
  end

  it 'not release a later lease through a stale one' do
    permit = Rack::Lock::Permit.new
    first  = permit.acquire
    first.release
    second = permit.acquire

    first.release.must_equal false
    permit.locked?.must_equal true
    permit.owned?.must_equal true

    second.release.must_equal true
    permit.locked?.must_equal false
  end

  it 'wake up a waiter admitted by a release from another thread' do
    permit = Rack::Lock::Permit.new
    lease  = permit.acquire
    admitted = Thread::Queue.new
    waiter = Thread.new do
      permit.acquire.release
      admitted << true
    end

    sleep 0.001 until permit.waiting_count == 1
    permit.owned?.must_equal true

    Thread.new { lease.release }
    waiter.join(5)

    waiter.alive?.must_equal false
    admitted.pop.must_equal true
    permit.locked?.must_equal false
    permit.waiting_count.must_equal 0
  end

  it 'admit waiters in the order they declared interest' do
    permit = Rack::Lock::Permit.new
    lease  = permit.acquire
    order  = Thread::Queue.new

    # Start each waiter only after the previous one is queued, so the queue
    # order is deterministic.  A new arrival may not overtake a thread that
    # started waiting earlier.
    threads = 3.times.map do |index|
      thread = Thread.new do
        permit.acquire.release
        order << index
      end

      deadline = Time.now + 5
      until permit.waiting_count == index + 1
        flunk "waiters did not queue: #{permit.waiting_count}" if Time.now > deadline
        sleep 0.001
      end

      thread
    end

    lease.release

    admitted = 3.times.map { order.pop }
    admitted.must_equal [0, 1, 2]

    threads.each(&:join)
    permit.locked?.must_equal false
  end

  it 'support the same-thread mutex interface' do
    permit = Rack::Lock::Permit.new

    permit.try_lock.must_equal true
    permit.owned?.must_equal true
    permit.try_lock.must_equal false
    lambda { permit.synchronize { } }.must_raise ThreadError
    permit.unlock.must_equal permit
    permit.locked?.must_equal false

    permit.synchronize { permit.locked?.must_equal true }
    permit.locked?.must_equal false
  end

  it 'raise ThreadError when unlocked by a thread that does not own it' do
    permit = Rack::Lock::Permit.new
    permit.lock

    thief = Thread.new do
      Thread.current.report_on_exception = false
      permit.unlock
    end

    lambda { thief.value }.must_raise ThreadError
    permit.unlock
    permit.locked?.must_equal false
  end

  it 'not leave an interrupted waiter at the front of the queue' do
    permit = Rack::Lock::Permit.new
    lease  = permit.acquire

    interrupted = Thread.new { permit.acquire }
    sleep 0.001 until permit.waiting_count == 1
    interrupted.kill
    interrupted.join(5)

    served = Thread.new { permit.acquire.release }
    sleep 0.001 until permit.waiting_count == 1
    lease.release
    served.join(5)

    served.alive?.must_equal false
    permit.locked?.must_equal false
  end
end

describe 'Rack::Lock with a response lifetime scope' do
  def lock_with(app, permit = Rack::Lock::Permit.new, scope: :response)
    [Rack::Lock.new(app, permit, scope: scope), permit]
  end

  it 'hold the permit until an enumerable body has been iterated' do
    app, permit = lock_with(proc { [200, { "content-type" => "text/plain" }, %w[a b]] })

    response = app.call(Rack::MockRequest.env_for('/'))

    permit.locked?.must_equal true
    response[2].to_enum.to_a.must_equal %w[a b]
    permit.locked?.must_equal false
  end

  it 'hold the permit until a streaming body returns from call' do
    streamed = []
    body = Class.new do
      define_method(:call) do |stream|
        stream << 'chunk'
        streamed << :called
      end
    end.new

    app, permit = lock_with(proc { [200, { "content-type" => "text/plain" }, body] })

    response = app.call(Rack::MockRequest.env_for('/'))
    permit.locked?.must_equal true
    response[2].respond_to?(:call).must_equal true
    response[2].respond_to?(:each).must_equal false
    response[2].call(StringIO.new)

    streamed.must_equal [:called]
    permit.locked?.must_equal false
  end

  it 'release when the body is closed before iteration starts' do
    app, permit = lock_with(proc { [200, { "content-type" => "text/plain" }, %w[a b]] })

    response = app.call(Rack::MockRequest.env_for('/'))
    permit.locked?.must_equal true

    response[2].close
    permit.locked?.must_equal false
  end

  it 'release when an enumerable body raises during iteration' do
    body = Class.new do
      def each
        yield 'one'
        raise 'stream failed'
      end
    end.new

    app, permit = lock_with(proc { [200, { "content-type" => "text/plain" }, body] })

    response = app.call(Rack::MockRequest.env_for('/'))
    lambda { response[2].each { |chunk| } }.must_raise RuntimeError

    permit.locked?.must_equal false
  end

  it 'release when a streaming body raises during call' do
    body = Class.new do
      def call(_stream)
        raise 'stream failed'
      end
    end.new

    app, permit = lock_with(proc { [200, { "content-type" => "text/plain" }, body] })

    response = app.call(Rack::MockRequest.env_for('/'))
    lambda { response[2].call(StringIO.new) }.must_raise RuntimeError

    permit.locked?.must_equal false
  end

  it 'not release a later request when a consumed body is closed again' do
    app, permit = lock_with(proc { [200, { "content-type" => "text/plain" }, %w[a b]] })

    first = app.call(Rack::MockRequest.env_for('/'))
    first[2].close
    permit.locked?.must_equal false

    second = app.call(Rack::MockRequest.env_for('/'))
    permit.locked?.must_equal true

    first[2].close
    permit.locked?.must_equal true

    second[2].close
    permit.locked?.must_equal false
  end

  it 'release exactly once when the app raises' do
    permit = Rack::Lock::Permit.new
    app = Rack::Lock.new(proc { raise 'app failed' }, permit, scope: :response)

    lambda { app.call(Rack::MockRequest.env_for('/')) }.must_raise RuntimeError
    permit.locked?.must_equal false

    # A later request must be admitted, i.e. the failed one did not leave a
    # lease behind.
    app = Rack::Lock.new(proc { [200, { "content-type" => "text/plain" }, ['ok']] }, permit, scope: :response)
    app.call(Rack::MockRequest.env_for('/'))[2].close
    permit.locked?.must_equal false
  end

  it 'release exactly once when the app throws' do
    permit = Rack::Lock::Permit.new
    app = Rack::Lock.new(proc { throw :bacon }, permit, scope: :response)

    lambda { app.call(Rack::MockRequest.env_for('/')) }.must_throw :bacon
    permit.locked?.must_equal false
  end

  it 'release when the app returns a malformed response' do
    permit = Rack::Lock::Permit.new
    app = Rack::Lock.new(proc { [].freeze }, permit, scope: :response)

    lambda { app.call(Rack::MockRequest.env_for('/')) }.must_raise RuntimeError
    permit.locked?.must_equal false
  end

  it 'block a later request until the earlier response body is consumed' do
    permit = Rack::Lock::Permit.new
    calls  = Thread::Queue.new
    app = Rack::Lock.new(proc { |env|
      calls << env['PATH_INFO']
      [200, { "content-type" => "text/plain" }, [env['PATH_INFO']]]
    }, permit, scope: :response)

    first = app.call(Rack::MockRequest.env_for('/first'))
    calls.pop.must_equal '/first'

    second_thread = Thread.new { app.call(Rack::MockRequest.env_for('/second')) }
    sleep 0.001 until permit.waiting_count == 1

    # The second request has not been passed to the application yet.
    calls.pop(true) rescue nil
    second_thread.alive?.must_equal true

    first[2].close
    second = second_thread.value

    calls.pop.must_equal '/second'
    permit.locked?.must_equal true
    second[2].close
    permit.locked?.must_equal false
  end

  it 'generate a response in one thread and consume it in another' do
    permit = Rack::Lock::Permit.new
    app = Rack::Lock.new(proc {
      [200, { "content-type" => "text/plain" }, %w[hello world]]
    }, permit, scope: :response)

    response = Thread.new { app.call(Rack::MockRequest.env_for('/')) }.value
    permit.locked?.must_equal true

    chunks = Thread.new { response[2].to_enum.to_a }.value
    chunks.must_equal %w[hello world]
    permit.locked?.must_equal false

    # Another request may now proceed from the main thread.
    app.call(Rack::MockRequest.env_for('/'))[2].close
    permit.locked?.must_equal false
  end

  it 'close a streaming body consumed from another thread without a ThreadError' do
    permit = Rack::Lock::Permit.new
    body = Class.new do
      def call(stream)
        stream << 'chunk'
      end
    end.new
    app = Rack::Lock.new(proc { [200, { "content-type" => "text/plain" }, body] }, permit, scope: :response)

    response = app.call(Rack::MockRequest.env_for('/'))
    stream = StringIO.new

    Thread.new { response[2].call(stream) }.join(5)

    permit.locked?.must_equal false
    stream.string.must_equal 'chunk'
  end

  it 'reject a Mutex because it cannot be released from another thread' do
    lambda {
      Rack::Lock.new(proc { [200, {}, []] }, Mutex.new, scope: :response)
    }.must_raise ArgumentError
  end

  it 'reject an unknown scope' do
    lambda {
      Rack::Lock.new(proc { [200, {}, []] }, Rack::Lock::Permit.new, scope: :unknown)
    }.must_raise ArgumentError
  end

  it 'treat a body that responds to both each and call as enumerable' do
    calls = []
    body = Class.new do
      define_method(:each) { |&block| block.call 'each-chunk' }
      define_method(:call) { |_stream| calls << :call }
    end.new

    app, permit = lock_with(proc { [200, { "content-type" => "text/plain" }, body] })
    response = app.call(Rack::MockRequest.env_for('/'))

    response[2].respond_to?(:each).must_equal true
    response[2].respond_to?(:call).must_equal true

    chunks = []
    response[2].each { |chunk| chunks << chunk }

    chunks.must_equal ['each-chunk']
    calls.must_equal []
    permit.locked?.must_equal false
  end

  it 'release when an enumerable body is converted with to_ary' do
    body = Class.new do
      def to_ary; %w[a b]; end
      def each(&block); to_ary.each(&block); end
    end.new

    app, permit = lock_with(proc { [200, { "content-type" => "text/plain" }, body] })
    response = app.call(Rack::MockRequest.env_for('/'))

    response[2].to_ary.must_equal %w[a b]
    permit.locked?.must_equal false
  end

  it 'release the default scope when the body is closed from another thread' do
    app, permit = lock_with(proc { [200, { "content-type" => "text/plain" }, %w[a b]] }, scope: :close)

    response = app.call(Rack::MockRequest.env_for('/'))
    permit.locked?.must_equal true

    # A Mutex would raise ThreadError here, so the default scope must not use
    # one.
    Thread.new { response[2].close }.join(5)

    permit.locked?.must_equal false
  end
end

describe 'Rack::Lock with a call scope' do
  it 'release the lock as soon as the application returns' do
    permit = Rack::Lock::Permit.new
    app = Rack::Lock.new(proc { [200, { "content-type" => "text/plain" }, %w[a b]] }, permit, scope: :call)

    response = app.call(Rack::MockRequest.env_for('/'))

    permit.locked?.must_equal false
    response[2].to_enum.to_a.must_equal %w[a b]
    permit.locked?.must_equal false
  end

  it 'release the lock when the application raises' do
    permit = Rack::Lock::Permit.new
    app = Rack::Lock.new(proc { raise 'app failed' }, permit, scope: :call)

    lambda { app.call(Rack::MockRequest.env_for('/')) }.must_raise RuntimeError
    permit.locked?.must_equal false
  end
end

describe 'Rack::Lock with Rack::Lint and a response lifetime scope' do
  def lint_app(scope)
    Rack::Lint.new(Rack::Lock.new(
      proc { [200, { "content-type" => "text/plain" }, %w[hi mom]] },
      Rack::Lock::Permit.new,
      scope: scope,
    ))
  end

  it 'pass an enumerable body through Rack::Lint' do
    response = lint_app(:response).call(Rack::MockRequest.env_for('/'))
    chunks = []
    response[2].each { |chunk| chunks << chunk }
    response[2].close

    chunks.must_equal %w[hi mom]
  end

  it 'pass an enumerable body through Rack::Lint with the default scope' do
    response = lint_app(:close).call(Rack::MockRequest.env_for('/'))
    response[2].to_enum.to_a.must_equal %w[hi mom]
    response[2].close
  end

  it 'pass a body discarded without iteration through Rack::Lint' do
    response = lint_app(:response).call(Rack::MockRequest.env_for('/'))
    response[2].close
  end
end

describe 'Rack::Lock serialization under concurrency' do
  it 'never run two response bodies at the same time' do
    permit   = Rack::Lock::Permit.new
    overlap  = 0
    peak     = 0
    mutex    = Mutex.new

    app = Rack::Lock.new(proc {
      body = Class.new do
        define_method(:each) do |&block|
          mutex.synchronize { overlap += 1; peak = [peak, overlap].max }
          sleep 0.001
          block.call 'chunk'
        ensure
          mutex.synchronize { overlap -= 1 }
        end
      end.new
      [200, { "content-type" => "text/plain" }, body]
    }, permit, scope: :response)

    threads = 8.times.map do
      Thread.new do
        5.times do
          body = app.call(Rack::MockRequest.env_for('/'))[2]
          body.to_enum.to_a
          body.close
        end
      end
    end

    threads.each(&:join)

    peak.must_equal 1
    permit.locked?.must_equal false
    permit.waiting_count.must_equal 0
  end

  it 'admit every waiter exactly once even when bodies are closed early' do
    permit = Rack::Lock::Permit.new
    seen   = Thread::Queue.new

    app = Rack::Lock.new(proc { [200, { "content-type" => "text/plain" }, ['x']] }, permit, scope: :response)

    threads = 6.times.map do |index|
      Thread.new do
        body = app.call(Rack::MockRequest.env_for('/'))[2]
        seen << index
        body.close
      end
    end

    threads.each { |thread| thread.join(10) }
    threads.each { |thread| thread.alive?.must_equal false }

    seen.size.must_equal 6
    permit.locked?.must_equal false
    permit.waiting_count.must_equal 0
  end
end
