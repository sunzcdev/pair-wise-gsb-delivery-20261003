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
end
