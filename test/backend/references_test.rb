require 'test_helper'

class I18nBackendReferencesTest < I18n::TestCase
  class ReferenceBackend < I18n::Backend::Simple
    include I18n::Backend::References
  end

  class FallbackReferenceBackend < I18n::Backend::Simple
    include I18n::Backend::Fallbacks
    include I18n::Backend::References
  end

  def setup
    super
    I18n.backend = ReferenceBackend.new
    @max_depth = I18n::Backend::References.max_depth
    @max_length = I18n::Backend::References.max_length
  end

  def teardown
    I18n::Backend::References.max_depth = @max_depth
    I18n::Backend::References.max_length = @max_length
    super
  end

  test "references: expands a marked fragment reference" do
    store_translations :en, :product => 'Acme', :tagline => 'Welcome to %{ref:product}'
    assert_equal 'Welcome to Acme', I18n.t(:tagline)
  end

  test "references: expands multiple references in a single message" do
    store_translations :en,
      :product => 'Acme',
      :slogan  => 'just do it',
      :tagline => '%{ref:product} says: %{ref:slogan}'
    assert_equal 'Acme says: just do it', I18n.t(:tagline)
  end

  test "references: the same key can be referenced repeatedly in one message" do
    store_translations :en, :product => 'Acme', :tagline => '%{ref:product} by %{ref:product}'
    assert_equal 'Acme by Acme', I18n.t(:tagline)
  end

  test "references: each reference can pass its own interpolation arguments" do
    store_translations :en,
      :greeting => 'Hello %{name}',
      :welcome  => '%{ref:greeting(name=Alice)} and %{ref:greeting(name=Bob)}'
    assert_equal 'Hello Alice and Hello Bob', I18n.t(:welcome)
  end

  test "references: reference arguments can use interpolation values of the enclosing translation" do
    store_translations :en,
      :greeting => 'Hello %{name}',
      :welcome  => '%{ref:greeting(name=%{user})}, welcome back %{user}'
    assert_equal 'Hello Bob, welcome back Bob', I18n.t(:welcome, :user => 'Bob')
  end

  test "references: integer reference arguments drive pluralization of the referenced entry" do
    store_translations :en,
      :inbox   => { :one => 'one message', :other => '%{count} messages' },
      :summary => 'Inbox: %{ref:inbox(count=1)} / %{ref:inbox(count=3)}'
    assert_equal 'Inbox: one message / 3 messages', I18n.t(:summary)
  end

  test "references: references are expanded in nested hashes and arrays" do
    store_translations :en,
      :product => 'Acme',
      :nested  => { :list => ['%{ref:product}', '%{ref:product}'] }
    assert_equal ['Acme', 'Acme'], I18n.t(:'nested.list')
  end

  test "references: references use the locale of the current translation" do
    store_translations :en, :greeting => 'Hello', :page => '%{ref:greeting}'
    store_translations :de, :greeting => 'Hallo', :page => '%{ref:greeting}'
    assert_equal 'Hallo', I18n.t(:page, :locale => :de)
    assert_equal 'Hello', I18n.t(:page, :locale => :en)
  end

  test "references: references resolve through the configured fallbacks" do
    I18n.backend = FallbackReferenceBackend.new
    I18n.fallbacks = I18n::Locale::Fallbacks.new(:de => [:en])
    store_translations :de, :welcome => 'Willkommen! %{ref:shared.farewell}'
    store_translations :en, :shared => { :farewell => 'Goodbye' }
    assert_equal 'Willkommen! Goodbye', I18n.t(:welcome, :locale => :de)
  end

  test "references: plain strings without the marker are left untouched" do
    store_translations :en, :plain => '100% certain {ref: not a reference} %{name}'
    assert_equal '100% certain {ref: not a reference} ok', I18n.t(:plain, :name => 'ok')
  end

  test "references: escaped references are not expanded" do
    store_translations :en, :product => 'Acme', :escaped => '%%{ref:product} %{name}'
    assert_equal '%{ref:product} ok', I18n.t(:escaped, :name => 'ok')
  end

  test "references: backends without the References module do not enable the syntax" do
    I18n.backend = I18n::Backend::Simple.new
    store_translations :en, :product => 'Acme', :tagline => 'Welcome to %{ref:product}'
    assert_equal 'Welcome to %{ref:product}', I18n.t(:tagline)
  end

  test "references: mutual references raise CircularReference reporting the chain" do
    store_translations :en, :a => 'A %{ref:b}', :b => 'B %{ref:a}'
    error = assert_raises(I18n::CircularReference) { I18n.t(:a) }
    assert_equal %w[a b a], error.chain
    assert_match 'circular reference detected: a -> b -> a', error.message
  end

  test "references: a self reference raises CircularReference" do
    store_translations :en, :loop => 'loop %{ref:loop}'
    error = assert_raises(I18n::CircularReference) { I18n.t(:loop) }
    assert_equal %w[loop loop], error.chain
  end

  test "references: a missing reference reports the chain and the underlying reason" do
    store_translations :en, :page => 'Say %{ref:missing.phrase}'
    error = assert_raises(I18n::MissingReference) { I18n.t(:page) }
    assert_equal 'missing.phrase', error.key
    assert_equal %w[page missing.phrase], error.chain
    assert_match 'missing reference "missing.phrase" in reference chain page -> missing.phrase', error.message
    assert_match 'Translation missing: en.missing.phrase', error.message
  end

  test "references: a missing nested reference reports the full chain" do
    store_translations :en, :outer => 'O %{ref:middle}', :middle => 'M %{ref:gone}'
    error = assert_raises(I18n::MissingReference) { I18n.t(:outer) }
    assert_equal %w[outer middle gone], error.chain
    assert_match 'outer -> middle -> gone', error.message
  end

  test "references: nesting deeper than max_depth raises ReferenceDepthExceeded" do
    I18n::Backend::References.max_depth = 2
    store_translations :en,
      :l1 => '%{ref:l2}', :l2 => '%{ref:l3}', :l3 => '%{ref:l4}', :l4 => 'end'
    error = assert_raises(I18n::ReferenceDepthExceeded) { I18n.t(:l1) }
    assert_equal %w[l1 l2 l3 l4], error.chain
    assert_match 'l1 -> l2 -> l3 -> l4', error.message
    assert_match 'maximum reference depth of 2', error.message
  end

  test "references: expansion beyond max_length raises ReferenceExpansionTooLong" do
    I18n::Backend::References.max_length = 20
    store_translations :en, :word => 'x' * 30, :page => 'word: %{ref:word}'
    error = assert_raises(I18n::ReferenceExpansionTooLong) { I18n.t(:page) }
    assert_equal %w[page word], error.chain
    assert_match 'maximum expansion length of 20', error.message
  end

  test "references: referencing a subtree raises InvalidReference" do
    store_translations :en, :namespace => { :inner => 'value' }, :page => '%{ref:namespace}'
    error = assert_raises(I18n::InvalidReference) { I18n.t(:page) }
    assert_match 'can not be embedded', error.message
    assert_match 'page -> namespace', error.message
  end

  test "references: malformed reference arguments raise InvalidReference" do
    store_translations :en, :greeting => 'Hello', :page => '%{ref:greeting(name)}'
    error = assert_raises(I18n::InvalidReference) { I18n.t(:page) }
    assert_match 'malformed reference argument', error.message
  end
end
