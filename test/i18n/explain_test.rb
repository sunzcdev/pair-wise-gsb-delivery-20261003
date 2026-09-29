# frozen_string_literal: true

require 'test_helper'

class I18nExplainApiTest < I18n::TestCase
  def setup
    super
    I18n.backend = I18n::Backend::Simple.new
    store_translations(:en, :foo => 'bar', :greet => 'Hi %{name}')
  end

  test "returns the translation and the explanation as a separate pair" do
    translation, explanation = I18n.explain(:foo, :locale => :en)

    assert_equal 'bar', translation
    assert_instance_of I18n::Explanation, explanation
    assert_equal :foo, explanation.key
    assert_equal :en, explanation.locale
  end

  test "hit points at the storage location the translation was read from" do
    _, explanation = I18n.explain(:foo, :locale => :en)

    assert explanation.hit?
    assert_equal(
      { :type => :lookup, :backend => I18n.backend, :locale => :en, :keys => [:en, :foo], :found => true },
      explanation.hit
    )
  end

  test "missing translation has no hit but keeps the miss trace" do
    translation, explanation = I18n.explain(:missing, :locale => :en)

    assert_equal 'Translation missing: en.missing', translation
    refute explanation.hit?
    assert explanation.steps.any? { |step| step[:type] == :lookup && step[:found] == false }
  end

  test "writes no global state and does not leak between calls" do
    _, first = I18n.explain(:foo, :locale => :en)
    size = first.steps.size

    I18n.t(:foo, :locale => :en)
    assert_equal size, first.steps.size

    _, second = I18n.explain(:foo, :locale => :en)
    refute_same first, second
    assert_equal first.steps, second.steps
  end

  test "does not execute interpolation callables for the sake of the explanation" do
    calls = 0
    name = lambda { |_values| calls += 1; 'Ruby' }

    translation, = I18n.explain(:greet, :locale => :en, :name => name)

    assert_equal 'Hi Ruby', translation
    assert_equal 1, calls
  end

  test "explain is a reserved key and never treated as an interpolation value" do
    assert I18n::RESERVED_KEYS.include?(:explain)
    translation, = I18n.explain(:greet, :locale => :en, :name => 'Ruby')
    assert_equal 'Hi Ruby', translation
  end
end

