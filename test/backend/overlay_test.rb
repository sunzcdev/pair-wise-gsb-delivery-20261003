require 'test_helper'

class I18nBackendOverlayTest < I18n::TestCase
  class Backend < I18n::Backend::Simple
    include I18n::Backend::Cache
    include I18n::Backend::Overlay
  end

  # Minimal cache store compatible with I18n::Backend::Cache.
  class MemoryStore
    attr_reader :writes

    def initialize
      @data = {}
      @writes = 0
    end

    def read(key)
      @data[key]
    end

    def write(key, value)
      @writes += 1
      @data[key] = value
    end

    def size
      @data.size
    end
  end

  DELETE = I18n::Backend::Overlay::DELETE

  def setup
    super
    I18n.backend = Backend.new
    I18n.cache_store = @cache_store = MemoryStore.new
    store_translations(:en,
      :greeting => 'Hello',
      :brand    => 'Default Brand',
      :secret   => 'backend secret',
      :nested   => { :a => 'A', :b => 'B' }
    )
    store_translations(:de, :greeting => 'Hallo')
  end

  def teardown
    I18n.cache_store = nil
    super
  end

  test "overlay overrides only the given keys, others fall through to the backend" do
    I18n.with_overlay(:en => { :greeting => 'Welcome to Acme' }) do
      assert_equal 'Welcome to Acme', I18n.t(:greeting)
      assert_equal 'Default Brand', I18n.t(:brand)
    end
  end

  test "context is restored after the block" do
    I18n.with_overlay(:en => { :greeting => 'Welcome to Acme' }) { }
    assert_equal 'Hello', I18n.t(:greeting)
    assert_nil I18n::Backend::Overlay.current
  end

  test "context is restored even when the block raises" do
    assert_raises(RuntimeError) do
      I18n.with_overlay(:en => { :greeting => 'Welcome to Acme' }) do
        assert_equal 'Welcome to Acme', I18n.t(:greeting)
        raise 'boom'
      end
    end
    assert_nil I18n::Backend::Overlay.current
    assert_equal 'Hello', I18n.t(:greeting)
  end

  test "does not swap or mutate the global backend" do
    backend = I18n.backend
    I18n.with_overlay(:en => { :greeting => 'Welcome to Acme' }) do
      assert_same backend, I18n.backend
    end
    assert_same backend, I18n.backend
  end

  test "overlay is read-only and cannot be mutated through lookup results" do
    I18n.with_overlay(:en => { :nested => { :a => 'A2' } }) do
      I18n.t(:nested)[:a] = 'hacked'
      assert_equal 'A2', I18n.t(:'nested.a')
    end
  end

  test "nested overlays deep-merge, inner wins, outer is restored" do
    I18n.with_overlay(:en => { :greeting => 'Outer', :brand => 'Outer Brand' }) do
      I18n.with_overlay(:en => { :greeting => 'Inner' }) do
        assert_equal 'Inner', I18n.t(:greeting)
        assert_equal 'Outer Brand', I18n.t(:brand)
        assert_equal 'backend secret', I18n.t(:secret)
      end
      assert_equal 'Outer', I18n.t(:greeting)
    end
    assert_equal 'Hello', I18n.t(:greeting)
  end

  test "overlay is scoped per locale" do
    I18n.with_overlay(:en => { :greeting => 'Welcome to Acme' }) do
      assert_equal 'Welcome to Acme', I18n.t(:greeting, :locale => :en)
      assert_equal 'Hallo', I18n.t(:greeting, :locale => :de)
    end
  end

  test "missing key falls through, nil blanks the key, DELETE removes it" do
    I18n.with_overlay(:en => { :greeting => nil, :secret => DELETE }) do
      # missing from the overlay: backend value
      assert_equal 'Default Brand', I18n.t(:brand)

      # nil: backend value suppressed, default still applies
      assert_nil I18n.t(:greeting, :default => nil)
      assert_equal 'fallback', I18n.t(:greeting, :default => 'fallback')
      assert_equal false, I18n.exists?(:greeting)

      # DELETE: backend value suppressed, reported missing
      assert_equal 'fallback', I18n.t(:secret, :default => 'fallback')
      assert_equal false, I18n.exists?(:secret)
      assert_raises(I18n::MissingTranslationData) { I18n.t(:secret, :raise => true) }
    end
  end

  test "nil keeps the key in subtrees, DELETE removes it" do
    I18n.with_overlay(:en => { :nested => { :a => nil, :b => DELETE } }) do
      assert_equal({ :a => nil }, I18n.t(:nested))
    end
  end

  test "subtree lookups deep-merge with the backend" do
    I18n.with_overlay(:en => { :nested => { :a => 'A2' } }) do
      assert_equal({ :a => 'A2', :b => 'B' }, I18n.t(:nested))
      assert_equal 'B', I18n.t(:'nested.b')
    end
  end

  test "cache does not serve one brand's translations to another brand" do
    with_brand_a = lambda { I18n.with_overlay(:en => { :greeting => 'Brand A' }) { I18n.t(:greeting) } }
    with_brand_b = lambda { I18n.with_overlay(:en => { :greeting => 'Brand B' }) { I18n.t(:greeting) } }

    # warm the cache in a fixed order, then re-read in a different order
    assert_equal 'Brand A', with_brand_a.call
    assert_equal 'Hello', I18n.t(:greeting)
    assert_equal 'Brand B', with_brand_b.call

    assert_equal 'Brand B', with_brand_b.call
    assert_equal 'Hello', I18n.t(:greeting)
    assert_equal 'Brand A', with_brand_a.call

    # identical overlays share cache entries, different ones do not
    assert_equal 3, @cache_store.size
  end

  test "interleaved fibers each see their own overlay" do
    results = {}

    fiber_a = Fiber.new do
      I18n.with_overlay(:en => { :greeting => 'Hi from A' }) do
        results[:a_first] = I18n.t(:greeting)
        Fiber.yield
        results[:a_second] = I18n.t(:greeting)
      end
    end

    fiber_b = Fiber.new do
      I18n.with_overlay(:en => { :greeting => 'Hi from B' }) do
        results[:b_first] = I18n.t(:greeting)
        Fiber.yield
        results[:b_second] = I18n.t(:greeting)
      end
    end

    fiber_a.resume # A enters its overlay, reads, yields inside the block
    fiber_b.resume # B enters its overlay, reads, yields inside the block
    fiber_a.resume # A must still see A's overlay, not B's
    fiber_b.resume

    assert_equal 'Hi from A', results[:a_first]
    assert_equal 'Hi from B', results[:b_first]
    assert_equal 'Hi from A', results[:a_second]
    assert_equal 'Hi from B', results[:b_second]
    assert_nil I18n::Backend::Overlay.current
  end

  test "interleaved fibers with exceptions still restore their context" do
    results = {}

    fiber_a = Fiber.new do
      I18n.with_overlay(:en => { :greeting => 'Hi from A' }) do
        Fiber.yield
        results[:a] = I18n.t(:greeting)
        raise 'boom'
      end
    end

    fiber_b = Fiber.new do
      I18n.with_overlay(:en => { :greeting => 'Hi from B' }) do
        Fiber.yield
        results[:b] = I18n.t(:greeting)
      end
    end

    fiber_a.resume
    fiber_b.resume
    assert_raises(RuntimeError) { fiber_a.resume }
    fiber_b.resume

    assert_equal 'Hi from A', results[:a]
    assert_equal 'Hi from B', results[:b]
    assert_nil I18n::Backend::Overlay.current
    assert_equal 'Hello', I18n.t(:greeting)
  end

  test "fiber spawned inside an overlay sees a snapshot but cannot leak back" do
    results = {}

    I18n.with_overlay(:en => { :greeting => 'Parent' }) do
      Fiber.new do
        results[:child_before] = I18n.t(:greeting)
        I18n.with_overlay(:en => { :greeting => 'Child' }) do
          results[:child_inner] = I18n.t(:greeting)
        end
        results[:child_after] = I18n.t(:greeting)
      end.resume
      results[:parent] = I18n.t(:greeting)
    end

    assert_equal 'Parent', results[:child_before]
    assert_equal 'Child', results[:child_inner]
    assert_equal 'Parent', results[:child_after]
    assert_equal 'Parent', results[:parent]
    assert_equal 'Hello', I18n.t(:greeting)
  end
end
