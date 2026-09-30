require_relative "../spec_helper"
require "fileutils"

describe "hash_branches plugin" do 
  after do
    $roda_app = nil
    $roda_autoload_hash_branches_loads = nil
    $roda_autoload_hash_branches_one_started = nil
    $roda_autoload_hash_branches_one_release = nil
    $roda_autoload_hash_branches_two_started = nil
    $roda_autoload_hash_branches_two_release = nil
    $roda_autoload_hash_branches_bad_started = nil
    $roda_autoload_hash_branches_bad_release = nil
    Dir['spec/autoload_hash_branches/**/*.rb'].each do |f|
      $LOADED_FEATURES.delete File.expand_path(f)
      $LOADED_FEATURES.delete File.realpath(f)
    end
    FileUtils.rm_rf('spec/autoload_hash_branches/concurrent')
  end

  def concurrent_branch_dir
    dir = 'spec/autoload_hash_branches/concurrent'
    FileUtils.mkdir_p(dir)
    dir
  end

  def concurrent_branch_app
    app(:bare) do
      plugin :autoload_hash_branches
      route do |r|
        r.hash_branches
        '-'
      end
    end
  end

  def write_branch_file(name, contents)
    file = File.join(concurrent_branch_dir, "#{name}.rb")
    File.write(file, contents)
    File.expand_path(file)
  end

  def request_thread(app, path, queue)
    Thread.new do
      Thread.current.abort_on_exception = false
      Thread.current[:result] =
        begin
          app.call({"REQUEST_METHOD" => "GET", "PATH_INFO" => path, "SCRIPT_NAME" => "", "rack.input" => StringIO.new})
        rescue => error
          error
        end
    ensure
      queue << Thread.current
    end
  end

  def wait_for(queue)
    queue.pop
  end

  def response_body(response)
    response[2].to_enum.to_a.join
  end

  def check_autoload_hash_branches
    @app.route do |r|
      r.hash_branches
      '-'
    end
    @app.opts[:loaded] = []
    $roda_app = @app

    @app.opts[:loaded].must_equal []

    body('/c').must_equal '-'
    @app.opts[:loaded].must_equal []

    body('/b').must_equal 'b'
    @app.opts[:loaded].must_equal [:b]

    body('/a').must_equal 'a'
    @app.opts[:loaded].must_equal [:b, :a]

    status('/a/e').must_equal 404
    @app.opts[:loaded].must_equal [:b, :a, :a_e]

    body('/a/d').must_equal 'a-d'
    @app.opts[:loaded].must_equal [:b, :a, :a_e, :a_d]

    body('/a/c').must_equal 'a-c'
    @app.opts[:loaded].must_equal [:b, :a, :a_e, :a_d, :a_c]

    body('/c').must_equal '-'
    body('/b').must_equal 'b'
    body('/a').must_equal 'a'
    status('/a/e').must_equal 404
    body('/a/d').must_equal 'a-d'
    body('/a/c').must_equal 'a-c'
    @app.opts[:loaded].must_equal [:b, :a, :a_e, :a_d, :a_c]
  end

  it "should autoload hash branches on request when using autoload_hash_branch" do
    app(:bare) do
      plugin :autoload_hash_branches

      autoload_hash_branch('a', 'spec/autoload_hash_branches/a')
      autoload_hash_branch('b', 'spec/autoload_hash_branches/b')
      autoload_hash_branch('/a', 'c', 'spec/autoload_hash_branches/a/c')
      autoload_hash_branch('/a', 'd', 'spec/autoload_hash_branches/a/d')
      autoload_hash_branch('/a', 'e', 'spec/autoload_hash_branches/a/e')
    end

    check_autoload_hash_branches
  end

  it "should autoload hash branches on request when using autoload_hash_branch_dir" do
    app(:bare) do
      plugin :autoload_hash_branches

      autoload_hash_branch_dir('./spec/autoload_hash_branches')
      autoload_hash_branch_dir('/a', './spec/autoload_hash_branches/a')
    end

    check_autoload_hash_branches
  end

  it "should eager load autoload hash branches when freezing the application" do
    app(:bare) do
      plugin :autoload_hash_branches

      autoload_hash_branch('a', './spec/autoload_hash_branches/a')
      autoload_hash_branch('b', './spec/autoload_hash_branches/b')
      autoload_hash_branch('/a', 'c', './spec/autoload_hash_branches/a/c')
      autoload_hash_branch('/a', 'd', './spec/autoload_hash_branches/a/d')
      autoload_hash_branch('/a', 'e', './spec/autoload_hash_branches/a/e')

      route do |r|
        r.hash_branches
        '-'
      end
    end

    $roda_app = @app
    @app.opts[:loaded] = []
    2.times{@app.freeze}
    @app.opts[:loaded].must_equal [:a, :b, :a_c, :a_d, :a_e]
    body('/c').must_equal '-'
    body('/b').must_equal 'b'
    body('/a').must_equal 'a'
    status('/a/e').must_equal 404
    body('/a/d').must_equal 'a-d'
    body('/a/c').must_equal 'a-c'
  end

  it "should load a hash branch once while concurrent requests wait" do
    one_started = Queue.new
    one_release = Queue.new
    waiter_waiting = Queue.new
    finished = Queue.new

    file = write_branch_file('one', <<-RUBY)
      $roda_autoload_hash_branches_loads << :one
      $roda_autoload_hash_branches_one_started << :started
      $roda_autoload_hash_branches_one_release.pop
      $roda_app.hash_branch('one'){|r| 'one'}
    RUBY

    app = concurrent_branch_app
    $roda_app = app
    $roda_autoload_hash_branches_loads = []
    $roda_autoload_hash_branches_one_started = one_started
    $roda_autoload_hash_branches_one_release = one_release
    app.autoload_hash_branch('one', file)

    loader = app.autoload_hash_branch_manager.loader(['', '/one'])
    loader.singleton_class.class_eval do
      define_method(:wait) do |attempt|
        waiter_waiting << :waiting
        super(attempt)
      end
    end

    owner = request_thread(app, '/one', finished)
    wait_for(one_started)
    waiter = request_thread(app, '/one', finished)
    wait_for(waiter_waiting)
    loader.waiters.must_equal 1

    one_release << :release
    threads = 2.times.map{wait_for(finished)}
    threads.each(&:join)
    response_body(owner[:result]).must_equal 'one'
    response_body(waiter[:result]).must_equal 'one'
    $roda_autoload_hash_branches_loads.must_equal [:one]
  end

  it "should not block a different hash branch while loading" do
    one_started = Queue.new
    one_release = Queue.new
    two_started = Queue.new
    two_release = Queue.new

    one_file = write_branch_file('one', <<-RUBY)
      $roda_autoload_hash_branches_loads << :one
      $roda_autoload_hash_branches_one_started << :started
      $roda_autoload_hash_branches_one_release.pop
      $roda_app.hash_branch('one'){|r| 'one'}
    RUBY
    two_file = write_branch_file('two', <<-RUBY)
      $roda_autoload_hash_branches_loads << :two
      $roda_autoload_hash_branches_two_started << :started
      $roda_autoload_hash_branches_two_release.pop
      $roda_app.hash_branch('two'){|r| 'two'}
    RUBY

    app = concurrent_branch_app
    $roda_app = app
    $roda_autoload_hash_branches_loads = []
    $roda_autoload_hash_branches_one_started = one_started
    $roda_autoload_hash_branches_one_release = one_release
    $roda_autoload_hash_branches_two_started = two_started
    $roda_autoload_hash_branches_two_release = two_release
    app.autoload_hash_branch('one', one_file)
    app.autoload_hash_branch('two', two_file)

    finished = Queue.new
    owner = request_thread(app, '/one', finished)
    wait_for(one_started)
    other = request_thread(app, '/two', finished)
    wait_for(two_started)
    two_release << :release
    wait_for(finished).join
    response_body(other[:result]).must_equal 'two'

    one_loader = app.autoload_hash_branch_manager.loader(['', '/one'])
    one_loader.waiters.must_equal 0
    one_release << :release
    wait_for(finished).join
    response_body(owner[:result]).must_equal 'one'
    $roda_autoload_hash_branches_loads.must_equal [:one, :two]
  end

  it "should fail all waiting requests, roll back routes, and allow the next batch to retry" do
    bad_started = Queue.new
    bad_release = Queue.new
    waiter_waiting = Queue.new
    finished = Queue.new

    file = write_branch_file('bad', <<-RUBY)
      $roda_autoload_hash_branches_loads << :bad
      $roda_app.hash_branch('partial'){|r| 'partial'}
      $roda_autoload_hash_branches_bad_started << :started
      $roda_autoload_hash_branches_bad_release.pop
      raise 'load failed'
    RUBY

    app = concurrent_branch_app
    $roda_app = app
    $roda_autoload_hash_branches_loads = []
    $roda_autoload_hash_branches_bad_started = bad_started
    $roda_autoload_hash_branches_bad_release = bad_release
    app.autoload_hash_branch('bad', file)

    loader = app.autoload_hash_branch_manager.loader(['', '/bad'])
    loader.singleton_class.class_eval do
      define_method(:wait) do |attempt|
        waiter_waiting << :waiting
        super(attempt)
      end
    end

    owner = request_thread(app, '/bad', finished)
    wait_for(bad_started)
    waiter = request_thread(app, '/bad', finished)
    wait_for(waiter_waiting)
    loader.waiters.must_equal 1
    bad_release << :release
    threads = 2.times.map{wait_for(finished)}
    threads.each(&:join)

    [owner, waiter].each do |thread|
      thread[:result].must_be_kind_of RuntimeError
    end
    waiter[:result].must_be_same_as owner[:result]
    owner[:result].message.must_equal 'load failed'
    app.hash_branches['']['/partial'].must_be_nil
    body('/partial').must_equal '-'

    write_branch_file('bad', <<-RUBY)
      $roda_autoload_hash_branches_loads << :bad_retry
      $roda_app.hash_branch('bad'){|r| 'retry'}
    RUBY

    body('/bad').must_equal 'retry'
    $roda_autoload_hash_branches_loads.must_equal [:bad, :bad_retry]
  end

  it "should roll back nested hash branch routes when a nested load fails" do
    child_file = File.expand_path(File.join(concurrent_branch_dir, 'child.rb'))
    parent_file = write_branch_file('parent', <<-RUBY)
      $roda_app.autoload_hash_branch('child', "#{child_file}")
      $roda_app.hash_branch('parent') do |r|
        r.on('child') {r.hash_branches}
      end
      child_key = ['', '/child']
      child_loader = $roda_app.autoload_hash_branch_manager.loader(child_key)
      $roda_app.autoload_hash_branch_manager.load(child_loader) do
        require child_loader.file
      end
    RUBY
    write_branch_file('child', <<-RUBY)
      $roda_app.hash_branch('child_partial'){|r| 'child-partial'}
      raise 'child load failed'
    RUBY

    app = concurrent_branch_app
    $roda_app = app
    app.autoload_hash_branch('parent', parent_file)

    error = proc do
      app.call({"REQUEST_METHOD" => "GET", "PATH_INFO" => "/parent", "SCRIPT_NAME" => "", "rack.input" => StringIO.new})
    end.must_raise RuntimeError
    error.message.must_equal 'child load failed'
    app.hash_branches['']['/parent'].wont_be_nil
    app.hash_branches['']['/child_partial'].must_be_nil
    app.hash_branches['']['/child'].wont_be_nil
  end

  it "should raise immediately when loading the same hash branch recursively" do
    file = write_branch_file('recursive', <<-RUBY)
      $roda_app.call({"REQUEST_METHOD" => "GET", "PATH_INFO" => "/recursive", "SCRIPT_NAME" => "", "rack.input" => StringIO.new})
      $roda_app.hash_branch('recursive'){|r| 'recursive'}
    RUBY

    app = concurrent_branch_app
    $roda_app = app
    app.autoload_hash_branch('recursive', file)

    proc do
      app.call({"REQUEST_METHOD" => "GET", "PATH_INFO" => "/recursive", "SCRIPT_NAME" => "", "rack.input" => StringIO.new})
    end.must_raise Roda::RodaError

    app.hash_branches['']['/recursive'].wont_be_nil
  end
end
