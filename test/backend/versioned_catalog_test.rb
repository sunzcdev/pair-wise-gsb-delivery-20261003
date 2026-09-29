require 'test_helper'

class I18nBackendVersionedCatalogTest < I18n::TestCase
  class FakeCacheStore < Hash
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
    I18n.cache_store = nil if I18n.respond_to?(:cache_store=)
    super
  end

  test "catalog_version starts at 0 and is bumped by each publish" do
    assert_equal 0, I18n.backend.catalog_version
    I18n.backend.publish_translations { |stage| stage.store_translations(:en, :foo => 'bar') }
    assert_equal 1, I18n.backend.catalog_version
    I18n.backend.publish_translations { |stage| stage.store_translations(:en, :foo => 'baz') }
    assert_equal 2, I18n.backend.catalog_version
  end

  test "publish_translations merges staged data onto the current catalog" do
    store_translations(:en, :foo => 'bar')
    I18n.backend.publish_translations do |stage|
      stage.store_translations(:en, :foo => 'baz', :nested => { :a => 'a' })
      stage.store_translations(:en, :nested => { :b => 'b' })
    end
    assert_equal 'baz', I18n.t(:foo)
    assert_equal({ :a => 'a', :b => 'b' }, I18n.t(:nested))
  end

  test "staged changes are isolated from live translations until published" do
    store_translations(:en, :foo => 'bar')
    stage = I18n.backend.stage_catalog
    stage.store_translations(:en, :foo => 'staged', :hello => 'world')

    assert_equal 'bar', I18n.t(:foo)
    assert_nil I18n.backend.send(:lookup, :en, :hello)

    I18n.backend.publish_catalog(stage)
    assert_equal 'staged', I18n.t(:foo)
    assert_equal 'world', I18n.t(:hello)
  end

  test "publishing rejects translation data that is not a hash and keeps the old version" do
    store_translations(:en, :foo => 'bar')
    assert_raises(I18n::Backend::Simple::InvalidCatalog) do
      I18n.backend.publish_translations do |stage|
        stage.store_translations(:en, 'not a hash')
      end
    end
    assert_equal 0, I18n.backend.catalog_version
    assert_equal 'bar', I18n.t(:foo)
  end

  test "publishing rejects dangling aliases and keeps the old version" do
    store_translations(:en, :foo => 'bar')
    assert_raises(I18n::Backend::Simple::InvalidCatalog) do
      I18n.backend.publish_translations do |stage|
        stage.store_translations(:en, :broken => :missing_target)
      end
    end
    assert_equal 0, I18n.backend.catalog_version
    assert_equal 'bar', I18n.t(:foo)
    assert_nil I18n.backend.send(:lookup, :en, :broken)
  end

  test "publishing rejects cyclic aliases and keeps the old version" do
    store_translations(:en, :foo => 'bar')
    assert_raises(I18n::Backend::Simple::InvalidCatalog) do
      I18n.backend.publish_translations do |stage|
        stage.store_translations(:en, :loop_a => :loop_b, :loop_b => :loop_a)
      end
    end
    assert_equal 0, I18n.backend.catalog_version
    assert_equal 'bar', I18n.t(:foo)
  end

  test "publishing accepts aliases that resolve within the staged catalog" do
    store_translations(:en, :greeting => 'Hello')
    I18n.backend.publish_translations do |stage|
      stage.store_translations(:en, :welcome => :greeting)
    end
    assert_equal 'Hello', I18n.t(:welcome)
  end

  test "an in-flight translation keeps a consistent catalog while a new version is published" do
    backend = I18n::Backend::Simple.new
    I18n.backend = backend
    backend.store_translations(:en, :greeting => 'Hello %{name}', :welcome => :greeting)

    entered = Queue.new
    release = Queue.new
    armed = true

    # Block the in-flight translation right after it resolved the alias
    # :welcome => :greeting and before it reads the aliased entry.
    probe = Module.new do
      define_method(:lookup) do |locale, key, scope = [], options = {}|
        if armed && key.to_sym == :greeting
          entered << true
          release.pop
        end
        super(locale, key, scope, options)
      end
    end
    backend.singleton_class.prepend(probe)

    result = nil
    reader = Thread.new { result = backend.translate(:en, :welcome, :name => 'Alice') }

    # Wait until the in-flight translation is blocked mid-lookup, then
    # atomically publish a version that changes both the alias and the
    # aliased entry.
    entered.pop
    backend.publish_translations do |stage|
      stage.store_translations(:en,
        :greeting => 'Hi %{name}',
        :farewell => 'Goodbye %{name}',
        :welcome  => :farewell
      )
    end
    release << true
    reader.join
    armed = false

    # The in-flight translation must be fully resolved against the old
    # version: neither the mixed "Hi Alice" (old alias, new entry) nor the
    # new "Goodbye Alice".
    assert_equal 'Hello Alice', result
    assert_equal 'Goodbye Alice', backend.translate(:en, :welcome, :name => 'Alice')
  end

  test "memoize distinguishes catalog versions" do
    backend = Class.new(I18n::Backend::Simple) { include I18n::Backend::Memoize }.new
    I18n.backend = backend
    backend.store_translations(:en, :foo => 'old')
    assert_equal 'old', backend.translate(:en, :foo)

    backend.publish_translations { |stage| stage.store_translations(:en, :foo => 'new') }
    assert_equal 'new', backend.translate(:en, :foo)
  end

  test "memoized lookups stay pinned to their catalog version" do
    backend = Class.new(I18n::Backend::Simple) { include I18n::Backend::Memoize }.new
    I18n.backend = backend
    backend.store_translations(:en, :foo => 'old')
    assert_equal 'old', backend.translate(:en, :foo)

    backend.send(:with_pinned_catalog) do
      backend.publish_translations { |stage| stage.store_translations(:en, :foo => 'new') }
      # The pinned translation still reads (and memoizes) the old version.
      assert_equal 'old', backend.translate(:en, :foo)
    end

    # The memoized old-version entry must not leak into the new version.
    assert_equal 'new', backend.translate(:en, :foo)
  end

  test "cache keys distinguish catalog versions" do
    backend = Class.new(I18n::Backend::Simple) { include I18n::Backend::Cache }.new
    I18n.backend = backend
    key_before = backend.send(:cache_key, :en, :foo, {})
    backend.publish_translations { |stage| stage.store_translations(:en, :foo => 'bar') }
    key_after = backend.send(:cache_key, :en, :foo, {})
    refute_equal key_before, key_after
    assert_includes key_after, '/1/'
  end

  test "cached translations follow the published catalog version" do
    I18n.cache_store = FakeCacheStore.new
    backend = Class.new(I18n::Backend::Simple) { include I18n::Backend::Cache }.new
    I18n.backend = backend
    backend.store_translations(:en, :foo => 'old')
    assert_equal 'old', I18n.t(:foo)

    backend.publish_translations { |stage| stage.store_translations(:en, :foo => 'new') }
    assert_equal 'new', I18n.t(:foo)
  end
end
