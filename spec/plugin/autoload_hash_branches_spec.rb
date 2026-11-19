require_relative "../spec_helper"

describe "hash_branches plugin" do 
  after do
    $roda_app = nil
    Dir['spec/autoload_hash_branches/**/*.rb'].each do |f|
      $LOADED_FEATURES.delete File.expand_path(f)
      $LOADED_FEATURES.delete File.realpath(f)
    end
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
end
