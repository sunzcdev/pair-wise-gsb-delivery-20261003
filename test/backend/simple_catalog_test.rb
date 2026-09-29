require 'test_helper'
require 'timeout'
require 'i18n/backend/cache'

class I18nBackendSimpleCatalogTest < I18n::TestCase
  # Blocks a lookup for a configured key until it is released, so tests can
  # publish a new catalog version while a translation is in progress.
  module LookupBarrier
    attr_accessor :barrier_key, :barrier_entered, :barrier_release

    def lookup(locale, key, scope = [], options = EMPTY_HASH)
      if barrier_key && key == barrier_key
        self.barrier_key = nil # one-shot
        barrier_entered << true
        barrier_release.pop
      end
      super
    end
  end

  class BarrierBackend < I18n::Backend::Simple
    include LookupBarrier
  end

  class MemoizedBarrierBackend < I18n::Backend::Simple
    include I18n::Backend::Memoize
    include LookupBarrier
  end

  class CacheStore < Hash
    def read(key)
      self[key]
    end

    def write(key, value)
      self[key] = value
    end
  end

  def setup
    super
    I18n.backend = I18n::Backend::Simple.new
  end

  def teardown
    I18n.cache_store = nil
    super
  end

  test "publish merges data into a new catalog version and swaps it in atomically" do
    store_translations(:en, :foo => 'bar')
    assert_equal 0, I18n.backend.catalog_version

    catalog = I18n.backend.publish(:en => { :foo => 'baz', :hello => 'world' }, :de => { :hello => 'welt' })

    assert_equal 1, catalog.version
    assert_equal 1, I18n.backend.catalog_version
    assert_equal 'baz', I18n.t(:foo, :locale => :en)
    assert_equal 'world', I18n.t(:hello, :locale => :en)
    assert_equal 'welt', I18n.t(:hello, :locale => :de)
  end

  test "publish symbolizes keys like store_translations does" do
    I18n.backend.publish('en' => { 'foo' => { 'bar' => 'baz' } })
    assert_equal 'baz', I18n.t(:'foo.bar', :locale => :en)
  end

  test "publish does not modify the previous catalog" do
    store_translations(:en, :foo => { :bar => 'bar' })
    previous = I18n.backend.current_catalog

    I18n.backend.publish(:en => { :foo => { :baz => 'baz' } })

    assert_equal({ :bar => 'bar' }, previous.data[:en][:foo])
  end

  test "a failing publish keeps the current catalog version" do
    store_translations(:en, :foo => 'bar')

    assert_raises(I18n::ArgumentError) { I18n.backend.publish('not a hash') }
    assert_raises(I18n::ArgumentError) { I18n.backend.publish(:en => 'not a hash') }

    assert_equal 0, I18n.backend.catalog_version
    assert_equal 'bar', I18n.t(:foo, :locale => :en)
  end

  test "a translation in progress keeps reading the catalog version it started with" do
    backend = BarrierBackend.new
    I18n.backend = backend
    store_translations(:en,
      :greeter => :welcome,
      :welcome => { :one => 'Hello %{name}', :other => 'Hello everyone' }
    )

    entered = Queue.new
    release = Queue.new
    backend.barrier_entered = entered
    backend.barrier_release = release
    backend.barrier_key = :welcome

    result = error = nil
    reader = Thread.new do
      begin
        I18n.backend = backend
        result = I18n.t(:greeter, :locale => :en, :count => 1, :name => 'David')
      rescue Exception => e
        error = e
      end
    end

    begin
      # wait until the reader resolved the old alias and is about to read its target
      Timeout.timeout(5) { entered.pop }

      # publish a version that changes both the alias and the aliased entry
      backend.publish(:en => {
        :greeter => :howdy,
        :howdy   => { :one => 'Howdy %{name}', :other => 'Howdy everyone' },
        :welcome => { :one => 'NEW %{name}', :other => 'NEW everyone' }
      })
    ensure
      release << true
      reader.join(5)
    end
    raise error if error

    # alias resolution, pluralization and interpolation all come from the old version
    assert_equal 'Hello David', result
    # translations started after the publish see the complete new version
    assert_equal 'Howdy David', I18n.t(:greeter, :locale => :en, :count => 1, :name => 'David')
  end

  test "memoized lookups are scoped to the catalog version" do
    backend = MemoizedBarrierBackend.new
    I18n.backend = backend
    store_translations(:en, :greeter => :welcome, :welcome => 'Hello %{name}')

    assert_equal 'Hello David', I18n.t(:welcome, :locale => :en, :name => 'David')

    entered = Queue.new
    release = Queue.new
    backend.barrier_entered = entered
    backend.barrier_release = release
    backend.barrier_key = :welcome

    result = error = nil
    reader = Thread.new do
      begin
        I18n.backend = backend
        result = I18n.t(:greeter, :locale => :en, :name => 'David')
      rescue Exception => e
        error = e
      end
    end

    begin
      Timeout.timeout(5) { entered.pop }

      backend.publish(:en => {
        :greeter => :howdy,
        :howdy   => 'Howdy %{name}',
        :welcome => 'NEW %{name}'
      })
    ensure
      release << true
      reader.join(5)
    end
    raise error if error

    # the in-flight translation memoized old version values under the old version
    assert_equal 'Hello David', result
    # ... which must not leak into the new version's memoizations
    assert_equal 'Howdy David', I18n.t(:greeter, :locale => :en, :name => 'David')
    assert_equal 'NEW David', I18n.t(:welcome, :locale => :en, :name => 'David')
  end

  test "cached translations are scoped to the catalog version" do
    backend = Class.new(I18n::Backend::Simple) { include I18n::Backend::Cache }.new
    I18n.backend = backend
    I18n.cache_store = CacheStore.new
    store_translations(:en, :foo => 'old')

    key_v0 = backend.send(:cache_key, :en, :foo, {})
    assert_equal 'old', I18n.t(:foo, :locale => :en)
    assert_equal 'old', I18n.cache_store[key_v0]

    backend.publish(:en => { :foo => 'new' })

    key_v1 = backend.send(:cache_key, :en, :foo, {})
    assert key_v0 != key_v1
    assert_equal 'new', I18n.t(:foo, :locale => :en)
    assert_equal 'new', I18n.cache_store[key_v1]
    # the stale entry is left behind but never read again
    assert_equal 'old', I18n.cache_store[key_v0]
  end
end
