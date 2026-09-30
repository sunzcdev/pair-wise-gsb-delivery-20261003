require_relative 'test_helper'

class ExtensionsTest < Minitest::Test
  module FooExtensions
    def foo
    end

    private
      def im_hiding_in_ur_foos
      end
  end

  module BarExtensions
    def bar
    end
  end

  module BazExtensions
    def baz
    end
  end

  module QuuxExtensions
    def quux
    end
  end

  module PainExtensions
    def foo=(name); end
    def bar?(name); end
    def fizz!(name); end
  end

  it 'will add the methods to the DSL for the class in which you register them and its subclasses' do
    Sinatra::Base.register FooExtensions
    assert Sinatra::Base.respond_to?(:foo)

    Sinatra::Application.register BarExtensions
    assert Sinatra::Application.respond_to?(:bar)
    assert Sinatra::Application.respond_to?(:foo)
    assert !Sinatra::Base.respond_to?(:bar)
  end

  it 'allows extending by passing a block' do
    Sinatra::Base.register { def im_in_ur_anonymous_module; end }
    assert Sinatra::Base.respond_to?(:im_in_ur_anonymous_module)
  end

  it 'will make sure any public methods added via Application#register are delegated to Sinatra::Delegator' do
    Sinatra::Application.register FooExtensions
    assert Sinatra::Delegator.private_instance_methods.
      map { |m| m.to_sym }.include?(:foo)
    assert !Sinatra::Delegator.private_instance_methods.
      map { |m| m.to_sym }.include?(:im_hiding_in_ur_foos)
  end

  it 'will handle special method names' do
    Sinatra::Application.register PainExtensions
    assert Sinatra::Delegator.private_instance_methods.
      map { |m| m.to_sym }.include?(:foo=)
    assert Sinatra::Delegator.private_instance_methods.
      map { |m| m.to_sym }.include?(:bar?)
    assert Sinatra::Delegator.private_instance_methods.
      map { |m| m.to_sym }.include?(:fizz!)
  end

  it 'will not delegate methods on Base#register' do
    Sinatra::Base.register QuuxExtensions
    assert !Sinatra::Delegator.private_instance_methods.include?("quux")
  end

  it 'will extend the Sinatra::Application application by default' do
    Sinatra.register BazExtensions
    assert !Sinatra::Base.respond_to?(:baz)
    assert Sinatra::Application.respond_to?(:baz)
  end

  module BizzleExtension
    def bizzle
      bizzle_option
    end

    def self.registered(base)
      fail "base should be BizzleApp" unless base == BizzleApp
      fail "base should have already extended BizzleExtension" unless base.respond_to?(:bizzle)
      base.set :bizzle_option, 'bizzle!'
    end
  end

  class BizzleApp < Sinatra::Base
  end

  it 'sends .registered to the extension module after extending the class' do
    BizzleApp.register BizzleExtension
    assert_equal 'bizzle!', BizzleApp.bizzle_option
    assert_equal 'bizzle!', BizzleApp.bizzle
  end

  module WellBehavedExtension
    def well_behaved_class_method
      'class method'
    end

    def self.registered(base)
      base.set :well_behaved_setting, 'published'
      base.before { @before_filter_ran = true }
      base.helpers do
        def well_behaved_helper
          'helper works'
        end
      end
      base.get('/well-behaved') { "#{well_behaved_helper} #{settings.well_behaved_setting} #{@before_filter_ran}" }
    end
  end

  module ExplodingExtension
    def exploding_class_method; end

    def self.registered(base)
      base.set :exploding_setting, 'leaked'
      base.before { }
      base.after { }
      base.helpers do
        def exploding_helper
          'leaked'
        end
      end
      base.get('/exploding') { 'leaked' }
      raise 'boom'
    end
  end

  module ReentrantExtension
    def self.registered(base)
      base.register_atomically(self)
    end
  end

  it 'publishes routes, helpers, filters and settings on successful atomic registration' do
    mock_app
    @app.register_atomically WellBehavedExtension
    assert @app.extensions.include?(WellBehavedExtension)
    assert @app.respond_to?(:well_behaved_class_method)
    assert_equal 'published', @app.well_behaved_setting
    get '/well-behaved'
    assert_status 200
    assert_equal 'helper works published true', body
  end

  it 'rolls back all declarations when an atomically registered extension fails' do
    mock_app
    error = assert_raises(RuntimeError) { @app.register_atomically ExplodingExtension }
    assert_equal 'boom', error.message
    assert_nil @app.routes['GET']
    assert_empty @app.filters[:before]
    assert_empty @app.filters[:after]
    assert !@app.respond_to?(:exploding_setting)
    assert !@app.method_defined?(:exploding_helper)
    assert !@app.respond_to?(:exploding_class_method)
    assert !@app.extensions.include?(ExplodingExtension)
    get '/exploding'
    assert_status 404
  end

  it 'detects reentrant atomic registration of the same extension' do
    mock_app
    error = assert_raises(RuntimeError) { @app.register_atomically ReentrantExtension }
    assert_match(/already being registered atomically/, error.message)
    assert !@app.extensions.include?(ReentrantExtension)
  end

  it 'does not modify the parent class when a subclass registers atomically' do
    mock_app
    parent = @app
    child = Sinatra.new(parent)
    child.register_atomically WellBehavedExtension
    assert_nil parent.routes['GET']
    assert_empty parent.filters[:before]
    assert !parent.respond_to?(:well_behaved_setting)
    assert !parent.method_defined?(:well_behaved_helper)
    assert !parent.extensions.include?(WellBehavedExtension)
    assert_equal 'published', child.well_behaved_setting
    assert_equal 1, child.routes['GET'].length
  end
end
