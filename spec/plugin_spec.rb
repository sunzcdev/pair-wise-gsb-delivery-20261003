require_relative "spec_helper"

require 'tmpdir'

describe "plugins" do
  it "should be able to override class, instance, response, and request methods, and execute configure method" do
    c = Module.new do
      self::ClassMethods = Module.new do
        def fix(str)
          opts[:prefix] + str.strip
        end
      end
      self::InstanceMethods = Module.new do
        def fix(str)
          super("a" + str)
        end
      end
      self::RequestMethods = Module.new do
        def hello(&block)
          on self.class.hello, &block
        end
      end
      self::RequestClassMethods = Module.new do
        def hello(&block)
          'hello'
        end
      end
      self::ResponseMethods = Module.new do
        def foobar
          self.class.foobar
        end
      end
      self::ResponseClassMethods = Module.new do
        def foobar
          "Default   "
        end
      end

      def self.load_dependencies(mod, prefix)
        mod.send(:include, Module.new do
          def fix(str)
            self.class.fix(str)
          end
        end)
      end

      def self.configure(mod, prefix)
        mod.opts[:prefix] = prefix
      end
    end

    app(:bare) do
      plugin(c, "Foo ").must_be_nil

      route do |r|
        r.hello do
          fix(response.foobar)
        end
      end
    end

    body('/hello').must_equal 'Foo aDefault'
  end

  it "should support registering plugins and loading them by symbol" do
    Roda::RodaPlugins.register_plugin(:foo, Module.new{module self::InstanceMethods; def a; '1' end end})
    app(:foo) do |_|
      a
    end

    body.must_equal '1'
  end

  it "should warn if attempting to load a plugin with arguments or a block" do
    Roda::RodaPlugins.register_plugin(:foo, Module.new)
    proc{app.plugin :foo, 1}.must_output(nil, /does not accept arguments or a block/)
    @app = nil
    proc{app.plugin(:foo){}}.must_output(nil, /does not accept arguments or a block/)
  end

  it "should raise error if attempting to load an invalid plugin" do
    proc{app(:banana)}.must_raise LoadError

    Dir.mktmpdir do |dir|
      begin
        $:.unshift(dir)
        Dir.mkdir(File.join(dir, 'roda'))
        Dir.mkdir(File.join(dir, 'roda', 'plugins'))
        File.write(File.join(dir, 'roda', 'plugins', 'banana.rb'), '')
        proc{app(:banana)}.must_raise Roda::RodaError

        c = Class.new(Roda)
        proc{c.plugin('banana')}.must_raise Roda::RodaError
      ensure
        $:.delete(dir)
      end
    end
  end

  it "should work with keyword arguments" do
    mod = Module.new do
      def self.load_dependencies(app, bar: 1)
        app.send(:define_method, :foo){bar.to_s}
      end
      def self.configure(app, bar: 1)
        app.send(:define_method, :bar){bar.to_s}
      end
    end
    app(:bare) do
      plugin mod, bar: 2
      route do |_|
        foo + bar 
      end
    end

    body.must_equal '22'
  end

  it "should keep a record of loaded plugins" do
    base = [Roda::RodaPlugins::Base]
    base << Roda::RodaPlugins::PlainHashResponseHeaders if ENV['PLAIN_HASH_RESPONSE_HEADERS']
    base << Roda::RodaPlugins::ShapeFriendly if ENV['SHAPE_FRIENDLY']
    app.plugins.must_equal base
    mod = Module.new{}
    app.plugin mod
    app.plugins.must_equal [*base, mod]
    Class.new(app).plugins.must_equal [*base, mod]
    app.plugins.frozen?.must_equal false
    app.freeze
    app.plugins.frozen?.must_equal true
  end
end

