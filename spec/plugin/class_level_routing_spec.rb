require_relative "../spec_helper"

describe "class_level_routing plugin" do 
  before do
    app(:bare) do 
      plugin :class_level_routing
      plugin :all_verbs

      root do
        'root'
      end

      on "foo" do
        request.get "bar" do
          "foobar"
        end

        "foo"
      end

      is "d", :d do |x|
        request.get do
          "bazget#{x}"
        end

        request.post do
          "bazpost#{x}"
        end
      end

      meths = %w'get post delete head options patch put trace'
      meths.concat(%w'link unlink') if ::Rack::Request.method_defined?(:link?)
      meths.each do |meth|
        send(meth, :d) do |m|
          if meth == 'head'
            response['x'] = "x-#{meth}-#{m}"
            ''
          else
            "x-#{meth}-#{m}"
          end
        end
      end
    end
  end

  it "adds class methods for setting up routes" do
    body.must_equal 'root'
    body('/foo').must_equal 'foo'
    body('/foo/bar').must_equal 'foobar'
    body('/d/go').must_equal 'bazgetgo'
    body('/d/go', 'REQUEST_METHOD'=>'POST').must_equal 'bazpostgo'
    body('/bar').must_equal "x-get-bar"
    body('/bar', 'REQUEST_METHOD'=>'POST').must_equal "x-post-bar"
    body('/bar', 'REQUEST_METHOD'=>'DELETE').must_equal "x-delete-bar"
    header('x', '/bar', 'REQUEST_METHOD'=>'HEAD').must_equal "x-head-bar"
    body('/bar', 'REQUEST_METHOD'=>'OPTIONS').must_equal "x-options-bar"
    body('/bar', 'REQUEST_METHOD'=>'PATCH').must_equal "x-patch-bar"
    body('/bar', 'REQUEST_METHOD'=>'PUT').must_equal "x-put-bar"
    body('/bar', 'REQUEST_METHOD'=>'TRACE').must_equal "x-trace-bar"
    if ::Rack::Request.method_defined?(:link?)
      body('/bar', 'REQUEST_METHOD'=>'LINK').must_equal "x-link-bar"
      body('/bar', 'REQUEST_METHOD'=>'UNLINK').must_equal "x-unlink-bar"
    end

    status.must_equal 200
    status("/asdfa/asdf").must_equal 404

    @app = Class.new(app)
    body.must_equal 'root'
    body('/foo').must_equal 'foo'
    body('/foo/bar').must_equal 'foobar'
    body('/d/go').must_equal 'bazgetgo'
    body('/d/go', 'REQUEST_METHOD'=>'POST').must_equal 'bazpostgo'
    body('/bar').must_equal "x-get-bar"
    body('/bar', 'REQUEST_METHOD'=>'POST').must_equal "x-post-bar"
    body('/bar', 'REQUEST_METHOD'=>'DELETE').must_equal "x-delete-bar"
    header('x', '/bar', 'REQUEST_METHOD'=>'HEAD').must_equal "x-head-bar"
    body('/bar', 'REQUEST_METHOD'=>'OPTIONS').must_equal "x-options-bar"
    body('/bar', 'REQUEST_METHOD'=>'PATCH').must_equal "x-patch-bar"
    body('/bar', 'REQUEST_METHOD'=>'PUT').must_equal "x-put-bar"
    body('/bar', 'REQUEST_METHOD'=>'TRACE').must_equal "x-trace-bar"
  end

  it "only calls class level routes if routing tree doesn't handle request" do
    app.route do |r|
      r.root do
        'iroot'
      end

      r.get 'foo' do
        'ifoo'
      end

      r.on 'bar' do
        r.get true do
          response.status = 404
          ''
        end
        r.post true do
          'ibar'
        end
      end
    end

    body.must_equal 'iroot'
    body('/foo').must_equal 'ifoo'
    body('/foo/bar').must_equal 'foobar'
    body('/d/go').must_equal 'bazgetgo'
    body('/d/go', 'REQUEST_METHOD'=>'POST').must_equal 'bazpostgo'
    body('/bar').must_equal ""
    body('/bar', 'REQUEST_METHOD'=>'POST').must_equal "ibar"
    body('/bar', 'REQUEST_METHOD'=>'DELETE').must_equal "x-delete-bar"
    header('x', '/bar', 'REQUEST_METHOD'=>'HEAD').must_equal "x-head-bar"
    body('/bar', 'REQUEST_METHOD'=>'OPTIONS').must_equal "x-options-bar"
    body('/bar', 'REQUEST_METHOD'=>'PATCH').must_equal "x-patch-bar"
    body('/bar', 'REQUEST_METHOD'=>'PUT').must_equal "x-put-bar"
    body('/bar', 'REQUEST_METHOD'=>'TRACE').must_equal "x-trace-bar"
  end

  it "works with the not_found plugin if loaded before" do
    app.plugin :not_found do
      "nf"
    end

    body.must_equal 'root'
    body('/foo').must_equal 'foo'
    body('/foo/bar').must_equal 'foobar'
    body('/d/go').must_equal 'bazgetgo'
    body('/d/go', 'REQUEST_METHOD'=>'POST').must_equal 'bazpostgo'
    body('/bar').must_equal "x-get-bar"
    body('/bar', 'REQUEST_METHOD'=>'POST').must_equal "x-post-bar"
    body('/bar', 'REQUEST_METHOD'=>'DELETE').must_equal "x-delete-bar"
    header('x', '/bar', 'REQUEST_METHOD'=>'HEAD').must_equal "x-head-bar"
    body('/bar', 'REQUEST_METHOD'=>'OPTIONS').must_equal "x-options-bar"
    body('/bar', 'REQUEST_METHOD'=>'PATCH').must_equal "x-patch-bar"
    body('/bar', 'REQUEST_METHOD'=>'PUT').must_equal "x-put-bar"
    body('/bar', 'REQUEST_METHOD'=>'TRACE').must_equal "x-trace-bar"

    status.must_equal 200
    status("/asdfa/asdf").must_equal 404
    body("/asdfa/asdf").must_equal "nf"
  end

  it "works when freezing the app" do
    app.freeze
    body.must_equal 'root'
    body('/foo').must_equal 'foo'
    body('/foo/bar').must_equal 'foobar'
    body('/d/go').must_equal 'bazgetgo'
    body('/d/go', 'REQUEST_METHOD'=>'POST').must_equal 'bazpostgo'
    body('/bar').must_equal "x-get-bar"
    body('/bar', 'REQUEST_METHOD'=>'POST').must_equal "x-post-bar"
    body('/bar', 'REQUEST_METHOD'=>'DELETE').must_equal "x-delete-bar"
    header('x', '/bar', 'REQUEST_METHOD'=>'HEAD').must_equal "x-head-bar"
    body('/bar', 'REQUEST_METHOD'=>'OPTIONS').must_equal "x-options-bar"
    body('/bar', 'REQUEST_METHOD'=>'PATCH').must_equal "x-patch-bar"
    body('/bar', 'REQUEST_METHOD'=>'PUT').must_equal "x-put-bar"
    body('/bar', 'REQUEST_METHOD'=>'TRACE').must_equal "x-trace-bar"
    if ::Rack::Request.method_defined?(:link?)
      body('/bar', 'REQUEST_METHOD'=>'LINK').must_equal "x-link-bar"
      body('/bar', 'REQUEST_METHOD'=>'UNLINK').must_equal "x-unlink-bar"
    end

    status.must_equal 200
    status("/asdfa/asdf").must_equal 404

    proc{app.on{}}.must_raise
  end

  it "accepts a name option giving a route an explicit identity" do
    app(:bare) do
      plugin :class_level_routing

      get "named", name: :named_route do
        "named"
      end
    end

    route = app.class_level_routes.first
    route[:name].must_equal :named_route
    route[:request_method].must_equal :get
    route[:args].must_equal ["named"]
    body("/named").must_equal "named"
  end

  it "defaults the name to nil when no name option is given" do
    app(:bare) do
      plugin :class_level_routing

      get "unnamed" do
        "unnamed"
      end
    end

    app.class_level_routes.first[:name].must_be_nil
    body("/unnamed").must_equal "unnamed"
  end

  it "removes a named route in a subclass without affecting the parent or sibling subclasses" do
    base = app(:bare) do
      plugin :class_level_routing

      get "products", name: :products_index do
        "GET products"
      end

      post "products", name: :products_create do
        "POST products"
      end

      get "catalog", name: :catalog_index do
        "catalog"
      end
    end

    base.freeze

    subclass_a = Class.new(base) do
      remove_class_level_route(:products_index)
    end
    subclass_b = Class.new(base)

    @app = subclass_a
    status("/products").must_equal 404
    # Other request methods on the same path remain candidates
    body("/products", "REQUEST_METHOD" => "POST").must_equal "POST products"
    # Other paths remain candidates
    body("/catalog").must_equal "catalog"

    @app = subclass_b
    body("/products").must_equal "GET products"
    body("/products", "REQUEST_METHOD" => "POST").must_equal "POST products"
    body("/catalog").must_equal "catalog"

    @app = base
    body("/products").must_equal "GET products"
    body("/products", "REQUEST_METHOD" => "POST").must_equal "POST products"

    base.class_level_routes.length.must_equal 3
    base.class_level_routes.must_be :frozen?
    subclass_a.class_level_routes.length.must_equal 2
    subclass_b.class_level_routes.length.must_equal 3
    subclass_a.class_level_routes.wont_be :frozen?
    subclass_a.class_level_routes.wont_be_same_as base.class_level_routes
    subclass_b.class_level_routes.wont_be_same_as base.class_level_routes
  end

  it "allows a frozen parent application to be subclassed and have a route removed" do
    base = app(:bare) do
      plugin :class_level_routing

      get "frozen-parent", name: :fp do
        "fp"
      end
    end

    base.freeze
    base.must_be :frozen?

    subclass = Class.new(base) do
      remove_class_level_route(:fp)
    end

    @app = subclass
    status("/frozen-parent").must_equal 404

    @app = base
    body("/frozen-parent").must_equal "fp"
    base.must_be :frozen?
    base.class_level_routes.must_be :frozen?
  end

  it "removes an unnamed route by request method and match arguments" do
    base = app(:bare) do
      plugin :class_level_routing

      get "legacy" do
        "legacy get"
      end

      post "legacy" do
        "legacy post"
      end
    end

    subclass = Class.new(base) do
      remove_class_level_route(:get, "legacy")
    end

    @app = subclass
    status("/legacy").must_equal 404
    body("/legacy", "REQUEST_METHOD" => "POST").must_equal "legacy post"

    @app = base
    body("/legacy").must_equal "legacy get"
    body("/legacy", "REQUEST_METHOD" => "POST").must_equal "legacy post"
  end

  it "removes a root route by name even though it has no match arguments" do
    base = app(:bare) do
      plugin :class_level_routing

      root(name: :home) do
        "home"
      end
    end

    base.freeze

    subclass = Class.new(base) do
      remove_class_level_route(:home)
    end

    @app = subclass
    status.must_equal 404

    @app = base
    body.must_equal "home"
  end

  it "matches a string name against a symbol name and vice versa" do
    base = app(:bare) do
      plugin :class_level_routing

      get "sym", name: :sym_route do
        "sym"
      end
    end

    subclass = Class.new(base) do
      remove_class_level_route("sym_route")
    end

    @app = subclass
    status("/sym").must_equal 404
  end

  it "raises RodaError when removing a named route that cannot be found" do
    app(:bare) do
      plugin :class_level_routing

      get "here" do
        "here"
      end
    end

    proc do
      app.remove_class_level_route(:missing)
    end.must_raise Roda::RodaError
  end

  it "raises RodaError when removing by method and args that cannot be found" do
    app(:bare) do
      plugin :class_level_routing

      get "here" do
        "here"
      end
    end

    proc do
      app.remove_class_level_route(:get, "missing")
    end.must_raise Roda::RodaError
  end

  it "does not remove anything when the identity cannot be found" do
    app(:bare) do
      plugin :class_level_routing

      get "here" do
        "here"
      end
    end

    before = app.class_level_routes.dup
    proc do
      app.remove_class_level_route(:missing)
    end.must_raise Roda::RodaError
    app.class_level_routes.must_equal before
    body("/here").must_equal "here"
  end

  it "keeps other candidates on the same path after removing one route" do
    base = app(:bare) do
      plugin :class_level_routing

      is "shared" do
        request.get do
          "shared get"
        end

        request.post do
          "shared post"
        end
      end

      get "shared", name: :shared_get do
        "named shared get"
      end
    end

    base.freeze

    subclass = Class.new(base) do
      remove_class_level_route(:shared_get)
    end

    @app = subclass
    # The named GET route is gone, but the is-block GET route still matches
    body("/shared").must_equal "shared get"
    body("/shared", "REQUEST_METHOD" => "POST").must_equal "shared post"
  end

  it "returns the removed route entry" do
    app(:bare) do
      plugin :class_level_routing

      get "ret", name: :ret_route do
        "ret"
      end
    end

    route = app.remove_class_level_route(:ret_route)
    route[:name].must_equal :ret_route
    route[:request_method].must_equal :get
    app.class_level_routes.must_be :empty?
    status("/ret").must_equal 404
  end

  it "allows a subclass to re-add a route after removing one" do
    base = app(:bare) do
      plugin :class_level_routing

      get "products", name: :products_index do
        "parent products"
      end
    end

    base.freeze

    subclass = Class.new(base) do
      remove_class_level_route(:products_index)

      get "products", name: :products_index do
        "subclass products"
      end
    end

    @app = subclass
    body("/products").must_equal "subclass products"

    @app = base
    body("/products").must_equal "parent products"
  end

  it "supports nested subclasses with independent route removal" do
    base = app(:bare) do
      plugin :class_level_routing

      get "a", name: :a_route do
        "a"
      end

      get "b", name: :b_route do
        "b"
      end
    end

    base.freeze

    child = Class.new(base) do
      remove_class_level_route(:a_route)
    end

    grandchild = Class.new(child) do
      remove_class_level_route(:b_route)
    end

    @app = grandchild
    status("/a").must_equal 404
    status("/b").must_equal 404
    grandchild.class_level_routes.must_be :empty?

    @app = child
    status("/a").must_equal 404
    body("/b").must_equal "b"
    child.class_level_routes.length.must_equal 1

    @app = base
    body("/a").must_equal "a"
    body("/b").must_equal "b"
    base.class_level_routes.length.must_equal 2
  end
end
