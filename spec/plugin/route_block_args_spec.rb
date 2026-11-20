require_relative "../spec_helper"

describe "route_block_args plugin" do
  it "works with hooks when loaded last" do
    a = []
    app(:bare) do
      plugin :hooks
      before { a << 1 }
      after {|_| a << 2 }
      plugin :route_block_args do
        [request, response]
      end
      route do |req, res|
        response.status = 401
        a << req.path << res.status
        "1"
      end
    end
    body.must_equal "1"
    a.must_equal [1, '/', 401, 2]
  end

  it "works with hooks when loaded first" do
    a = []
    app(:bare) do
      plugin :route_block_args do
        [request, response]
      end
      plugin :hooks
      before { a << 1 }
      after {|_| a << 2 }
      route do |req, res|
        response.status = 401
        a << req.path << res.status
        "1"
      end
    end
    body.must_equal "1"
    a.must_equal [1, '/', 401, 2]
  end

  it "still supports a single route block argument" do
    app(:bare) do
      plugin :route_block_args do
        request
      end
      route { |r| "OK" }
    end

    status('/').must_equal(200)
  end

  it "supports many route block arguments" do
    app(:bare) do
      plugin :route_block_args do
        [request.params, request.env, response.headers]
      end
      route do |p, e, h|
        h['foo'] = "#{p['a']}-#{e['B']}"
      end
    end

    header('foo', 'rack.input'=>rack_input).must_equal('-')
    body('rack.input'=>rack_input).must_equal('-')
    header('foo', 'QUERY_STRING'=>'a=c', 'B'=>'D', 'rack.input'=>rack_input).must_equal('c-D')
  end

  it "works if given after the route block" do
    app(:bare) do
      opts[:check_arity] = false
      route do |p, e, h|
        h['foo'] = "#{p['a']}-#{e['B']}"
      end
      plugin :route_block_args do
        [request.params, request.env, response.headers]
      end
    end

    header('foo', 'rack.input'=>rack_input).must_equal('-')
    body('rack.input'=>rack_input).must_equal('-')
    header('foo', 'QUERY_STRING'=>'a=c', 'B'=>'D', 'rack.input'=>rack_input).must_equal('c-D')
  end

  it "allows different argument sources per named route namespace" do
    calls = []
    app(:bare) do
      plugin :named_routes
      plugin :route_block_args do
        calls << :default
        [request, :default]
      end
      route_block_args(:alpha) do
        calls << :alpha
        [request, :alpha]
      end
      route_block_args(:beta) do
        calls << :beta
        [request, :beta]
      end

      # This named route does not handle the request, so the main route
      # continues to the next named route.
      route('first', :alpha) do |_r, tag|
        calls << [:first, tag]
        nil
      end

      route('second', :beta) do |r, tag|
        calls << [:second, tag]
        r.is('second'){'second'}
      end

      route do |r, tag|
        calls << [:main, tag]
        # The alpha named route is dispatched to twice, and the beta named
        # route once, all within a single request.
        r.route('first', :alpha)
        r.route('first', :alpha)
        r.route('second', :beta)
      end
    end

    body('/second').must_equal 'second'

    # Each namespace's argument source is computed exactly once for the
    # request, even though the alpha named route block is called twice.  The
    # two named route blocks do not share the same computed arguments.
    calls.must_equal [
      :default, [:main, :default],
      :alpha, [:first, :alpha], [:first, :alpha],
      :beta, [:second, :beta]
    ]
  end

  it "does not compute arguments for namespaces that are never entered" do
    calls = []
    app(:bare) do
      plugin :named_routes
      plugin :route_block_args do
        calls << :default
        request
      end
      route_block_args(:expensive) do
        calls << :expensive
        request
      end

      route('expensive', :expensive){|r| r.is('expensive'){'expensive'}}
      route('cheap', :cheap){|r| r.is('cheap'){'cheap'}}

      route do |r|
        r.route('cheap', :cheap)
        r.is('other'){'other'}
      end
    end

    body('/other').must_equal 'other'
    calls.must_equal [:default]
  end

  it "supports a namespace when loading the plugin" do
    app(:bare) do
      plugin :named_routes
      plugin :route_block_args, :admin do
        [request, :admin]
      end

      route('users', :admin) do |r, tag|
        r.is('users'){"#{tag}-users"}
      end

      route do |r|
        r.route('users', :admin)
      end
    end

    body('/users').must_equal 'admin-users'
  end

  it "uses the parent route arguments for namespaces without a registered source" do
    calls = []
    app(:bare) do
      plugin :named_routes
      plugin :route_block_args do
        calls << :default
        [request, :default]
      end

      route('a', :unregistered) do |r, tag|
        calls << [:a, tag]
        r.is('a'){'a'}
      end

      route do |r, tag|
        calls << [:main, tag]
        r.route('a', :unregistered)
      end
    end

    body('/a').must_equal 'a'
    calls.must_equal [:default, [:main, :default], [:a, :default]]
  end

  it "allows different argument sources per hash branch and hash path namespace" do
    calls = []
    app(:bare) do
      plugin :hash_routes
      plugin :route_block_args do
        calls << :default
        [request, :default]
      end
      route_block_args('/api') do
        calls << :api
        [request, :api]
      end
      route_block_args(:secret) do
        calls << :secret
        [request, :secret]
      end

      hash_branch('/api', 'item') do |r, tag|
        calls << [:item, tag]
        r.is{'item'}
      end

      hash_path(:secret, '/key') do |r, tag|
        calls << [:key, tag]
        'key'
      end

      route do |r, tag|
        calls << [:main, tag]
        r.on('api'){r.hash_routes('/api')}
        r.on('secret'){r.hash_paths(:secret)}
      end
    end

    body('/api/item').must_equal 'item'
    calls.must_equal [:default, [:main, :default], :api, [:item, :api]]

    calls.clear
    body('/secret/key').must_equal 'key'
    calls.must_equal [:default, [:main, :default], :secret, [:key, :secret]]
  end

  it "recomputes namespace arguments for each request" do
    calls = []
    app(:bare) do
      plugin :named_routes
      plugin :route_block_args do
        request
      end
      route_block_args(:counted) do
        calls << :counted
        [request, :counted]
      end

      route('a', :counted) do |r, tag|
        r.is('a'){"#{tag}-#{r.path}"}
      end

      route do |r|
        r.route('a', :counted)
      end
    end

    body('/a').must_equal 'counted-/a'
    body('/a').must_equal 'counted-/a'
    calls.must_equal [:counted, :counted]
  end

  it "works when the application is frozen" do
    app(:bare) do
      plugin :named_routes
      plugin :route_block_args do
        [request, :default]
      end
      route_block_args(:admin) do
        [request, :admin]
      end

      route('a', :admin) do |r, tag|
        r.is('a'){tag.to_s}
      end

      route do |r, _|
        r.route('a', :admin)
      end
    end

    app.freeze
    body('/a').must_equal 'admin'
  end

  it "raises if registering namespace arguments without a block" do
    app(:bare) do
      plugin :route_block_args do
        request
      end
    end

    proc{app.route_block_args(:foo)}.must_raise Roda::RodaError
  end

  it "raises if loading the plugin without a block" do
    proc do
      app(:bare) do
        plugin :route_block_args
      end
    end.must_raise Roda::RodaError
  end

  it "supports plugins overriding convert_route_block with the old signature" do
    converted = []
    cp = Module.new
    cp::ClassMethods = Module.new do
      define_method(:convert_route_block) do |block|
        converted << block
        super(block)
      end
      private :convert_route_block
    end

    app(:bare) do
      plugin cp
      plugin :named_routes
      plugin :route_block_args do
        [request, :default]
      end
      route_block_args(:admin) do
        [request, :admin]
      end

      route('a', :admin) do |r, tag|
        r.is('a'){tag.to_s}
      end

      route do |r, _|
        r.route('a', :admin)
      end
    end

    body('/a').must_equal 'admin'
    converted.wont_be_empty
  end

  it "composes convert_route_block overrides regardless of load order" do
    order = []
    wrapping = Module.new
    wrapping::ClassMethods = Module.new do
      define_method(:convert_route_block) do |block|
        order << :wrap
        super(block)
      end
      private :convert_route_block
    end

    [:before, :after].each do |placement|
      order.clear
      app(:bare) do
        plugin wrapping if placement == :before
        plugin :named_routes
        plugin :route_block_args do
          [request, :default]
        end
        route_block_args(:admin) do
          [request, :admin]
        end
        plugin wrapping if placement == :after

        route('a', :admin) do |r, tag|
          r.is('a'){tag.to_s}
        end
        route do |r, _|
          r.route('a', :admin)
        end
      end

      body('/a').must_equal 'admin', "when convert_route_block wrapper is loaded #{placement} route_block_args"
      order.wont_be_empty "when convert_route_block wrapper is loaded #{placement} route_block_args"
    end
  end
end