describe "load_plugins" do
  def tracked_plugin(configured, deps: [], conflicts: [])
    mod = Module.new do
      define_singleton_method(:plugin_dependencies){deps} unless deps.empty?
      define_singleton_method(:plugin_conflicts){conflicts} unless conflicts.empty?
      define_singleton_method(:configure){|app| configured << self}
    end
    mod
  end

  it "should load plugins in dependency order and configure shared dependencies only once" do
    configured = []
    d = tracked_plugin(configured)
    b = tracked_plugin(configured, deps: [d])
    c = tracked_plugin(configured, deps: [d])
    a = tracked_plugin(configured, deps: [b, c])

    app = Class.new(Roda)
    app.load_plugins(a, d).must_be_nil

    configured.must_equal [d, b, c, a]
    app.plugins.last(4).must_equal [d, b, c, a]
  end

  it "should resolve dependencies given as symbols of registered plugins" do
    configured = []
    b = tracked_plugin(configured)
    Roda::RodaPlugins.register_plugin(:lp_dep_b, b)
    a = tracked_plugin(configured, deps: [:lp_dep_b])

    app = Class.new(Roda)
    app.load_plugins(a)

    configured.must_equal [b, a]
  end

  it "should raise with dependency chain before configuring anything when a dependency is missing" do
    configured = []
    b = tracked_plugin(configured, deps: [:lp_missing_dep])
    a = tracked_plugin(configured, deps: [b])

    app = Class.new(Roda)
    before = app.plugins.dup
    e = proc{app.load_plugins(a)}.must_raise(Roda::RodaError)
    e.message.must_include ':lp_missing_dep'
    e.message.must_include 'dependency chain'

    configured.must_equal []
    app.plugins.must_equal before
  end

  it "should raise with dependency chain before configuring anything when a dependency cycle is detected" do
    configured = []
    a = tracked_plugin(configured)
    b = tracked_plugin(configured)
    Roda::RodaPlugins.register_plugin(:lp_cycle_a, a)
    Roda::RodaPlugins.register_plugin(:lp_cycle_b, b)
    a.define_singleton_method(:plugin_dependencies){[:lp_cycle_b]}
    b.define_singleton_method(:plugin_dependencies){[:lp_cycle_a]}

    app = Class.new(Roda)
    e = proc{app.load_plugins(:lp_cycle_a)}.must_raise(Roda::RodaError)
    e.message.must_include ':lp_cycle_a -> :lp_cycle_b -> :lp_cycle_a'

    configured.must_equal []
  end

  it "should raise with dependency chain before configuring anything when plugins in the group conflict" do
    configured = []
    x = tracked_plugin(configured)
    Roda::RodaPlugins.register_plugin(:lp_conflict_x, x)
    y = tracked_plugin(configured, deps: [], conflicts: [:lp_conflict_x])
    z = tracked_plugin(configured, deps: [y])

    app = Class.new(Roda)
    e = proc{app.load_plugins(x, z)}.must_raise(Roda::RodaError)
    e.message.must_include 'conflict'
    e.message.must_include ':lp_conflict_x'
    e.message.must_include 'dependency chain'

    configured.must_equal []
    app.plugins.wont_include x
    app.plugins.wont_include z
  end

  it "should detect conflicts with already loaded plugins in both directions" do
    configured = []
    x = tracked_plugin(configured)
    y = tracked_plugin(configured, conflicts: [x])
    z = tracked_plugin(configured)
    w = tracked_plugin(configured, conflicts: [z])

    app = Class.new(Roda)
    app.load_plugins(x, z)
    configured.must_equal [x, z]

    proc{app.load_plugins(y)}.must_raise(Roda::RodaError)
    proc{app.load_plugins(w)}.must_raise(Roda::RodaError)
    configured.must_equal [x, z]
    app.plugins.wont_include y
    app.plugins.wont_include w
  end

  it "should take plugins loaded by the parent class into account without modifying the parent" do
    configured = []
    x = tracked_plugin(configured)
    y = tracked_plugin(configured, deps: [x])
    z = tracked_plugin(configured, conflicts: [x])

    parent = Class.new(Roda)
    parent.load_plugins(x)
    configured.must_equal [x]

    child = Class.new(parent)
    child.load_plugins(y)
    configured.must_equal [x, y]
    child.plugins.must_equal parent.plugins + [y]
    parent.plugins.wont_include y

    proc{child.load_plugins(z)}.must_raise(Roda::RodaError)
    configured.must_equal [x, y]
    child.plugins.wont_include z
    parent.plugins.wont_include z
  end

  it "should leave independent apps unaffected after a failed load" do
    configured = []
    good = tracked_plugin(configured)
    bad = tracked_plugin(configured, deps: [:lp_another_missing_dep])

    app1 = Class.new(Roda)
    app2 = Class.new(Roda)

    proc{app1.load_plugins(good, bad)}.must_raise(Roda::RodaError)
    configured.must_equal []
    app1.plugins.must_equal Class.new(Roda).plugins

    app2.load_plugins(good)
    configured.must_equal [good]
    app2.plugins.must_equal Class.new(Roda).plugins + [good]
  end

  it "should raise when loading plugins into a frozen class" do
    app = Class.new(Roda)
    app.freeze
    proc{app.load_plugins(Module.new)}.must_raise(Roda::RodaError)
  end
end
