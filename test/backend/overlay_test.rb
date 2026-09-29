require 'test_helper'

class I18nBackendOverlayTest < I18n::TestCase
  class Backend < I18n::Backend::Simple
    include I18n::Backend::Overlay
  end

  class CachedBackend < I18n::Backend::Simple
    include I18n::Backend::Cache
    include I18n::Backend::Overlay
  end

  class HashStore
    attr_reader :data

    def initialize
      @data = {}
    end

    def read(key)
      @data[key]
    end

    def write(key, value)
      @data[key] = value
    end
  end

  DELETED = I18n::Backend::Overlay::DELETED

  def setup
    super
    I18n.backend = Backend.new
    store_translations(:en, {
      :greeting => 'Hello',
      :tagline  => 'Base tagline',
      :admin    => { :panel => { :title => 'Dashboard' } }
    })
    store_translations(:de, { :greeting => 'Hallo' })
  end

  def teardown
    I18n.cache_store = nil
    super
  end

  test "overrides translations for the duration of the block only" do
    assert_equal 'Hello', I18n.t(:greeting)
    I18n.with_overlay(:en => { :greeting => 'Howdy' }) do
      assert_equal 'Howdy', I18n.t(:greeting)
    end
    assert_equal 'Hello', I18n.t(:greeting)
  end

  test "keys not covered by the overlay fall through to the backend" do
    I18n.with_overlay(:en => { :greeting => 'Howdy' }) do
      assert_equal 'Base tagline', I18n.t(:tagline)
      assert_equal 'Dashboard', I18n.t(:'admin.panel.title')
    end
  end

  test "does not touch the global backend" do
    backend = I18n.backend
    I18n.with_overlay(:en => { :greeting => 'Howdy' }) do
      assert_same backend, I18n.backend
    end
    assert_same backend, I18n.backend
  end

  test "overlay is scoped per locale" do
    I18n.with_overlay(:en => { :greeting => 'Howdy' }) do
      assert_equal 'Howdy', I18n.t(:greeting, :locale => :en)
      assert_equal 'Hallo', I18n.t(:greeting, :locale => :de)
    end
  end

  test "nested overlays: inner wins, outer fills the rest, both unwind" do
    I18n.with_overlay(:en => { :greeting => 'Outer', :tagline => 'Outer tagline' }) do
      I18n.with_overlay(:en => { :greeting => 'Inner' }) do
        assert_equal 'Inner', I18n.t(:greeting)
        assert_equal 'Outer tagline', I18n.t(:tagline)
      end
      assert_equal 'Outer', I18n.t(:greeting)
    end
    assert_equal 'Hello', I18n.t(:greeting)
    assert_equal 'Base tagline', I18n.t(:tagline)
  end

  test "restores the context when the block raises" do
    assert_raises(RuntimeError) do
      I18n.with_overlay(:en => { :greeting => 'Howdy' }) { raise 'boom' }
    end
    assert_equal 'Hello', I18n.t(:greeting)
    assert I18n::Backend::Overlay.current_stack.empty?
  end

  test "overlay is read-only and does not mutate the given hash or the backend" do
    overlay = { :en => { :greeting => 'Howdy' } }
    I18n.with_overlay(overlay) do
      assert_equal 'Howdy', I18n.t(:greeting)
      assert_raises(FrozenError) { I18n::Backend::Overlay.current_stack.last[:en][:greeting] = 'x' }
    end
    assert_equal({ :en => { :greeting => 'Howdy' } }, overlay)
    assert_equal 'Hello', I18n.t(:greeting)
  end

  test "accepts string keys and scope/separator lookups" do
    I18n.with_overlay('en' => { 'admin' => { 'panel' => { 'title' => 'Brand HQ' } } }) do
      assert_equal 'Brand HQ', I18n.t(:'admin.panel.title')
      assert_equal 'Brand HQ', I18n.t(:title, :scope => [:admin, :panel])
    end
  end

  test "missing, nil and deleted are three distinct states" do
    # missing: the overlay does not contain the key -> backend value
    I18n.with_overlay(:en => {}) do
      assert_equal 'Hello', I18n.t(:greeting)
      assert I18n.exists?(:greeting)
    end

    # nil: masks exactly this key; behaves like a stored nil (default applies,
    # raise raises), but keys nested below a nil leaf still fall through
    I18n.with_overlay(:en => { :greeting => nil, :admin => nil }) do
      assert_equal 'fallback', I18n.t(:greeting, :default => 'fallback')
      assert_raises(I18n::MissingTranslationData) { I18n.t(:greeting, :raise => true) }
      assert !I18n.exists?(:greeting)
      assert_equal 'Dashboard', I18n.t(:'admin.panel.title')
    end

    # deleted: tombstone masks the key and its whole subtree
    I18n.with_overlay(:en => { :greeting => DELETED, :admin => DELETED }) do
      assert_equal 'fallback', I18n.t(:greeting, :default => 'fallback')
      assert_raises(I18n::MissingTranslationData) { I18n.t(:greeting, :raise => true) }
      assert_raises(I18n::MissingTranslationData) { I18n.t(:'admin.panel.title', :raise => true) }
      assert !I18n.exists?(:'admin.panel.title')
    end

    assert_equal 'Hello', I18n.t(:greeting)
    assert_equal 'Dashboard', I18n.t(:'admin.panel.title')
  end

  test "interleaved fibers each see their own overlay on a shared backend" do
    backend = I18n.backend
    results = {}

    brand_a = Fiber.new do
      I18n.backend = backend
      I18n.with_overlay(:en => { :greeting => 'Brand A' }) do
        results[:a_inside_before_yield] = I18n.t(:greeting)
        Fiber.yield
        results[:a_inside_after_yield] = I18n.t(:greeting)
      end
      results[:a_after] = I18n.t(:greeting)
    end

    brand_b = Fiber.new do
      I18n.backend = backend
      I18n.with_overlay(:en => { :greeting => 'Brand B' }) do
        results[:b_inside] = I18n.t(:greeting)
        Fiber.yield
        results[:b_inside_later] = I18n.t(:greeting)
      end
      results[:b_after] = I18n.t(:greeting)
    end

    brand_a.resume # enters overlay A, reads, yields
    brand_b.resume # enters overlay B, reads, yields
    brand_a.resume # still inside overlay A, reads again, leaves block
    brand_b.resume # still inside overlay B, reads again, leaves block

    assert_equal 'Brand A', results[:a_inside_before_yield]
    assert_equal 'Brand A', results[:a_inside_after_yield]
    assert_equal 'Brand B', results[:b_inside]
    assert_equal 'Brand B', results[:b_inside_later]
    assert_equal 'Hello', results[:a_after]
    assert_equal 'Hello', results[:b_after]
    assert_equal 'Hello', I18n.t(:greeting)
  end

  test "cache does not serve one brand's result to another brand" do
    I18n.backend = CachedBackend.new
    store_translations(:en, { :greeting => 'Hello' })
    store = HashStore.new
    I18n.cache_store = store

    I18n.with_overlay(:en => { :greeting => 'Brand A' }) do
      assert_equal 'Brand A', I18n.t(:greeting)
    end
    I18n.with_overlay(:en => { :greeting => 'Brand B' }) do
      assert_equal 'Brand B', I18n.t(:greeting)
    end
    assert_equal 'Hello', I18n.t(:greeting)

    # separate cache entries for brand A, brand B and no overlay
    assert_equal 3, store.data.keys.size

    # equal overlay content reuses the cache entry and still returns
    # the correct brand
    I18n.with_overlay(:en => { :greeting => 'Brand A' }) do
      assert_equal 'Brand A', I18n.t(:greeting)
    end
    assert_equal 3, store.data.keys.size
  end

  test "cache stays brand-safe when two fibers interleave on a shared cached backend" do
    backend = CachedBackend.new
    backend.store_translations(:en, { :greeting => 'Hello' })
    store = HashStore.new
    I18n.cache_store = store
    results = {}

    brand_a = Fiber.new do
      I18n.backend = backend
      I18n.with_overlay(:en => { :greeting => 'Brand A' }) do
        Fiber.yield
        results[:a] = I18n.t(:greeting)
      end
    end

    brand_b = Fiber.new do
      I18n.backend = backend
      I18n.with_overlay(:en => { :greeting => 'Brand B' }) do
        Fiber.yield
        results[:b] = I18n.t(:greeting)
      end
    end

    brand_a.resume
    brand_b.resume
    brand_a.resume # populates/reads the cache while B's overlay is active too
    brand_b.resume

    assert_equal 'Brand A', results[:a]
    assert_equal 'Brand B', results[:b]
    assert_equal 'Hello', I18n.t(:greeting)
  end
end