class I18nExplainTraceTest < I18n::TestCase
  module AccessCounter
    def accesses
      @accesses ||= []
    end

    def translate(locale, key, options = I18n::EMPTY_HASH)
      accesses << [:translate, locale, key]
      super
    end

    def lookup(locale, key, scope = [], options = I18n::EMPTY_HASH)
      accesses << [:lookup, locale, key, scope]
      super
    end
  end

  class CountingBackend < I18n::Backend::Simple
    include I18n::Backend::Fallbacks
    include I18n::Backend::Cascade
    prepend AccessCounter
  end

  def setup
    super
    @backend_1 = CountingBackend.new
    @backend_2 = CountingBackend.new
    I18n.backend = I18n::Backend::Chain.new(@backend_1, @backend_2)
    I18n.fallbacks = I18n::Locale::Fallbacks.new(:de => [:en])

    # Cross-language hit: requested in :de, only stored in :en.
    # Cross-level hit: requested as foo.bar.baz, only stored as foo.baz.
    # Symbol alias: foo.baz points to shared.label where the text lives.
    @backend_2.store_translations(:en, {
      :foo => { :baz => :'shared.label' },
      :shared => { :label => 'Hit (en)' }
    })
  end

  test "traces a cross-language, cross-level hit through chain, fallbacks, cascade and alias" do
    translation, explanation = I18n.explain(:'foo.bar.baz', :locale => :de, :cascade => true)

    assert_equal 'Hit (en)', translation
    assert_equal expected_steps, explanation.steps
    assert_equal(
      { :type => :lookup, :backend => @backend_2, :locale => :en, :keys => [:en, :shared, :label], :found => true },
      explanation.hit
    )
  end

  test "explain performs exactly the same backend accesses as a plain translate" do
    I18n.t(:'foo.bar.baz', :locale => :de, :cascade => true)
    plain_accesses_1 = @backend_1.accesses.dup
    plain_accesses_2 = @backend_2.accesses.dup

    @backend_1.accesses.clear
    @backend_2.accesses.clear

    I18n.explain(:'foo.bar.baz', :locale => :de, :cascade => true)

    assert_equal plain_accesses_1, @backend_1.accesses
    assert_equal plain_accesses_2, @backend_2.accesses

    expected_accesses = [
      [:translate, :de, :'foo.bar.baz'],
      [:lookup, :de, :'foo.bar.baz', nil],
      [:lookup, :en, :'foo.bar.baz', nil],
      [:translate, :de, :'shared.label'],
      [:lookup, :de, :'shared.label', nil],
      [:lookup, :en, :'shared.label', nil]
    ]
    assert_equal expected_accesses, @backend_1.accesses
    assert_equal expected_accesses, @backend_2.accesses
  end

  protected

  def expected_steps
    b1, b2 = @backend_1, @backend_2
    ch = ->(b, k) { { :type => :chain, :backend => b, :locale => :de, :key => k } }
    fb = ->(b, l, k) { { :type => :fallback, :backend => b, :locale => l, :original_locale => :de, :key => k } }
    tr = ->(b, l, k) { { :type => :translate, :backend => b, :locale => l, :key => k } }
    ca = ->(b, l, s, k) { { :type => :cascade, :backend => b, :locale => l, :scope => s, :key => k } }
    lo = ->(b, l, ks, f) { { :type => :lookup, :backend => b, :locale => l, :keys => ks, :found => f } }

    outer = :'foo.bar.baz'
    aliased = :'shared.label'
    [
      ch.(b1, outer),
      fb.(b1, :de, outer), tr.(b1, :de, outer),
      ca.(b1, :de, [:foo, :bar], 'baz'), lo.(b1, :de, [:de, :foo, :bar, :baz], false),
      ca.(b1, :de, [:foo], 'baz'),        lo.(b1, :de, [:de, :foo, :baz], false),
      ca.(b1, :de, [], 'baz'),            lo.(b1, :de, [:de, :baz], false),
      fb.(b1, :en, outer), tr.(b1, :en, outer),
      ca.(b1, :en, [:foo, :bar], 'baz'), lo.(b1, :en, [:en, :foo, :bar, :baz], false),
      ca.(b1, :en, [:foo], 'baz'),        lo.(b1, :en, [:en, :foo, :baz], false),
      ca.(b1, :en, [], 'baz'),            lo.(b1, :en, [:en, :baz], false),

      ch.(b2, outer),
      fb.(b2, :de, outer), tr.(b2, :de, outer),
      ca.(b2, :de, [:foo, :bar], 'baz'), lo.(b2, :de, [:de, :foo, :bar, :baz], false),
      ca.(b2, :de, [:foo], 'baz'),        lo.(b2, :de, [:de, :foo, :baz], false),
      ca.(b2, :de, [], 'baz'),            lo.(b2, :de, [:de, :baz], false),
      fb.(b2, :en, outer), tr.(b2, :en, outer),
      ca.(b2, :en, [:foo, :bar], 'baz'), lo.(b2, :en, [:en, :foo, :bar, :baz], false),
      ca.(b2, :en, [:foo], 'baz'),        lo.(b2, :en, [:en, :foo, :baz], true),

      { :type => :resolve, :backend => b2, :locale => :de, :key => aliased },

      ch.(b1, aliased),
      fb.(b1, :de, aliased), tr.(b1, :de, aliased),
      ca.(b1, :de, [:shared], 'label'), lo.(b1, :de, [:de, :shared, :label], false),
      ca.(b1, :de, [], 'label'),        lo.(b1, :de, [:de, :label], false),
      fb.(b1, :en, aliased), tr.(b1, :en, aliased),
      ca.(b1, :en, [:shared], 'label'), lo.(b1, :en, [:en, :shared, :label], false),
      ca.(b1, :en, [], 'label'),        lo.(b1, :en, [:en, :label], false),

      ch.(b2, aliased),
      fb.(b2, :de, aliased), tr.(b2, :de, aliased),
      ca.(b2, :de, [:shared], 'label'), lo.(b2, :de, [:de, :shared, :label], false),
      ca.(b2, :de, [], 'label'),        lo.(b2, :de, [:de, :label], false),
      fb.(b2, :en, aliased), tr.(b2, :en, aliased),
      ca.(b2, :en, [:shared], 'label'), lo.(b2, :en, [:en, :shared, :label], true)
    ]
  end
end
