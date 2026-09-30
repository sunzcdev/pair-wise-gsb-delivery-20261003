require_relative "../spec_helper"

describe "autoload_hash_branches plugin concurrency" do
  FIXTURE_DIR = File.expand_path("../../autoload_hash_branches_concurrent", __FILE__)

  after do
    $roda_app = nil
    $roda_concurrent_entered = nil
    $roda_concurrent_started = nil
    $roda_concurrent_continue = nil
    $roda_concurrent_loaded = nil
    $roda_concurrent_other_finished = nil
    $roda_concurrent_loaded_count = nil
    $roda_concurrent_allow_failure_success = nil

    Dir[File.join(FIXTURE_DIR, "**/*.rb")].each do |f|
      $LOADED_FEATURES.delete(File.expand_path(f))
      $LOADED_FEATURES.delete(f)
    end
  end

  # A one-shot barrier: wait_for blocks until release is called, and release
  # lets every past and future waiter proceed.  Gives a failure after a timeout
  # so a broken test reports instead of hanging the suite.
  class Barrier
    def initialize
      @mutex = Mutex.new
      @condition = ConditionVariable.new
      @open = false
    end

    def wait_for
      @mutex.synchronize do
        deadline = Time.now + 10
        until @open
          remaining = deadline - Time.now
          raise "barrier timed out" if remaining <= 0
          @condition.wait(@mutex, remaining)
        end
      end
    end

    def release
      @mutex.synchronize do
        @open = true
        @condition.broadcast
      end
    end
  end

  # Arrival counter: each arrival is recorded, so the test can deterministically
  # wait until a known number of threads have reached a point.  Arrivals are not
  # blocked; blocking is the Barrier's job.
  class Rendezvous
    def initialize
      @mutex = Mutex.new
      @condition = ConditionVariable.new
      @arrivals = 0
    end

    def arrive
      @mutex.synchronize do
        @arrivals += 1
        @condition.broadcast
      end
    end

    def wait_for_arrivals(count)
      @mutex.synchronize do
        deadline = Time.now + 10
        until @arrivals >= count
          remaining = deadline - Time.now
          raise "rendezvous timed out waiting for #{count} arrivals, got #{@arrivals}" if remaining <= 0
          @condition.wait(@mutex, remaining)
        end
      end
    end
  end

  # Gate that never blocks, used when a branch is re-required on retry and no
  # thread needs to wait for it to enter the file.
  class OpenGate
    def arrive; end
  end

  def concurrent_app
    app(:bare) do
      plugin :autoload_hash_branches

      route do |r|
        r.hash_branches
        '-'
      end
    end

    $roda_app = @app
    $roda_concurrent_loaded_count = 0
    @app
  end

  def register_branch(name)
    @app.autoload_hash_branch(name, File.join(FIXTURE_DIR, "#{name}.rb"))
  end

  # Run a request to the path in a new thread, returning a handle that exposes
  # either the response body or the raised exception after join.
  def request_thread(path)
    Thread.new do
      Thread.current.report_on_exception = false
      begin
        status, _headers, body = @app.call(
          "REQUEST_METHOD" => "GET",
          "PATH_INFO" => path,
          "SCRIPT_NAME" => "",
          "rack.input" => StringIO.new
        )
        result = +""
        body.each { |chunk| result << chunk }
        body.close if body.respond_to?(:close)
        [status, result]
      rescue Exception => e
        e
      end
    end
  end

  it "loads a branch once when concurrent requests arrive, with waiters using the loaded result" do
    concurrent_app
    register_branch('slow')

    $roda_concurrent_entered = Rendezvous.new
    $roda_concurrent_loaded = Barrier.new

    leader = request_thread('/slow')

    # Deterministically wait until the loader is inside the branch file.
    $roda_concurrent_entered.wait_for_arrivals(1)

    waiters = Array.new(3) { request_thread('/slow') }

    # Wait until all waiters have parked on the in-progress attempt, proving
    # they are waiting rather than starting a second load.
    state = @app.autoload_hash_branch_states[File.expand_path('slow.rb', FIXTURE_DIR)]
    deadline = Time.now + 10
    until state.attempt && waiters.all? { |thread| thread.status == 'sleep' }
      raise "waiters did not block on the in-progress load" if Time.now > deadline
      Thread.pass
    end

    $roda_concurrent_loaded_count.must_equal 1

    # Release the loader and collect all results.
    $roda_concurrent_loaded.release
    results = ([leader] + waiters).map(&:value)

    results.each do |result|
      result.must_be_kind_of(Array)
      result[0].must_equal 200
      result[1].must_equal 'slow-loaded'
    end

    $roda_concurrent_loaded_count.must_equal 1
  end

  it "does not block other branches while a branch is loading" do
    concurrent_app
    register_branch('slow')
    register_branch('other')

    $roda_concurrent_entered = Rendezvous.new
    $roda_concurrent_loaded = Barrier.new
    $roda_concurrent_other_finished = Queue.new

    leader = request_thread('/slow')

    $roda_concurrent_entered.wait_for_arrivals(1)

    # While slow is still blocked inside require, load and complete other.
    other = request_thread('/other')
    $roda_concurrent_other_finished.pop

    other.value.must_equal [200, 'other-loaded']

    $roda_concurrent_loaded.release
    leader.value.must_equal [200, 'slow-loaded']
  end

  it "fails all waiting requests when loading raises, rolls back routes, and allows a retry" do
    concurrent_app
    register_branch('failing')

    $roda_concurrent_started = Rendezvous.new
    $roda_concurrent_continue = Barrier.new
    $roda_concurrent_allow_failure_success = false

    leader = request_thread('/failing')

    # Wait until the loader has entered the file but not registered yet.
    $roda_concurrent_started.wait_for_arrivals(1)

    waiters = Array.new(3) { request_thread('/failing') }

    state = @app.autoload_hash_branch_states[File.expand_path('failing.rb', FIXTURE_DIR)]
    deadline = Time.now + 10
    until state.attempt && waiters.all? { |thread| thread.status == 'sleep' }
      raise "waiters did not block on the in-progress load" if Time.now > deadline
      Thread.pass
    end

    # Release the loader so it raises, failing the whole batch.
    $roda_concurrent_continue.release
    failures = ([leader] + waiters).map(&:value)

    failures.each do |result|
      result.must_be_kind_of(RuntimeError)
      result.message.must_equal 'boom'
    end

    # Every request in the batch failed, but the file was loaded only once.
    $roda_concurrent_loaded_count.must_equal 1

    # The half-registered route must have been rolled back: the registered
    # method is the loader (defined in the plugin), not a block from the
    # failed file.  Issuing a request here would itself start a retry, so
    # inspect the routing table directly.
    meth = @app.hash_branches['']['/failing']
    @app.instance_method(meth).source_location.first.must_include(
      File.join('roda', 'plugins', 'autoload_hash_branches.rb')
    )

    # A later batch retries the load and succeeds.
    $roda_concurrent_allow_failure_success = true
    $roda_concurrent_started = OpenGate.new
    retry_continue = Barrier.new
    retry_continue.release
    $roda_concurrent_continue = retry_continue

    body('/failing').must_equal 'failing-recovered'
    $roda_concurrent_loaded_count.must_equal 2

    body('/failing').must_equal 'failing-recovered'
    $roda_concurrent_loaded_count.must_equal 2
  end

  it "raises when the branch file recursively requests the same branch while loading" do
    concurrent_app
    register_branch('recursive')

    error = request_thread('/recursive').value
    error.must_be_kind_of(Roda::RodaError)
    error.message.must_match(/recursive autoloading/)

    # The failed attempt did not leave a route registered: the segment still
    # points at the loader method, not a block from the failed file.
    meth = @app.hash_branches['']['/recursive']
    @app.instance_method(meth).source_location.first.must_include(
      File.join('roda', 'plugins', 'autoload_hash_branches.rb')
    )
    $roda_concurrent_loaded_count.must_equal 1

    # Fix the recursive condition and retry: the file loads and registers.
    $roda_app.autoload_hash_branch('recursive', File.join(FIXTURE_DIR, 'recursive_ok.rb'))

    recursive_ok = File.join(FIXTURE_DIR, 'recursive_ok.rb')
    File.write(recursive_ok, "$roda_app.hash_branch('recursive') { |r| 'recursive-ok' }\n")

    body('/recursive').must_equal 'recursive-ok'
  ensure
    recursive_ok = File.join(FIXTURE_DIR, 'recursive_ok.rb')
    File.delete(recursive_ok) if File.file?(recursive_ok)
    $LOADED_FEATURES.delete(recursive_ok)
  end

  it "rolls back partially registered routes when an eager load on freeze fails" do
    concurrent_app
    register_branch('failing')

    $roda_concurrent_started = OpenGate.new
    continue = Barrier.new
    continue.release
    $roda_concurrent_continue = continue
    $roda_concurrent_allow_failure_success = false

    proc { @app.freeze }.must_raise(RuntimeError)

    # The route the failing file registered before raising is rolled back.
    meth = @app.hash_branches['']['/failing']
    @app.instance_method(meth).source_location.first.must_include(
      File.join('roda', 'plugins', 'autoload_hash_branches.rb')
    )
  end
end
