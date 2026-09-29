require 'test_helper'

class I18nBackendFragmentReferenceTest < I18n::TestCase
  class FallbackBackend < I18n::Backend::Simple
    include I18n::Backend::Fallbacks
  end

  def setup
    super
    I18n.backend = I18n::Backend::Simple.new
  end

  def teardown
    I18n::FragmentReference.max_depth = I18n::FragmentReference::DEFAULT_MAX_DEPTH
    I18n::FragmentReference.max_expansion_length = I18n::FragmentReference::DEFAULT_MAX_EXPANSION_LENGTH
    super
  end

  test "expands a single reference in a marked string" do
    store_translations :en,
      :product_name => 'Gadget',
      :promo => I18n.fragment('Deal of the day: ${ref:product_name}!')

    assert_equal 'Deal of the day: Gadget!', I18n.t(:promo, :locale => :en)
  end

  test "does not expand references in plain strings" do
    store_translations :en,
      :product_name => 'Gadget',
      :promo => 'Deal of the day: ${ref:product_name}!'

    assert_equal 'Deal of the day: ${ref:product_name}!', I18n.t(:promo, :locale => :en)
  end

  test "expands multiple references in one message, each with its own params" do
    store_translations :en,
      :greeting => 'Hello %{name}!',
      :farewell => 'Bye %{name}!',
      :dialog => I18n.fragment('${ref:greeting, name: Alice} ... ${ref:farewell, name: Bob}')

    assert_equal 'Hello Alice! ... Bye Bob!', I18n.t(:dialog, :locale => :en)
  end

  test "reference params support quoted strings, numbers and booleans" do
    store_translations :en,
      :template => '%{name}:%{count}:%{ratio}:%{flag}',
      :report => I18n.fragment('${ref:template, name: "Doe, John", count: 3, ratio: 1.5, flag: true}')

    assert_equal 'Doe, John:3:1.5:true', I18n.t(:report, :locale => :en)
  end

  test "reference params drive pluralization of the referenced entry" do
    store_translations :en,
      :items => { :one => '%{count} item', :other => '%{count} items' },
      :cart => I18n.fragment('You have ${ref:items, count: 2} and ${ref:items, count: 1}.')

    assert_equal 'You have 2 items and 1 item.', I18n.t(:cart, :locale => :en)
  end

  test "reference param values can interpolate values from the outer translation" do
    store_translations :en,
      :greeting => 'Hello %{name}!',
      :welcome => I18n.fragment('${ref:greeting, name: "%{user}"}')

    assert_equal 'Hello Carol!', I18n.t(:welcome, :locale => :en, :user => 'Carol')
  end

  test "expands nested fragment references" do
    store_translations :en,
      :product_name => 'Gadget',
      :tagline => I18n.fragment('${ref:product_name} - simply the best'),
      :promo => I18n.fragment('Deal: ${ref:tagline}!')

    assert_equal 'Deal: Gadget - simply the best!', I18n.t(:promo, :locale => :en)
  end

  test "expands repeated references to the same entry in one message" do
    store_translations :en,
      :product_name => 'Gadget',
      :promo => I18n.fragment('${ref:product_name} / ${ref:product_name}')

    assert_equal 'Gadget / Gadget', I18n.t(:promo, :locale => :en)
  end

  test "references use the locale of the current translation" do
    store_translations :en, :greeting => 'Hello'
    store_translations :de, :greeting => 'Hallo'
    store_translations :de, :welcome => I18n.fragment('${ref:greeting} %{name}')

    assert_equal 'Hallo Fred', I18n.t(:welcome, :locale => :de, :name => 'Fred')
  end

  test "references fall back to other locales when the referenced entry is missing" do
    I18n.backend = FallbackBackend.new
    I18n.fallbacks = I18n::Locale::Fallbacks.new(:de => [:de, :en])
    store_translations :en, :product_name => 'Gadget'
    store_translations :de, :promo => I18n.fragment('Angebot: ${ref:product_name}!')

    assert_equal 'Angebot: Gadget!', I18n.t(:promo, :locale => :de)
  end

  test "references prefer the current locale over fallback locales" do
    I18n.backend = FallbackBackend.new
    I18n.fallbacks = I18n::Locale::Fallbacks.new(:de => [:de, :en])
    store_translations :en, :product_name => 'Gadget'
    store_translations :de,
      :product_name => 'Gerät',
      :promo => I18n.fragment('Angebot: ${ref:product_name}!')

    assert_equal 'Angebot: Gerät!', I18n.t(:promo, :locale => :de)
  end

  test "mutual references raise a cycle error reporting the reference chain" do
    store_translations :en,
      :frag_a => I18n.fragment('A ${ref:frag_b}'),
      :frag_b => I18n.fragment('B ${ref:frag_a}')

    error = assert_raises(I18n::FragmentReferenceCycle) { I18n.t(:frag_a, :locale => :en) }
    assert_includes error.message, 'en.frag_a -> en.frag_b -> en.frag_a'
    assert_equal %w[en.frag_a en.frag_b en.frag_a], error.chain
  end

  test "a self reference raises a cycle error" do
    store_translations :en, :loop => I18n.fragment('${ref:loop}')

    error = assert_raises(I18n::FragmentReferenceCycle) { I18n.t(:loop, :locale => :en) }
    assert_includes error.message, 'en.loop -> en.loop'
  end

  test "a missing referenced translation raises an error with the reference chain" do
    store_translations :en, :promo => I18n.fragment('Deal: ${ref:no.such.key}!')

    error = assert_raises(I18n::MissingFragmentReference) { I18n.t(:promo, :locale => :en) }
    assert_includes error.message, 'en.promo -> en.no.such.key'
    assert_includes error.message, 'Translation missing: en.no.such.key'
  end

  test "a missing nested reference reports the full chain" do
    store_translations :en,
      :outer => I18n.fragment('${ref:middle}'),
      :middle => I18n.fragment('${ref:missing_key}')

    error = assert_raises(I18n::MissingFragmentReference) { I18n.t(:outer, :locale => :en) }
    assert_includes error.message, 'en.outer -> en.middle -> en.missing_key'
  end

  test "references nested deeper than max_depth raise an error with the chain" do
    I18n::FragmentReference.max_depth = 2
    store_translations :en,
      :chain1 => I18n.fragment('${ref:chain2}'),
      :chain2 => I18n.fragment('${ref:chain3}'),
      :chain3 => I18n.fragment('${ref:chain4}'),
      :chain4 => 'end'

    error = assert_raises(I18n::FragmentReferenceDepthExceeded) { I18n.t(:chain1, :locale => :en) }
    assert_includes error.message, 'maximum depth of 2'
    assert_includes error.message, 'en.chain1 -> en.chain2 -> en.chain3'
  end

  test "references within max_depth still expand" do
    I18n::FragmentReference.max_depth = 2
    store_translations :en,
      :chain1 => I18n.fragment('${ref:chain2}'),
      :chain2 => I18n.fragment('${ref:chain3}'),
      :chain3 => 'end'

    assert_equal 'end', I18n.t(:chain1, :locale => :en)
  end

  test "expansions longer than max_expansion_length raise an error with the chain" do
    I18n::FragmentReference.max_expansion_length = 10
    store_translations :en,
      :word => 'abcdef',
      :sentence => I18n.fragment('${ref:word} ${ref:word} ${ref:word}')

    error = assert_raises(I18n::FragmentReferenceExpansionTooLong) { I18n.t(:sentence, :locale => :en) }
    assert_includes error.message, 'maximum length of 10'
    assert_includes error.message, 'en.sentence'
  end

  test "a reference to a non-string translation raises an error with the chain" do
    store_translations :en,
      :section => { :title => 'Title' },
      :page => I18n.fragment('${ref:section}')

    error = assert_raises(I18n::FragmentReferenceError) { I18n.t(:page, :locale => :en) }
    assert_includes error.message, 'en.page -> section'
  end
end
