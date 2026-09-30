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

  module AtomicHelpers
    def atomic_helper
      'atomic!'
    end
  end

  module WellBehavedExtension
    def self.registered(base)
      base.set :well_behaved_option, 'well behaved'
      base.get('/well-behaved') { 'hi' }
      base.before('/well-behaved') { pass }
      base.helpers AtomicHelpers
    end
  end

  module ExplodingExtension
    def self.registered(base)
      base.set :exploding_option, 'exploding'
      base.get('/exploding') { 'boom' }
      base.before('/exploding') { pass }
      base.helpers do
        def exploding_helper
          'boom'
        end
      end
      raise 'registration failed'
    end
  end

  it 'publishes routes, helpers, filters and settings on successful atomic registration' do
    app = Class.new(Sinatra::Base)
    app.register_atomically WellBehavedExtension
    assert_equal 'well behaved', app.well_behaved_option
    assert_equal 1, app.routes['GET'].length
    assert_equal 1, app.filters[:before].length
    assert app.method_defined?(:atomic_helper)
    assert_includes app.extensions, WellBehavedExtension
  end

  it 'withdraws all declarations when an extension raises at the end of registration' do
    app = Class.new(Sinatra::Base)
    assert_raises(RuntimeError) { app.register_atomically ExplodingExtension }
    assert !app.respond_to?(:exploding_option)
    assert !app.method_defined?(:exploding_helper)
    assert_equal [], app.routes['GET'] || []
    assert_equal [], app.filters[:before]
    assert !app.extensions.include?(ExplodingExtension)
  end

  it 'leaves pre-existing declarations untouched when atomic registration fails' do
    app = Class.new(Sinatra::Base)
    app.set :pre_existing, 'untouched'
    app.get('/pre-existing') { 'hi' }
    assert_raises(RuntimeError) { app.register_atomically ExplodingExtension }
    assert_equal 'untouched', app.pre_existing
    assert_equal 1, app.routes['GET'].length
  end

  it 'does not roll back side effects an extension performs on its own' do
    side_effects = []
    extension = Module.new do
      define_singleton_method(:registered) do |base|
        side_effects << :ran
        base.get('/doomed') { 'boom' }
        raise 'registration failed'
      end
    end
    app = Class.new(Sinatra::Base)
    assert_raises(RuntimeError) { app.register_atomically extension }
    assert_equal [:ran], side_effects
    assert_equal [], app.routes['GET'] || []
  end

  it 'allows registering an extension again after a failed atomic registration' do
    attempts = 0
    extension = Module.new do
      define_singleton_method(:registered) do |base|
        attempts += 1
        raise 'registration failed' if attempts == 1

        base.set :retried_option, 'retried'
      end
    end
    app = Class.new(Sinatra::Base)
    assert_raises(RuntimeError) { app.register_atomically extension }
    app.register_atomically extension
    assert_equal 'retried', app.retried_option
  end

  module ReentrantExtension
    def self.registered(base)
      base.register_atomically self
    end
  end

  it 'detects re-entrant atomic registration of the same extension' do
    app = Class.new(Sinatra::Base)
    error = assert_raises(RuntimeError) { app.register_atomically ReentrantExtension }
    assert_match(/re-entrant/, error.message)
  end

  it 'does not modify the parent class when a subclass registers atomically' do
    parent = Class.new(Sinatra::Base)
    child = Class.new(parent)
    child.register_atomically WellBehavedExtension
    assert_equal 'well behaved', child.well_behaved_option
    assert_equal 1, child.routes['GET'].length
    assert child.method_defined?(:atomic_helper)
    assert !parent.respond_to?(:well_behaved_option)
    assert_equal [], parent.routes['GET'] || []
    assert_equal [], parent.filters[:before]
    assert !parent.method_defined?(:atomic_helper)
    assert !parent.extensions.include?(WellBehavedExtension)
  end

  it 'does not modify the parent class when atomic registration on a subclass fails' do
    parent = Class.new(Sinatra::Base)
    child = Class.new(parent)
    assert_raises(RuntimeError) { child.register_atomically ExplodingExtension }
    assert !parent.respond_to?(:exploding_option)
    assert !parent.method_defined?(:exploding_helper)
    assert_equal [], parent.routes['GET'] || []
    assert_equal [], parent.filters[:before]
  end
end
