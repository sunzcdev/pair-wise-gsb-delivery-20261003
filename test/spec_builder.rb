# frozen_string_literal: true

require_relative 'helper'

separate_testing do
  require_relative '../lib/rack/builder'
  require_relative '../lib/rack/lint'
  require_relative '../lib/rack/mock_request'
  require_relative '../lib/rack/content_length'
  require_relative '../lib/rack/show_exceptions'
  require_relative '../lib/rack/auth/basic'
end

class NothingMiddleware
  def initialize(app, **)
    @app = app
  end
  def call(env)
    @@env = env
    response = @app.call(env)
    response
  end
  def self.env
    @@env
  end
end

describe Rack::Builder do
  def builder(&block)
    Rack::Lint.new Rack::Builder.new(&block)
  end

  def builder_to_app(&block)
    Rack::Lint.new Rack::Builder.new(&block).to_app
  end

  it "can provide options" do
    builder = Rack::Builder.new(foo: :bar)
    builder.options[:foo].must_equal :bar
  end

  it "forwards positional hashes and keyword arguments to middleware" do
    middleware = Class.new do
      class << self
        attr_accessor :arguments
      end

      def initialize(app, options, keyword:)
        self.class.arguments = [options, keyword]
        @app = app
      end

      def call(env)
        @app.call(env)
      end
    end

    app = builder_to_app do
      use middleware, {}, keyword: true
      run lambda { |env| [200, { "content-type" => "text/plain" }, ["OK"]] }
    end

    Rack::MockRequest.new(app).get("/")
    middleware.arguments.must_equal([{}, true])
  end

  it "supports run with block" do
    app = builder_to_app do
      run {|env| [200, { "content-type" => "text/plain" }, ["OK"]]}
    end
    Rack::MockRequest.new(app).get("/").body.to_s.must_equal 'OK'
  end

  it "raises if #run provided both app and block" do
    proc {
      builder_to_app do
        run(Object.new) {|env| [200, { "content-type" => "text/plain" }, ["OK"]]}
      end
    }.must_raise(ArgumentError)
  end

  it "supports mapping" do
    app = builder_to_app do
      map '/' do |outer_env|
        run lambda { |inner_env| [200, { "content-type" => "text/plain" }, ['root']] }
      end
      map '/sub' do
        run lambda { |inner_env| [200, { "content-type" => "text/plain" }, ['sub']] }
      end
    end
    Rack::MockRequest.new(app).get("/").body.to_s.must_equal 'root'
    Rack::MockRequest.new(app).get("/sub").body.to_s.must_equal 'sub'
  end

  it "supports use when mapping" do
    app = builder_to_app do
      map '/sub' do
        use Rack::ContentLength
        run lambda { |inner_env| [200, { "content-type" => "text/plain" }, ['sub']] }
      end
      use Rack::ContentLength
      run lambda { |inner_env| [200, { "content-type" => "text/plain" }, ['root']] }
    end
    Rack::MockRequest.new(app).get("/").headers['content-length'].must_equal '4'
    Rack::MockRequest.new(app).get("/sub").headers['content-length'].must_equal '3'
  end

  it "doesn't dupe env even when mapping" do
    app = builder_to_app do
      use NothingMiddleware, noop: :noop
      map '/' do |outer_env|
        run lambda { |inner_env|
          inner_env['new_key'] = 'new_value'
          [200, { "content-type" => "text/plain" }, ['root']]
        }
      end
    end
    Rack::MockRequest.new(app).get("/").body.to_s.must_equal 'root'
    NothingMiddleware.env['new_key'].must_equal 'new_value'
  end

  it "dupe #to_app when mapping so Rack::Reloader can reload the application on each request" do
    app = builder do
      map '/' do |outer_env|
        run lambda { |env|  [200, { "content-type" => "text/plain" }, [object_id.to_s]] }
      end
    end

    builder_app1_id = Rack::MockRequest.new(app).get("/").body.to_s
    builder_app2_id = Rack::MockRequest.new(app).get("/").body.to_s

    builder_app2_id.wont_equal builder_app1_id
  end

  it "chains apps by default" do
    app = builder_to_app do
      use Rack::ShowExceptions
      run lambda { |env| raise "bzzzt" }
    end

    Rack::MockRequest.new(app).get("/").must_be :server_error?
    Rack::MockRequest.new(app).get("/").must_be :server_error?
    Rack::MockRequest.new(app).get("/").must_be :server_error?
  end

  it "has implicit #to_app" do
    app = builder do
      use Rack::ShowExceptions
      run lambda { |env| raise "bzzzt" }
    end

    Rack::MockRequest.new(app).get("/").must_be :server_error?
    Rack::MockRequest.new(app).get("/").must_be :server_error?
    Rack::MockRequest.new(app).get("/").must_be :server_error?
  end

  it "supports blocks on use" do
    app = builder do
      use Rack::ShowExceptions
      use Rack::Auth::Basic do |username, password|
        'secret' == password
      end

      run lambda { |env| [200, { "content-type" => "text/plain" }, ['Hi Boss']] }
    end

    response = Rack::MockRequest.new(app).get("/")
    response.must_be :client_error?
    response.status.must_equal 401

    # with auth...
    response = Rack::MockRequest.new(app).get("/",
        'HTTP_AUTHORIZATION' => 'Basic ' + ["joe:secret"].pack("m*"))
    response.status.must_equal 200
    response.body.to_s.must_equal 'Hi Boss'
  end

  it "has explicit #to_app" do
    app = builder do
      use Rack::ShowExceptions
      run lambda { |env| raise "bzzzt" }
    end

    Rack::MockRequest.new(app).get("/").must_be :server_error?
    Rack::MockRequest.new(app).get("/").must_be :server_error?
    Rack::MockRequest.new(app).get("/").must_be :server_error?
  end

  it "can mix map and run for endpoints" do
    app = builder do
      map '/sub' do
        run lambda { |inner_env| [200, { "content-type" => "text/plain" }, ['sub']] }
      end
      run lambda { |inner_env| [200, { "content-type" => "text/plain" }, ['root']] }
    end

    Rack::MockRequest.new(app).get("/").body.to_s.must_equal 'root'
    Rack::MockRequest.new(app).get("/sub").body.to_s.must_equal 'sub'
  end

  it "accepts middleware-only map blocks" do
    app = builder do
      map('/foo') { use Rack::ShowExceptions }
      run lambda { |env| raise "bzzzt" }
    end

    proc { Rack::MockRequest.new(app).get("/") }.must_raise(RuntimeError)
    Rack::MockRequest.new(app).get("/foo").must_be :server_error?
  end

  it "yields the generated app to a block for warmup" do
    warmed_up_app = nil

    app = Rack::Builder.new do
      warmup { |a| warmed_up_app = a }
      run lambda { |env| [200, {}, []] }
    end.to_app

    warmed_up_app.must_equal app
  end

  it "initialize apps once" do
    app = builder do
      class AppClass
        def initialize
          @called = 0
        end
        def call(env)
          raise "bzzzt"  if @called > 0
        @called += 1
          [200, { 'content-type' => 'text/plain' }, ['OK']]
        end
      end

      use Rack::ShowExceptions
      run AppClass.new
    end

    Rack::MockRequest.new(app).get("/").status.must_equal 200
    Rack::MockRequest.new(app).get("/").must_be :server_error?
  end

  it "allows use after run" do
    app = builder do
      run lambda { |env| raise "bzzzt" }
      use Rack::ShowExceptions
    end

    Rack::MockRequest.new(app).get("/").must_be :server_error?
    Rack::MockRequest.new(app).get("/").must_be :server_error?
    Rack::MockRequest.new(app).get("/").must_be :server_error?
  end

  it "supports #freeze_app for freezing app and middleware" do
    app = builder do
      freeze_app
      use Rack::ShowExceptions
      use(Class.new do
        def initialize(app) @app = app end
        def call(env) @a = 1 if env['PATH_INFO'] == '/a'; @app.call(env) end
      end)
      o = Object.new
      def o.call(env)
        @a = 1 if env['PATH_INFO'] == '/b';
        [200, {}, []]
      end
      run o
    end

    Rack::MockRequest.new(app).get("/a").must_be :server_error?
    Rack::MockRequest.new(app).get("/b").must_be :server_error?
    Rack::MockRequest.new(app).get("/c").status.must_equal 200
  end

  it 'complains about a missing run' do
    proc do
      Rack::Lint.new Rack::Builder.app { use Rack::ShowExceptions }
    end.must_raise(RuntimeError)
  end

  describe "to_plan" do
    it "captures middleware identity, arguments and map hierarchy without instantiating anything" do
      middleware = Class.new do
        def initialize(app, *); raise "must not be instantiated"; end
      end

      endpoint = lambda { |env| [200, { "content-type" => "text/plain" }, ["OK"]] }
      builder = Rack::Builder.new do
        use Rack::ContentLength
        use middleware, 1, 2, key: :value
        map '/sub' do
          use Rack::ContentLength
          run lambda { |env| [200, { "content-type" => "text/plain" }, ['sub']] }
        end
        run endpoint
      end

      plan = builder.to_plan
      plan.must_be_kind_of Rack::Builder::Plan
      plan.run.must_equal endpoint

      layers = plan.layers
      layers.size.must_equal 3

      layers[0].must_be_kind_of Rack::Builder::Plan::Map
      layers[0].mapping.keys.must_equal ['/sub']
      nested = layers[0].mapping['/sub']
      nested.must_be_kind_of Rack::Builder::Plan
      nested.layers.size.must_equal 1
      nested.layers[0].middleware.must_equal Rack::ContentLength

      layers[1].must_be_kind_of Rack::Builder::Plan::Use
      layers[1].middleware.must_equal middleware
      layers[1].arguments.must_equal [1, 2]
      layers[1].keywords.must_equal({ key: :value })

      layers[2].middleware.must_equal Rack::ContentLength
    end

    it "provides a reviewable nested hash via #to_h" do
      builder = Rack::Builder.new(foo: :bar) do
        use Rack::ContentLength, 5
        map('/sub') { run lambda { |env| [200, {}, []] } }
        run Object.new
      end

      plan = builder.to_plan
      review = plan.to_h

      review[:freeze_app].must_equal false
      review[:options].must_equal({ foo: :bar })
      review[:warmup].must_be_nil
      review[:layers][0][:type].must_equal :map
      review[:layers][0][:mapping]['/sub'][:run].must_equal :opaque
      review[:layers][1][:type].must_equal :use
      review[:layers][1][:middleware].must_equal Rack::ContentLength
      review[:layers][1][:arguments].must_equal [5]
      review[:layers][1][:block].must_be_nil
    end

    it "marks arbitrary Ruby code as opaque" do
      static = Rack::Builder.new do
        use Rack::ContentLength
        run Object.new
      end.to_plan
      static.opaque?.must_equal false
      static.reviewable?.must_equal true

      with_run_block = Rack::Builder.new do
        run lambda { |env| [200, {}, []] }
      end.to_plan
      with_run_block.opaque?.must_equal true

      with_use_block = Rack::Builder.new do
        use(Rack::Auth::Basic) { |user, pass| true }
        run Object.new
      end.to_plan
      with_use_block.opaque?.must_equal true
      with_use_block.to_h[:layers][0][:block].must_equal :opaque

      with_warmup = Rack::Builder.new do
        warmup { |app| }
        run Object.new
      end.to_plan
      with_warmup.opaque?.must_equal true
      with_warmup.to_h[:warmup].must_equal :opaque

      with_nested_opaque = Rack::Builder.new do
        map('/sub') { run lambda { |env| [200, {}, []] } }
        run Object.new
      end.to_plan
      with_nested_opaque.opaque?.must_equal true
    end

    it "builds a working application from the plan" do
      plan = Rack::Builder.new do
        use Rack::ContentLength
        map '/sub' do
          run lambda { |env| [200, { "content-type" => "text/plain" }, ['sub']] }
        end
        run lambda { |env| [200, { "content-type" => "text/plain" }, ['root']] }
      end.to_plan

      app = Rack::Lint.new(plan.to_app)
      Rack::MockRequest.new(app).get("/").body.to_s.must_equal 'root'
      Rack::MockRequest.new(app).get("/sub").body.to_s.must_equal 'sub'
    end

    it "builds completely fresh instances on each #to_app" do
      instances = []
      middleware = Class.new do
        define_method(:initialize) do |app|
          instances << self
          @app = app
        end
        define_method(:call) { |env| @app.call(env) }
      end

      plan = Rack::Builder.new do
        use middleware
        run lambda { |env| [200, { "content-type" => "text/plain" }, ['OK']] }
      end.to_plan

      plan.to_app
      plan.to_app
      instances.size.must_equal 2
      instances[0].wont_equal instances[1]
    end

    it "keeps the old application usable and closes owned resources when warmup fails" do
      closed = []
      middleware = Class.new do
        define_method(:initialize) { |app| @app = app }
        define_method(:call) { |env| @app.call(env) }
        define_method(:close) { closed << self }
      end

      should_fail = false
      plan = Rack::Builder.new do
        use middleware
        warmup { |app| raise "warmup failed" if should_fail }
        run lambda { |env| [200, { "content-type" => "text/plain" }, ['OK']] }
      end.to_plan

      old_app = plan.to_app

      should_fail = true
      error = proc { plan.to_app }.must_raise(RuntimeError)
      error.message.must_equal "warmup failed"

      # Resources created by the failed build were closed:
      closed.size.must_equal 1

      # The previously built application is untouched and still works:
      Rack::MockRequest.new(Rack::Lint.new(old_app)).get("/").body.to_s.must_equal 'OK'

      # And a subsequent successful build does not reuse the failed build's instance:
      should_fail = false
      new_app = plan.to_app
      Rack::MockRequest.new(Rack::Lint.new(new_app)).get("/").body.to_s.must_equal 'OK'
      closed.size.must_equal 1
    end

    it "closes owned resources when middleware instantiation fails" do
      closed = []
      good = Class.new do
        define_method(:initialize) { |app| @app = app }
        define_method(:call) { |env| @app.call(env) }
        define_method(:close) { closed << self }
      end
      bad = Class.new do
        def initialize(app)
          raise "cannot instantiate"
        end
      end

      plan = Rack::Builder.new do
        use bad
        use good
        run lambda { |env| [200, {}, []] }
      end.to_plan

      proc { plan.to_app }.must_raise(RuntimeError)
      closed.size.must_equal 1
    end

    it "does not close externally supplied applications on failure" do
      endpoint = Object.new
      def endpoint.call(env); [200, {}, []]; end
      def endpoint.close; raise "must not be closed"; end

      plan = Rack::Builder.new do
        warmup { |app| raise "warmup failed" }
        run endpoint
      end.to_plan

      proc { plan.to_app }.must_raise(RuntimeError)
    end

    it "does not mutate the builder when capturing a plan" do
      builder = Rack::Builder.new do
        map('/sub') { run lambda { |env| [200, {}, ['sub']] } }
        run lambda { |env| [200, {}, ['root']] }
      end

      builder.to_plan
      app = Rack::Lint.new(builder.to_app)
      Rack::MockRequest.new(app).get("/").body.to_s.must_equal 'root'
      Rack::MockRequest.new(app).get("/sub").body.to_s.must_equal 'sub'
    end
  end

  describe "parse_file" do
    def config_file(name)
      File.join(File.dirname(__FILE__), 'builder', name)
    end

    it "raises if parses commented options" do
      proc do
        Rack::Builder.parse_file config_file('options.ru')
      end.must_raise(RuntimeError).
       message.must_include('Parsing options from the first comment line is no longer supported')
    end

    it "removes __END__ before evaluating app" do
      app, _ = Rack::Builder.parse_file config_file('end.ru')
      Rack::MockRequest.new(app).get("/").body.to_s.must_equal 'OK'
    end

    it "supports multi-line comments" do
      app = Rack::Builder.parse_file(config_file('comment.ru'))
      app.must_be_kind_of(Proc)
    end

    it 'requires an_underscore_app not ending in .ru' do
      $: << File.dirname(__FILE__)
      app, * = Rack::Builder.parse_file 'builder/an_underscore_app'
      Rack::MockRequest.new(app).get('/').body.to_s.must_equal 'OK'
      $:.pop
    end

    it "sets __LINE__ correctly" do
      app, _ = Rack::Builder.parse_file config_file('line.ru')
      Rack::MockRequest.new(app).get("/").body.to_s.must_equal '3'
    end

    it "strips leading unicode byte order mark when present" do
      enc = Encoding.default_external
      begin
        verbose, $VERBOSE = $VERBOSE, nil
        Encoding.default_external = 'UTF-8'
        app, _ = Rack::Builder.parse_file config_file('bom.ru')
        Rack::MockRequest.new(app).get("/").body.to_s.must_equal 'OK'
      ensure
        Encoding.default_external = enc
        $VERBOSE = verbose
      end
    end

    it "respects the frozen_string_literal magic comment" do
      app, _ = Rack::Builder.parse_file(config_file('frozen.ru'))
      response = Rack::MockRequest.new(app).get('/')
      response.body.must_equal 'frozen'
      body = response.instance_variable_get(:@body)
      body.must_equal(['frozen'])
      body[0].frozen?.must_equal true
    end
  end

  describe 'new_from_string' do
    it "builds a rack app from string" do
      app, = Rack::Builder.new_from_string "run lambda{|env| [200, {'content-type' => 'text/plane'}, ['OK']] }"
      Rack::MockRequest.new(app).get("/").body.to_s.must_equal 'OK'
    end
  end

  it "does not strip byte order mark if file is not read as UTF-8" do
    enc = Encoding.default_external
    begin
      verbose, $VERBOSE = $VERBOSE, nil
      Encoding.default_external = 'BINARY'
      app, _ = Rack::Builder.parse_file File.join(__dir__, 'builder/line.ru')
      Rack::MockRequest.new(app).get("/").body.to_s.must_equal '3'
    ensure
      Encoding.default_external = enc
      $VERBOSE = verbose
    end
  end
end
