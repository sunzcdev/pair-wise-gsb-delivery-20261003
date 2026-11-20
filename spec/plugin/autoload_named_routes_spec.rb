require_relative "../spec_helper"

describe "named_routes plugin" do 
  after do
    $roda_app = nil
    Dir['spec/autoload_named_routes/**/*.rb'].each do |f|
      $LOADED_FEATURES.delete File.expand_path(f)
      $LOADED_FEATURES.delete File.realpath(f)
    end
  end

  it "should autoload hash branches on request when using autoload_named_route" do
    app(:bare) do
      $roda_app = self
      opts[:loaded] = []
      plugin :autoload_named_routes

      autoload_named_route(:a, 'spec/autoload_named_routes/a')
      autoload_named_route(:b, 'spec/autoload_named_routes/b')
      autoload_named_route(:a, :c, 'spec/autoload_named_routes/a/c')
      autoload_named_route(:a, :d, 'spec/autoload_named_routes/a/d')
      autoload_named_route(:a, :e, 'spec/autoload_named_routes/a/e')

      route do |r|
        r.on('a'){r.route(:a)}
        r.on('b'){r.route(:b)}
        '-'
      end
    end

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

  it "should eager load autoload hash branches when freezing the application" do
    app(:bare) do
      plugin :autoload_named_routes

      autoload_named_route(:a, './spec/autoload_named_routes/a')
      autoload_named_route(:b, './spec/autoload_named_routes/b')
      autoload_named_route(:a, :a, './spec/autoload_named_routes/a/c')
      autoload_named_route(:a, :d, './spec/autoload_named_routes/a/d')
      autoload_named_route(:a, :e, './spec/autoload_named_routes/a/e')

      route do |r|
        r.on('a'){r.route(:a)}
        r.on('b'){r.route(:b)}
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

  it "should only load autoloaded named route files once when handling concurrent first requests" do
    app(:bare) do
      $roda_app = self
      opts[:loaded] = []
      plugin :autoload_named_routes

      autoload_named_route(:concurrent, 'spec/autoload_named_routes/concurrent')

      route do |r|
        r.on('concurrent'){r.route(:concurrent)}
        '-'
      end
    end

    queue = Queue.new
    threads = 4.times.map{Thread.new{queue << body('/concurrent')}}
    threads.each(&:join)
    4.times{queue.pop.must_equal 'concurrent'}

    # The file was executed exactly once, even with concurrent first requests
    @app.opts[:loaded].must_equal [:concurrent]

    body('/concurrent').must_equal 'concurrent'
    @app.opts[:loaded].must_equal [:concurrent]
  end

  it "should reraise the recorded error and not serve partially registered routes when an autoloaded file raises" do
    app(:bare) do
      $roda_app = self
      opts[:loaded] = []
      plugin :autoload_named_routes

      autoload_named_route(:b, 'spec/autoload_named_routes/b')
      autoload_named_route(:fail_half, 'spec/autoload_named_routes/fail')
      autoload_named_route(:fail_missing, 'spec/autoload_named_routes/fail')

      route do |r|
        r.on('b'){r.route(:b)}
        r.on('fail') do
          r.on('half'){r.route(:fail_half)}
          r.on('missing'){r.route(:fail_missing)}
        end
        '-'
      end
    end

    # The file registers the fail_half route and then raises before
    # registering the fail_missing route.  Concurrent first requests all
    # receive the error, and the file is only executed once.
    queue = Queue.new
    threads = 4.times.map do
      Thread.new do
        begin
          body('/fail/half')
          queue << nil
        rescue => e
          queue << e
        end
      end
    end
    threads.each(&:join)
    4.times do
      e = queue.pop
      e.must_be_instance_of RuntimeError
      e.message.must_equal 'autoload named route load failure'
    end
    @app.opts[:loaded].must_equal [:fail]

    # The half-registered route must not handle requests, the recorded
    # error is reraised instead.
    e = proc{body('/fail/half')}.must_raise(RuntimeError)
    e.message.must_equal 'autoload named route load failure'

    # The route that was never registered due to the error reraises the
    # recorded error as well.
    e = proc{body('/fail/missing')}.must_raise(RuntimeError)
    e.message.must_equal 'autoload named route load failure'

    # The file was not executed again after the failure.
    @app.opts[:loaded].must_equal [:fail]

    # Other mount points are not affected by the failure.
    body('/b').must_equal 'b'
    body('/c').must_equal '-'
    @app.opts[:loaded].must_equal [:fail, :b]
  end

  it "should only publish autoloaded named routes to the correct class and namespace when freezing a subclass" do
    app(:bare) do
      plugin :autoload_named_routes

      autoload_named_route(:a, 'spec/autoload_named_routes/a')
      autoload_named_route(:a, :c, 'spec/autoload_named_routes/a/c')

      route do |r|
        r.on('a'){r.route(:a)}
        '-'
      end
    end

    $roda_app = @app
    @app.opts[:loaded] = []

    sub = Class.new(@app)
    $roda_app = sub
    sub.freeze

    sub_body = lambda do |path|
      s = String.new
      sub.call(req_env('PATH_INFO'=>path))[2].each{|x| s << x}
      s
    end

    # Freezing the subclass loaded the files and published the routes to
    # the subclass, in the correct namespace.
    sub_body.call('/a').must_equal 'a'
    sub_body.call('/a/c').must_equal 'a-c'
    sub.opts[:loaded].must_equal [:a, :a_c]

    # The parent class did not have the routes published to it, and the
    # files were not executed again for the parent's autoload stubs.
    status('/a').must_equal 404
    status('/a/c').must_equal 404
    @app.opts[:loaded].must_equal []
  end
end
