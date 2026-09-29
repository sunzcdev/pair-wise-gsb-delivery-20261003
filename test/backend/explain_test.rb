require 'test_helper'

class I18nBackendExplainTest < I18n::TestCase
  # Counts every lookup the storage actually performs. Included below the
  # Cascade module so that every cascade level shows up as an individual
  # query, exactly like the trace records it.
  module LookupCounter
    def lookup(locale, key, scope = [], options = I18n::EMPTY_HASH)
      (@lookup_calls ||= []) << [locale, key, scope]
      super
    end
  end

  # A Simple backend with Fallbacks and Cascade that counts every lookup,
  # so the call trace reported by I18n.explain can be verified item by item
  # against the queries the backend actually received.
  class CountingBackend < I18n::Backend::Simple
    include LookupCounter
    include I18n::Backend::Fallbacks
    include I18n::Backend::Cascade

    attr_reader :lookup_calls

    def initialize
      @lookup_calls = []
      super()
    end
  end

  def setup
    super
    @first  = CountingBackend.new
    @second = CountingBackend.new
    @first.store_translations(:en, :unrelated => 'first')
    @second.store_translations(:en, :foo => { :baz => 'second hit %{name}' })
    I18n.backend = I18n::Backend::Chain.new(@first, @second)
    I18n.fallbacks = I18n::Locale::Fallbacks.new(:de => [:en])
  end

  def explain(key, **options)
    I18n.explain(key, **options)
  end

  test "returns the translation and the explanation separately" do
    result = explain(:unrelated, :locale => :en)

    assert_kind_of I18n::Explain::Result, result
    assert_equal 'first', result.translation
    assert_kind_of I18n::Explain::Trace, result.trace
    assert result.hit?
  end

  test "keeps no state between calls" do
    first  = explain(:unrelated, :locale => :en)
    second = explain(:unrelated, :locale => :en)

    refute_same first.trace, second.trace
    assert_equal first.trace.steps.size, second.trace.steps.size
  end

  test "explains a cross-locale, cross-level hit through a chain of two backends" do
    result = explain(:'foo.bar.baz', :locale => :de, :cascade => true, :name => 'X')

    assert_equal 'second hit X', result.translation

    # the chain consulted both backends in order
    chain_steps = result.trace.steps.select { |step| step.action == :chain }
    assert_equal [@first, @second], chain_steps.map(&:backend)
    assert_equal [false, true], chain_steps.map(&:hit?)

    # each backend tried the fallback locales :de, then :en
    fallback_steps = result.trace.steps.select { |step| step.action == :fallback }
    assert_equal [[@first, :de], [@first, :en], [@second, :de], [@second, :en]],
                 fallback_steps.map { |step| [step.backend, step.locale] }
    assert_equal [false, false, false, true], fallback_steps.map(&:hit?)

    # the winning backend cascaded from foo.bar.baz up to foo.baz
    cascade_steps = result.trace.steps.select { |step| step.action == :cascade && step.backend == @second && step.locale == :en }
    assert_equal [[:foo, :bar], [:foo]], cascade_steps.map { |step| step.detail[:scope] }
    assert_equal [false, true], cascade_steps.map(&:hit?)

    # the trace finally points at the storage location that was hit
    hit = result.trace.hit
    assert_equal :lookup, hit.action
    assert_equal @second, hit.backend
    assert_equal :en, hit.locale
    assert_equal [:en, :foo, :baz], hit.detail[:keys]
    assert_equal @second.send(:translations), hit.store
  end

  test "does not re-query backends to build the explanation" do
    result = explain(:'foo.bar.baz', :locale => :de, :cascade => true, :name => 'X')
    lookup_steps = result.trace.steps.select { |step| step.action == :lookup }

    # every query a backend actually received shows up as a :lookup step, once
    [@first, @second].each do |backend|
      steps = lookup_steps.select { |step| step.backend == backend }
      assert_equal backend.lookup_calls.size, steps.size
      assert_equal backend.lookup_calls.map { |locale, key, scope| I18n.normalize_keys(locale, key, scope) },
                   steps.map { |step| step.detail[:keys] }
    end

    # and a plain translate performs exactly the same queries
    counts_before = [@first, @second].map { |backend| backend.lookup_calls.size }
    I18n.t(:'foo.bar.baz', :locale => :de, :cascade => true, :name => 'X')
    counts_after = [@first, @second].map { |backend| backend.lookup_calls.size }
    assert_equal counts_before, counts_after.zip(counts_before).map { |after, before| after - before }
  end

  test "explains symbol aliases combined with chain, fallbacks and cascade" do
    @second.store_translations(:en, :foo => { :baz => :'other.key' }, :other => { :key => 'aliased %{name}' })

    result = explain(:'foo.bar.baz', :locale => :de, :cascade => true, :name => 'X')

    assert_equal 'aliased X', result.translation

    alias_steps = result.trace.steps.select { |step| step.action == :alias }
    assert_equal 1, alias_steps.size
    assert_equal :'other.key', alias_steps.first.key
    assert_equal @second, alias_steps.first.backend

    # the alias was followed through the chain and hit the second backend's store
    hit = result.trace.hit
    assert_equal @second, hit.backend
    assert_equal :en, hit.locale
    assert_equal [:en, :other, :key], hit.detail[:keys]
    assert_equal @second.send(:translations), hit.store

    # following the alias did not duplicate any backend query
    lookup_steps = result.trace.steps.select { |step| step.action == :lookup }
    assert_equal @first.lookup_calls.size + @second.lookup_calls.size, lookup_steps.size
  end

  test "does not execute interpolation callables to build the explanation" do
    calls = 0
    I18n.backend = I18n::Backend::Simple.new
    I18n.backend.store_translations(:en, :greet => 'hi %{who}')

    result = explain(:greet, :locale => :en, :who => lambda { |*| calls += 1; 'X' })

    assert_equal 'hi X', result.translation
    assert_equal 1, calls
  end

  test "explains a missing translation without raising" do
    result = explain(:missing, :locale => :de)

    assert_match(/translation missing/i, result.translation)
    refute result.hit?
    assert_nil result.trace.hit
    assert result.trace.steps.any? { |step| step.action == :lookup && !step.hit? }
  end
end
