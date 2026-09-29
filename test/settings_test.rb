require_relative 'test_helper'

class SettingsTest < Minitest::Test
  setup do
    @base = Sinatra.new(Sinatra::Base)
    @base.set :environment => :foo, :app_file => nil

    @application = Sinatra.new(Sinatra::Application)
    @application.set :environment => :foo, :app_file => nil
  end

  it 'sets settings to literal values' do
    @base.set(:foo, 'bar')
    assert @base.respond_to?(:foo)
    assert_equal 'bar', @base.foo
  end

  it 'sets settings to Procs' do
    @base.set(:foo, Proc.new { 'baz' })
    assert @base.respond_to?(:foo)
    assert_equal 'baz', @base.foo
  end

  it 'sets settings using a block' do
    @base.set(:foo){ 'baz' }
    assert @base.respond_to?(:foo)
    assert_equal 'baz', @base.foo
  end

  it 'raises an error with a value and a block' do
    assert_raises ArgumentError do
      @base.set(:fiz, 'boom!'){ 'baz' }
    end
    assert !@base.respond_to?(:fiz)
  end

  it 'raises an error without value and block' do
    assert_raises(ArgumentError) { @base.set(:fiz) }
    assert !@base.respond_to?(:fiz)
  end

  it 'allows setting a value to the app class' do
    @base.set :base, @base
    assert @base.respond_to?(:base)
    assert_equal @base, @base.base
  end

  it 'raises an error with the app class as value and a block' do
    assert_raises ArgumentError do
      @base.set(:fiz, @base) { 'baz' }
    end
    assert !@base.respond_to?(:fiz)
  end

  it "sets multiple settings with a Hash" do
    @base.set :foo => 1234,
        :bar => 'Hello World',
        :baz => Proc.new { 'bizzle' }
    assert_equal 1234, @base.foo
    assert_equal 'Hello World', @base.bar
    assert_equal 'bizzle', @base.baz
  end

  it 'sets multiple settings using #each' do
    @base.set [["foo", "bar"]]
    assert_equal "bar", @base.foo
  end

  it 'inherits settings methods when subclassed' do
    @base.set :foo, 'bar'
    @base.set :biz, Proc.new { 'baz' }

    sub = Class.new(@base)
    assert sub.respond_to?(:foo)
    assert_equal 'bar', sub.foo
    assert sub.respond_to?(:biz)
    assert_equal 'baz', sub.biz
  end

  it 'overrides settings in subclass' do
    @base.set :foo, 'bar'
    @base.set :biz, Proc.new { 'baz' }
    sub = Class.new(@base)
    sub.set :foo, 'bling'
    assert_equal 'bling', sub.foo
    assert_equal 'bar', @base.foo
  end

  it 'creates setter methods when first defined' do
    @base.set :foo, 'bar'
    assert @base.respond_to?('foo=')
    @base.foo = 'biz'
    assert_equal 'biz', @base.foo
  end

  it 'creates predicate methods when first defined' do
    @base.set :foo, 'hello world'
    assert @base.respond_to?(:foo?)
    assert @base.foo?
    @base.set :foo, nil
    assert !@base.foo?
  end

  it 'uses existing setter methods if detected' do
    class << @base
      def foo
        @foo
      end
      def foo=(value)
        @foo = 'oops'
      end
    end

    @base.set :foo, 'bam'
    assert_equal 'oops', @base.foo
  end

  it 'merges values of multiple set calls if those are hashes' do
    @base.set :foo, :a => 1
    sub = Class.new(@base)
    sub.set :foo, :b => 2
    assert_equal({:a => 1, :b => 2}, sub.foo)
  end

  it 'merging does not affect the superclass' do
    @base.set :foo, :a => 1
    sub = Class.new(@base)
    sub.set :foo, :b => 2
    assert_equal({:a => 1}, @base.foo)
  end

  it 'is possible to change a value from a hash to something else' do
    @base.set :foo, :a => 1
    @base.set :foo, :bar
    assert_equal(:bar, @base.foo)
  end

  it 'merges values with values of the superclass if those are hashes' do
    @base.set :foo, :a => 1
    @base.set :foo, :b => 2
    assert_equal({:a => 1, :b => 2}, @base.foo)
  end

  it "sets multiple settings to true with #enable" do
    @base.enable :sessions, :foo, :bar
    assert @base.sessions
    assert @base.foo
    assert @base.bar
  end

  it "sets multiple settings to false with #disable" do
    @base.disable :sessions, :foo, :bar
    assert !@base.sessions
    assert !@base.foo
    assert !@base.bar
  end

  it 'is accessible from instances via #settings' do
    assert_equal :foo, @base.new!.settings.environment
  end

  it 'is accessible from class via #settings' do
    assert_equal :foo, @base.settings.environment
  end

  describe 'default_content_type' do
    it 'defaults to html' do
      assert_equal 'text/html', @base.default_content_type
    end

    it 'can be changed' do
      @base.set :default_content_type, 'application/json'
      @base.get('/') { '{"a":1}' }
      @app = @base
      get '/'
      assert_equal 200, status
      assert_equal 'application/json', response.content_type
    end

    it 'can be disabled' do
      @base.set :default_content_type, nil
      @base.error(404) { "" }
      @app = @base
      get '/'
      assert_equal 404, status
      assert_nil response.content_type
      assert_empty body
    end

    it 'may emit content without a content-type (to be sniffed)' do
      @base.set :default_content_type, nil
      @base.get('/') { raise Sinatra::BadRequest, "This is a drill" }
      @app = @base
      get '/'
      assert_equal 400, status
      assert_nil response.content_type
      assert_body "This is a drill"
    end
  end

  describe 'methodoverride' do
    it 'is disabled on Base' do
      assert ! @base.method_override?
    end

    it 'is enabled on Application' do
      assert @application.method_override?
    end

    it 'enables MethodOverride middleware' do
      @base.set :method_override, true
      @base.put('/') { 'okay' }
      @app = @base
      post '/', {'_method'=>'PUT'}, {}
      assert_equal 200, status
      assert_equal 'okay', body
    end

    it 'is backward compatible with methodoverride' do
      assert ! @base.methodoverride?
      @base.enable :methodoverride
      assert @base.methodoverride?
    end

    it 'ignores bundler/inline from callers' do
      @application.stub(:caller, ->(_){ ['/path/to/bundler/inline.rb', $0] }) do
        assert_equal File.expand_path($0), File.expand_path(@application.send(:caller_files).first)
      end
    end
  end

  describe 'run' do
    it 'is disabled on Base' do
      assert ! @base.run?
    end

    it 'is enabled on Application except in test environment' do
      assert @application.run?

      @application.set :environment, :test
      assert ! @application.run?
    end
  end

  describe 'raise_errors' do
    it 'is enabled on Base only in test' do
      assert ! @base.raise_errors?

      @base.set(:environment, :test)
      assert @base.raise_errors?
    end

    it 'is enabled on Application only in test' do
      assert ! @application.raise_errors?

      @application.set(:environment, :test)
      assert @application.raise_errors?
    end
  end

  describe 'show_exceptions' do
    it 'is disabled on Base except under development' do
      assert ! @base.show_exceptions?
      @base.environment = :development
      assert @base.show_exceptions?
    end

    it 'is disabled on Application except in development' do
      assert ! @application.show_exceptions?

      @application.set(:environment, :development)
      assert @application.show_exceptions?
    end

    it 'returns a friendly 500' do
      klass = Sinatra.new(Sinatra::Application)
      mock_app(klass) {
        enable :show_exceptions

        get '/' do
          raise StandardError
        end
      }

      get '/'
      assert_equal 500, status
      assert body.include?("StandardError")
      assert body.include?("<code>show_exceptions</code> setting")
    end

    it 'does not attempt to show unparsable query parameters' do
      klass = Sinatra.new(Sinatra::Application)
      mock_app(klass) {
        enable :show_exceptions

        get '/' do
          raise Sinatra::BadRequest
        end
      }

      get '/'
      assert_equal 400, status
      refute body.include?('<div id="get">')
      refute body.include?('<div id="post">')
    end

    it 'does not override app-specified error handling when set to :after_handler' do
      ran = false
      mock_app do
        set :show_exceptions, :after_handler
        error(RuntimeError) { ran = true }
        get('/') { raise RuntimeError }
      end

      get '/'
      assert_equal 500, status
      assert ran
    end

    it 'does catch any other exceptions when set to :after_handler' do
      ran = false
      mock_app do
        set :show_exceptions, :after_handler
        error(RuntimeError) { ran = true }
        get('/') { raise ArgumentError }
      end

      get '/'
      assert_equal 500, status
      assert !ran
    end
  end

  describe 'dump_errors' do
    it 'is disabled on Base in test' do
      @base.environment = :test
      assert ! @base.dump_errors?
      @base.environment = :development
      assert @base.dump_errors?
      @base.environment = :production
      assert @base.dump_errors?
    end

    it 'dumps exception with backtrace to rack.errors' do
      klass = Sinatra.new(Sinatra::Application)

      mock_app(klass) {
        enable :dump_errors
        disable :raise_errors

        error do
          error = @env['rack.errors'].instance_variable_get(:@error)
          error.rewind

          error.read
        end

        get '/' do
          raise
        end
      }

      get '/'
      assert body.include?("RuntimeError") && body.include?("settings_test.rb")
    end

    it 'does not dump 404 errors' do
      klass = Sinatra.new(Sinatra::Application)

      mock_app(klass) {
        enable :dump_errors
        disable :raise_errors

        error do
          error = @env['rack.errors'].instance_variable_get(:@error)
          error.rewind

          error.read
        end

        get '/' do
          raise Sinatra::NotFound
        end
      }

      get '/'
      assert !body.include?("NotFound") && !body.include?("settings_test.rb")
    end
  end

  describe 'sessions' do
    it 'is disabled on Base' do
      assert ! @base.sessions?
    end

    it 'is disabled on Application' do
      assert ! @application.sessions?
    end
  end

  describe 'logging' do
    it 'is disabled on Base' do
      assert ! @base.logging?
    end

    it 'is enabled on Application except in test environment' do
      assert @application.logging?

      @application.set :environment, :test
      assert ! @application.logging
    end
  end

  describe 'static' do
    it 'is disabled on Base by default' do
      assert ! @base.static?
    end

    it 'is enabled on Base when public_folder is set and exists' do
      @base.set :environment, :development
      @base.set :public_folder, __dir__
      assert @base.static?
    end

    it 'is enabled on Base when root is set and root/public_folder exists' do
      @base.set :environment, :development
      @base.set :root, __dir__
      assert @base.static?
    end

    it 'is disabled on Application by default' do
      assert ! @application.static?
    end

    it 'is enabled on Application when public_folder is set and exists' do
      @application.set :environment, :development
      @application.set :public_folder, __dir__
      assert @application.static?
    end

    it 'is enabled on Application when root is set and root/public_folder exists' do
      @application.set :environment, :development
      @application.set :root, __dir__
      assert @application.static?
    end

    it 'is possible to use Module#public' do
      @base.send(:define_method, :foo) { }
      @base.send(:private, :foo)
      assert !@base.public_method_defined?(:foo)
      @base.send(:public, :foo)
      assert @base.public_method_defined?(:foo)
    end

    it 'is possible to use the keyword public in a sinatra app' do
      app = Sinatra.new do
        private
        def priv; end
        public
        def pub; end
      end
      assert !app.public_method_defined?(:priv)
      assert app.public_method_defined?(:pub)
    end
  end

  describe 'bind' do
    it 'defaults to 0.0.0.0' do
      assert_equal '0.0.0.0', @base.bind
      assert_equal '0.0.0.0', @application.bind
    end
  end

  describe 'port' do
    it 'defaults to 4567' do
      assert_equal 4567, @base.port
      assert_equal 4567, @application.port
    end
  end

  describe 'server' do
    it 'includes webrick' do
      assert @base.server.include?('webrick')
      assert @application.server.include?('webrick')
    end

    it 'includes puma' do
      assert @base.server.include?('puma')
      assert @application.server.include?('puma')
    end

    it 'includes falcon on non-jruby' do
      if RUBY_ENGINE != 'jruby'
        assert @base.server.include?('falcon')
        assert @application.server.include?('falcon')
      else
        assert !@base.server.include?('falcon')
        assert !@application.server.include?('falcon')
      end
    end
  end

  describe 'app_file' do
    it 'is nil for base classes' do
      assert_nil Sinatra::Base.app_file
      assert_nil Sinatra::Application.app_file
    end

    it 'defaults to the file subclassing' do
      assert_equal File.expand_path(__FILE__), Sinatra.new.app_file
    end
  end

  describe 'root' do
    it 'is nil if app_file is not set' do
      assert_nil @base.root
      assert_nil @application.root
    end

    it 'is equal to the expanded basename of app_file' do
      @base.app_file = __FILE__
      assert_equal File.expand_path(__dir__), @base.root

      @application.app_file = __FILE__
      assert_equal File.expand_path(__dir__), @application.root
    end
  end

  describe 'views' do
    it 'is nil if root is not set' do
      assert_nil @base.views
      assert_nil @application.views
    end

    it 'is set to root joined with views/' do
      @base.root = __dir__
      assert_equal __dir__ + "/views", @base.views

      @application.root = __dir__
      assert_equal __dir__ + "/views", @application.views
    end
  end

  describe 'public_folder' do
    it 'is nil if root is not set' do
      assert_nil @base.public_folder
      assert_nil @application.public_folder
    end

    it 'is set to root joined with public/' do
      @base.root = __dir__
      assert_equal __dir__ + "/public", @base.public_folder

      @application.root = __dir__
      assert_equal __dir__ + "/public", @application.public_folder
    end
  end

  describe 'public_dir' do
    it 'is an alias for public_folder' do
      @base.public_dir = __dir__
      assert_equal __dir__, @base.public_dir
      assert_equal @base.public_folder, @base.public_dir

      @application.public_dir = __dir__
      assert_equal __dir__, @application.public_dir
      assert_equal @application.public_folder, @application.public_dir
    end
  end

  describe 'lock' do
    it 'is disabled by default' do
      assert ! @base.lock?
      assert ! @application.lock?
    end
  end

  describe 'protection' do
    class MiddlewareTracker < Rack::Builder
      def self.track
        Rack.send :remove_const, :Builder
        Rack.const_set :Builder, MiddlewareTracker
        MiddlewareTracker.used.clear
        yield
      ensure
        Rack.send :remove_const, :Builder
        Rack.const_set :Builder, MiddlewareTracker.superclass
      end

      def self.used
        @used ||= []
      end

      def use(middleware, *)
        MiddlewareTracker.used << middleware
        super
      end
    end

    it 'sets up Rack::Protection' do
      MiddlewareTracker.track do
        Sinatra::Base.new
        assert_include MiddlewareTracker.used, Rack::Protection
      end
    end

    it 'sets up Rack::Protection::PathTraversal' do
      MiddlewareTracker.track do
        Sinatra::Base.new
        assert_include MiddlewareTracker.used, Rack::Protection::PathTraversal
      end
    end

    it 'does not set up Rack::Protection::PathTraversal when disabling it' do
      MiddlewareTracker.track do
        Sinatra.new { set :protection, :except => :path_traversal }.new
        assert_include MiddlewareTracker.used, Rack::Protection
        assert !MiddlewareTracker.used.include?(Rack::Protection::PathTraversal)
      end
    end

    it 'sets up RemoteToken if sessions are enabled' do
      MiddlewareTracker.track do
        Sinatra.new { enable :sessions }.new
        assert_include MiddlewareTracker.used, Rack::Protection::RemoteToken
      end
    end

    it 'sets up RemoteToken if sessions are enabled with a custom session store' do
      MiddlewareTracker.track do
        Sinatra.new {
          enable :sessions
          set :session_store, Rack::Session::Pool
        }.new
        assert_include MiddlewareTracker.used, Rack::Session::Pool
        assert_include MiddlewareTracker.used, Rack::Protection::RemoteToken
      end
    end

    it 'does not set up RemoteToken if sessions are disabled' do
      MiddlewareTracker.track do
        Sinatra.new.new
        assert !MiddlewareTracker.used.include?(Rack::Protection::RemoteToken)
      end
    end

    it 'sets up RemoteToken if it is configured to' do
      MiddlewareTracker.track do
        Sinatra.new { set :protection, :session => true }.new
        assert_include MiddlewareTracker.used, Rack::Protection::RemoteToken
      end
    end
  end

  describe 'pin_to_request' do
    it 'evaluates a pinned setting once per request' do
      count = 0
      mock_app do
        set(:version) { count += 1 }
        pin_to_request :version
        get('/') { [settings.version, settings.version, settings.version].join(',') }
      end

      get '/'
      assert_body '1,1,1'
      assert_equal 1, count

      get '/'
      assert_body '2,2,2'
      assert_equal 2, count
    end

    it 'evaluates pinned settings on every access outside of a request' do
      count = 0
      @base.set(:version) { count += 1 }
      @base.pin_to_request :version

      assert_equal 1, @base.version
      assert_equal 2, @base.version
      assert_equal 2, count
    end

    it 'does not cache settings that are not pinned' do
      count = 0
      mock_app do
        set(:version) { count += 1 }
        get('/') { [settings.version, settings.version].join(',') }
      end

      get '/'
      assert_body '1,2'
      assert_equal 2, count
    end

    it 'raises an ArgumentError for unknown settings' do
      assert_raises(ArgumentError) { @base.pin_to_request :unknown_setting }
    end

    it 'keeps settings pinned when they are set again' do
      count = 0
      mock_app do
        set(:version) { count += 1 }
        pin_to_request :version
        set(:version) { count += 10 }
        get('/') { [settings.version, settings.version].join(',') }
      end

      get '/'
      assert_body '10,10'
      assert_equal 10, count
    end

    it 'evaluates settings a pinned setting depends on only once per request' do
      versions = 0
      configs = 0
      mock_app do
        set(:version) { versions += 1 }
        set(:config) { configs += 1; "config-#{version}" }
        pin_to_request :version, :config
        get('/') { [settings.config, settings.config, settings.version].join(',') }
      end

      get '/'
      assert_body 'config-1,config-1,1'
      assert_equal 1, versions
      assert_equal 1, configs

      get '/'
      assert_body 'config-2,config-2,2'
    end

    it 'raises an error reporting the chain on circular dependencies' do
      klass = Sinatra.new(Sinatra::Base)
      klass.set(:first) { klass.second }
      klass.set(:second) { klass.third }
      klass.set(:third) { klass.first }
      klass.pin_to_request :first, :second, :third

      error = assert_raises(Sinatra::Error) do
        klass.with_pinned_settings { klass.first }
      end
      assert_equal 'circular dependency detected between pinned settings: first -> second -> third -> first',
                   error.message
    end

    it 'is inherited by subclasses' do
      count = 0
      parent = Sinatra.new(Sinatra::Base)
      parent.set(:version) { count += 1 }
      parent.pin_to_request :version

      mock_app(parent) do
        get('/') { [settings.version, settings.version].join(',') }
      end

      get '/'
      assert_body '1,1'
      assert_equal 1, count

      get '/'
      assert_body '2,2'
      assert_equal 2, count
    end

    it 'memoizes pinned settings of apps mounted inside a request' do
      sub_count = 0
      sub = Sinatra.new(Sinatra::Base) do
        set(:version) { sub_count += 1 }
        pin_to_request :version
        get('/') { [settings.version, settings.version].join(',') }
      end

      parent_count = 0
      mock_app do
        set(:version) { parent_count += 1 }
        pin_to_request :version
        get('/mounted') do
          body = sub.call(env.merge('PATH_INFO' => '/')).last.each.to_a.join
          "parent=#{settings.version},sub=#{body},parent=#{settings.version}"
        end
      end

      get '/mounted'
      assert_body 'parent=1,sub=1,1,parent=1'
      assert_equal 1, parent_count
      assert_equal 1, sub_count

      get '/mounted'
      assert_body 'parent=2,sub=2,2,parent=2'
      assert_equal 2, parent_count
      assert_equal 2, sub_count
    end

    it 'does not cache failed evaluations' do
      count = 0
      mock_app do
        set(:version) { count += 1; raise 'boom' if count.odd?; count }
        pin_to_request :version
        get('/') do
          first = begin
            settings.version
          rescue RuntimeError
            'error'
          end
          "#{first},#{settings.version}"
        end
      end

      get '/'
      assert_body 'error,2'

      get '/'
      assert_body 'error,4'
      assert_equal 4, count
    end

    it 'does not reuse values from a failed request' do
      count = 0
      mock_app do
        enable :show_exceptions
        set(:version) { count += 1 }
        pin_to_request :version
        get('/') do
          raise 'boom' if settings.version == 1

          "version=#{settings.version}"
        end
      end

      get '/'
      assert_equal 500, status

      get '/'
      assert_body 'version=2'
      assert_equal 2, count
    end
  end
end
