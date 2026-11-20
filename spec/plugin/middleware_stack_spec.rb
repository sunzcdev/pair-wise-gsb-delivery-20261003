require_relative "../spec_helper"

describe "middleware_stack plugin" do 
  it "adds middleware_stack method for removing and inserting into middleware stack" do
    make_middleware = lambda do |name|
      Class.new do
        define_singleton_method(:name){name}

        attr_reader :app
        attr_reader :args
        attr_reader :block
        def initialize(app, *args, &block)
          @app = app
          @args = args
          @block = block
        end

        def call(env)
          (env['rack.record'] ||= []) << [self.class.name, args, block]
          app.call(env)
        end
      end
    end

    recorded = nil

    app(:middleware_stack) do |r|
      recorded = env['rack.record']
      nil 
    end

    status.must_equal 404
    recorded.must_be_nil

    called = false
    app.middleware_stack.before{called = true}.use(make_middleware[:m1], :a1).must_be_nil
    unless_lint do
      called.must_equal false
    end

    status.must_equal 404
    recorded.must_equal [[:m1, [:a1], nil]]

    app.middleware_stack.before{|m, *a| m.name == :m1}.use(make_middleware[:m2]).must_be_nil

    status.must_equal 404
    recorded.must_equal [[:m2, [], nil], [:m1, [:a1], nil]]

    b = lambda{}
    app.middleware_stack.before{|m, *a| m.name == :m1}.use(make_middleware[:m3], :a2, :a3, &b).must_be_nil

    status.must_equal 404
    recorded.must_equal [[:m2, [], nil], [:m3, [:a2, :a3], b], [:m1, [:a1], nil]]

    app.middleware_stack.after{|m, *a| m.name == :m4}.use(make_middleware[:m4]).must_be_nil
    status.must_equal 404
    recorded.must_equal [[:m2, [], nil], [:m3, [:a2, :a3], b], [:m1, [:a1], nil], [:m4, [], nil]]

    app.middleware_stack.after{|m, *a| m.name == :m4}.use(make_middleware[:m5]).must_be_nil
    status.must_equal 404
    recorded.must_equal [[:m2, [], nil], [:m3, [:a2, :a3], b], [:m1, [:a1], nil], [:m4, [], nil], [:m5, [], nil]]

    app.middleware_stack.after{|m, *a| a == [:a1]}.use(make_middleware[:m6]).must_be_nil
    status.must_equal 404
    recorded.must_equal [[:m2, [], nil], [:m3, [:a2, :a3], b], [:m1, [:a1], nil], [:m6, [], nil], [:m4, [], nil], [:m5, [], nil]]

    app.middleware_stack.remove{|m, *a| a.empty?}.must_be_nil
    status.must_equal 404
    recorded.must_equal [[:m3, [:a2, :a3], b], [:m1, [:a1], nil]]

    sp = app.middleware_stack.after{|m, *a| m.name == :m3}
    sp.use(make_middleware[:m7])
    sp.use(make_middleware[:m8])
    status.must_equal 404
    recorded.must_equal [[:m3, [:a2, :a3], b], [:m7, [], nil], [:m8, [], nil], [:m1, [:a1], nil]]

    sp = app.middleware_stack.before{|m, *a| m.name == :m8}
    sp.use(make_middleware[:m9])
    sp.use(make_middleware[:m10])
    status.must_equal 404
    recorded.must_equal [[:m3, [:a2, :a3], b], [:m7, [], nil], [:m9, [], nil], [:m10, [], nil], [:m8, [], nil], [:m1, [:a1], nil]]
  end

  it "supports naming middleware and declaring order constraints" do
    make_middleware = middleware_classes

    middleware_app do
      middleware_stack.named(:logger).use(make_middleware[:logger])
      middleware_stack.named(:session).use(make_middleware[:session]).after(:logger)
      middleware_stack.named(:router).use(make_middleware[:router]).after(:session)
    end

    middleware_order.must_equal [:logger, :session, :router]
    request_order.must_equal [:logger, :session, :router]
  end

  it "supports the same middleware class under two names with different arguments" do
    cache = middleware_classes[:cache]

    middleware_app do
      middleware_stack.named(:cache_public).use(cache, 60)
      middleware_stack.named(:cache_private).use(cache, 5).after(:cache_public)
    end

    middleware_order.must_equal [:cache_public, :cache_private]
    recorded_arguments.must_equal [[cache, [60]], [cache, [5]]]
  end

  it "supports declaring a constraint before the target middleware is added" do
    make_middleware = middleware_classes

    middleware_app do
      middleware_stack.named(:csrf).use(make_middleware[:csrf]).after(:logger).before(:router)
      middleware_stack.named(:router).use(make_middleware[:router])
      middleware_stack.named(:logger).use(make_middleware[:logger])
    end

    middleware_order.must_equal [:logger, :csrf, :router]
    request_order.must_equal [:logger, :csrf, :router]
  end

  it "supports adding unnamed middleware relative to a named entry" do
    make_middleware = middleware_classes

    middleware_app do
      middleware_stack.named(:logger).use(make_middleware[:logger])
      middleware_stack.before(:logger).use(make_middleware[:head])
      middleware_stack.after(:logger).use(make_middleware[:tail])
    end

    middleware_order.must_equal [make_middleware[:head], :logger, make_middleware[:tail]]
    request_order.must_equal [:head, :logger, :tail]
  end

  it "supports naming middleware after it has been added" do
    make_middleware = middleware_classes

    middleware_app do
      middleware_stack.use(make_middleware[:logger]).named(:logger)
      middleware_stack.use(make_middleware[:session]).named(:session).after(:logger)
    end

    middleware_order.must_equal [:logger, :session]
    request_order.must_equal [:logger, :session]
  end

  it "keeps unconstrained middleware in their original order" do
    make_middleware = middleware_classes

    middleware_app do
      use make_middleware[:first]
      use make_middleware[:second]
      middleware_stack.named(:third).use(make_middleware[:third]).before(:fourth)
      use make_middleware[:last]
      middleware_stack.named(:fourth).use(make_middleware[:fourth])
    end

    middleware_order.must_equal [make_middleware[:first], make_middleware[:second], :third, make_middleware[:last], :fourth]
    request_order.must_equal [:first, :second, :third, :last, :fourth]
  end

  it "raises an error pointing at the declaration for an undefined name" do
    make_middleware = middleware_classes

    middleware_app do
      middleware_stack.named(:logger).use(make_middleware[:logger]).after(:loger)
    end

    e = proc{app.app}.must_raise(Roda::RodaError)
    e.message.must_include "references undefined middleware name :loger"
    e.message.must_include "middleware_stack_spec.rb"
  end

  it "raises an error pointing at the declaration when no middleware was added for a name" do
    make_middleware = middleware_classes

    middleware_app do
      middleware_stack.named(:logger).use(make_middleware[:logger])
      middleware_stack.named(:unused)
      middleware_stack.named(:session).use(make_middleware[:session]).after(:unused)
    end

    e = proc{app.middleware_stack.order}.must_raise(Roda::RodaError)
    e.message.must_include "middleware name :unused, but no middleware was added for it"
    e.message.must_include "name declared at"
    e.message.must_include "middleware_stack_spec.rb"
  end

  it "raises an error pointing at the declarations for a cycle" do
    make_middleware = middleware_classes

    middleware_app do
      middleware_stack.named(:one).use(make_middleware[:one]).after(:two)
      middleware_stack.named(:two).use(make_middleware[:two]).after(:three)
      middleware_stack.named(:three).use(make_middleware[:three]).after(:one)
    end

    e = proc{app.middleware_stack.order}.must_raise(Roda::RodaError)
    e.message.must_include "form a cycle"
    e.message.must_include "must come before"
    e.message.scan("middleware_stack_spec.rb").length.must_be :>=, 3
  end

  it "raises an error for a self-referential constraint" do
    make_middleware = middleware_classes

    middleware_app do
      middleware_stack.named(:one).use(make_middleware[:one]).after(:one)
    end

    e = proc{app.app}.must_raise(Roda::RodaError)
    e.message.must_include "form a cycle"
    e.message.must_include ":one must come before :one"
  end

  it "raises an error when the same name is declared twice in the same class" do
    make_middleware = middleware_classes

    e = proc do
      middleware_app do
        middleware_stack.named(:one).use(make_middleware[:one])
        middleware_stack.named(:one).use(make_middleware[:one])
      end
    end.must_raise(Roda::RodaError)
    e.message.must_include "middleware name :one is already declared"
    e.message.must_include "middleware_stack_spec.rb"
  end

  it "allows named middleware to be removed by name, removing constraints referencing it" do
    make_middleware = middleware_classes

    middleware_app do
      middleware_stack.named(:logger).use(make_middleware[:logger])
      middleware_stack.named(:session).use(make_middleware[:session])
      middleware_stack.named(:csrf).use(make_middleware[:csrf]).after(:logger).before(:session)
    end

    app.middleware_stack.remove(:csrf).must_be_nil
    middleware_order.must_equal [:logger, :session]
    request_order.must_equal [:logger, :session]
  end

  it "supports two subclasses of the same parent using different ordering configurations" do
    make_middleware = middleware_classes

    parent = app(:middleware_stack) do |r|
      r.run(env['subapp'])
    end

    sub1 = Class.new(parent) do
      middleware_stack.named(:logger).use(make_middleware[:logger])
      middleware_stack.named(:session).use(make_middleware[:session]).after(:logger)
    end
    sub1.route{|r| "sub1"}

    sub2 = Class.new(parent) do
      middleware_stack.named(:session).use(make_middleware[:session])
      middleware_stack.named(:logger).use(make_middleware[:logger]).after(:session)
      middleware_stack.named(:router).use(make_middleware[:router]).after(:session).before(:logger)
    end
    sub2.route{|r| "sub2"}

    # The parent has no configured middleware, and each subclass resolves and
    # applies its own ordering independently.
    @app = parent
    middleware_order.must_equal []

    @app = sub1
    middleware_order.must_equal [:logger, :session]
    request_order.must_equal [:logger, :session]
    body.must_equal "sub1"

    @app = sub2
    middleware_order.must_equal [:session, :router, :logger]
    request_order.must_equal [:session, :router, :logger]
    body.must_equal "sub2"

    # Building sub2 does not change sub1's resolved order.
    @app = sub1
    middleware_order.must_equal [:logger, :session]
    body.must_equal "sub1"

    # Ordering failures are isolated to the subclass that declares them, and
    # to the point where the rack app is built (not when the constraint is
    # declared).
    sub3 = Class.new(parent) do
      middleware_stack.named(:logger).use(make_middleware[:logger]).after(:missing)
    end
    sub3.route{|r| "sub3"}
    proc{sub3.app}.must_raise(Roda::RodaError)

    # Neither the failing sibling nor the parent is affected.
    parent.app.must_be_kind_of(Object)
    @app = sub1
    middleware_order.must_equal [:logger, :session]
    body.must_equal "sub1"
    @app = sub2
    middleware_order.must_equal [:session, :router, :logger]
    body.must_equal "sub2"
  end

  it "raises when called without a name, block, or middleware" do
    middleware_app{}
    proc{app.middleware_stack.before}.must_raise(Roda::RodaError)
    proc{app.middleware_stack.after}.must_raise(Roda::RodaError)
    proc{app.middleware_stack.remove}.must_raise(Roda::RodaError)
    proc{app.middleware_stack.before(:a){true}}.must_raise(Roda::RodaError)
  end

  it "supports keyword arguments for named, relative, and Stack#use middleware" do
    make_middleware = Class.new do
      def initialize(app, key: nil)
        @app = app
        @key = key
      end

      def call(env)
        (env['rack.record'] ||= []) << @key
        @app.call(env)
      end
    end

    middleware_app do
      middleware_stack.named(:logger).use(make_middleware, key: :logger)
      middleware_stack.named(:session).use(make_middleware, key: :session).after(:logger)
      middleware_stack.after(:session).use(make_middleware, key: :relative)
      middleware_stack.use(make_middleware, key: :unnamed)
    end

    env = req_env({})
    app.call(env)
    env['rack.record'].must_equal [:logger, :session, :relative, :unnamed]
  end

  it "raises when modifying a frozen app's middleware stack" do
    make_middleware = middleware_classes

    middleware_app do
      middleware_stack.named(:logger).use(make_middleware[:logger])
    end

    app.freeze
    middleware_order.must_equal [:logger]

    proc{app.middleware_stack.named(:session)}.must_raise(Roda::RodaError)
    proc{app.middleware_stack.use(make_middleware[:session])}.must_raise(Roda::RodaError)
    proc{app.middleware_stack.remove(:logger)}.must_raise(Roda::RodaError)
    proc{app.middleware_stack.named(:logger).after(:session)}.must_raise(Roda::RodaError)
  end

  private

  # Create an app with the middleware_stack plugin, and call the block in the
  # class context so middleware can be configured.
  def middleware_app(&block)
    a = app(:bare){plugin :middleware_stack}
    a.class_exec(&block)
    a.route{|r| nil}
    @app = a
  end

  # The resolved middleware order, ignoring Rack::Lint, which the spec helper
  # adds to every app when running with LINT=1.
  def middleware_order
    app.middleware_stack.order.reject{|entry| entry.equal?(lint_middleware)}
  end

  # Rack::Lint if the specs are running with LINT=1, nil otherwise.
  def lint_middleware
    defined?(Rack::Lint) ? Rack::Lint : nil
  end

  def middleware_classes
    [:logger, :session, :router, :csrf, :cache, :head, :tail, :first, :second, :third, :fourth, :last, :one, :two, :three].each_with_object({}) do |name, h|
      h[name] = Class.new do
        define_singleton_method(:name){name}
        attr_reader :app
        attr_reader :args
        def initialize(app, *args)
          @app = app
          @args = args
        end
        def call(env)
          (env['rack.record'] ||= []) << [self.class, args]
          app.call(env)
        end
      end
    end
  end

  # Record the order middleware are called in for the next request.
  def request_order
    env = req_env({})
    @app.call(env)
    env['rack.record'].map{|klass, _| klass.name}.reject{|name| name.equal?(lint_middleware && lint_middleware.name)}
  end

  def recorded_arguments
    env = req_env({})
    @app.call(env)
    env['rack.record'].reject{|klass, _| klass.equal?(lint_middleware)}
  end
end
