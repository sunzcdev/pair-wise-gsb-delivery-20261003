# encoding: utf-8
require 'test_helper'

class I18nBackendTransliterator < I18n::TestCase
  def setup
    super
    I18n.backend = I18n::Backend::Simple.new
    @proc = lambda { |n| n.upcase }
    @hash = { "ü" => "ue", "ö" => "oe", "a" => "a" }
    @transliterator = I18n::Backend::Transliterator.get
  end

  test "transliteration rule can be a proc" do
    store_translations(:xx, :i18n => {:transliterate => {:rule => @proc}})
    assert_equal "HELLO", I18n.backend.transliterate(:xx, "hello")
  end

  test "transliteration rule can be a hash" do
    store_translations(:xx, :i18n => {:transliterate => {:rule => @hash}})
    assert_equal "ue", I18n.backend.transliterate(:xx, "ü")
  end

  test "transliteration rule must be a proc or hash" do
    store_translations(:xx, :i18n => {:transliterate => {:rule => ""}})
    assert_raises I18n::ArgumentError do
      I18n.backend.transliterate(:xx, "ü")
    end
  end

  test "transliterator defaults to latin => ascii when no rule is given" do
    assert_equal "AEroskobing", I18n.backend.transliterate(:xx, "Ærøskøbing")
  end

  test "default transliterator should not modify ascii characters" do
    (0..127).each do |byte|
      char = [byte].pack("U")
      assert_equal char, @transliterator.transliterate(char)
    end
  end

  test "default transliterator correctly transliterates latin characters" do
    # create string with range of Unicode's western characters with
    # diacritics, excluding the division and multiplication signs which for
    # some reason or other are floating in the middle of all the letters.
    string = (0xC0..0x17E).to_a.reject {|c| [0xD7, 0xF7].include? c}.append(0x1E9E).pack("U*")
    string.split(//) do |char|
      assert_match %r{^[a-zA-Z']*$}, @transliterator.transliterate(string)
    end
  end

  test "should replace non-ASCII chars not in map with a replacement char" do
    assert_equal "abc?", @transliterator.transliterate("abcſ")
  end

  test "can replace non-ASCII chars not in map with a custom replacement string" do
    assert_equal "abc#", @transliterator.transliterate("abcſ", "#")
  end

  test "can keep non-ASCII chars not in map with :none" do
    assert_equal "abcſ", @transliterator.transliterate("abcſ", :none)
  end

  test "default transliterator raises errors for invalid UTF-8" do
    assert_raises ArgumentError do
      @transliterator.transliterate("a\x92b")
    end
  end

  test "I18n.transliterate should transliterate using a default transliterator" do
    assert_equal "aeo", I18n.transliterate("áèö")
  end

  test "I18n.transliterate should transliterate using a locale" do
    store_translations(:xx, :i18n => {:transliterate => {:rule => @hash}})
    assert_equal "ue", I18n.transliterate("ü", :locale => :xx)
  end

  test "default transliterator fails with custom rules with uncomposed input" do
    char = [117, 776].pack("U*") # "ü" as ASCII "u" plus COMBINING DIAERESIS
    transliterator = I18n::Backend::Transliterator.get(@hash)
    refute_equal "ue", transliterator.transliterate(char)
  end

  test "DEFAULT_APPROXIMATIONS is frozen to prevent concurrency issues" do
    assert I18n::Backend::Transliterator::HashTransliterator::DEFAULT_APPROXIMATIONS.frozen?
  end

end

class I18nBackendTransliteratorRuleSetTest < I18n::TestCase
  RuleSet = I18n::Backend::Transliterator::RuleSet

  COMPOSED_U_DIAERESIS = "ü"   # "ü"
  DECOMPOSED_U_DIAERESIS = "ü" # "u" + COMBINING DIAERESIS

  def setup
    super
    I18n.backend = I18n::Backend::Simple.new
  end

  def assert_composed_and_decomposed
    assert_equal "ü", COMPOSED_U_DIAERESIS
    assert_equal "ü", DECOMPOSED_U_DIAERESIS
    refute_equal COMPOSED_U_DIAERESIS, DECOMPOSED_U_DIAERESIS
  end

  test "maps a group of characters to a single fragment" do
    transliterator = RuleSet.new({ "sch" => "sh" })
    assert_equal "sh", transliterator.transliterate("sch")
    assert_equal "shul", transliterator.transliterate("schul")
  end

  test "longest match wins over overlapping shorter rules" do
    transliterator = RuleSet.new({ "sch" => "sh", "ch" => "kh", "c" => "k" })
    assert_equal "sh", transliterator.transliterate("sch")
    assert_equal "akh", transliterator.transliterate("ach")
    assert_equal "k", transliterator.transliterate("c")
  end

  test "shorter rule applies where the longer overlapping rule does not match" do
    transliterator = RuleSet.new({ "ab" => "X", "b" => "Y" })
    assert_equal "X", transliterator.transliterate("ab")
    assert_equal "acY", transliterator.transliterate("acb")
  end

  test "supports one-to-many replacements" do
    transliterator = RuleSet.new({ "æ" => "ae", "ß" => "ss" })
    assert_equal "aess", transliterator.transliterate("æß")
  end

  test "supports deletion rules" do
    transliterator = RuleSet.new({ "̈" => "" })
    assert_equal "u", transliterator.transliterate("ü")
  end

  test "matches composed and decomposed input identically when normalizing" do
    assert_composed_and_decomposed
    transliterator = RuleSet.new({ COMPOSED_U_DIAERESIS => "ue" }, :normalize => :nfd)
    assert_equal "ue", transliterator.transliterate(COMPOSED_U_DIAERESIS)
    assert_equal "ue", transliterator.transliterate(DECOMPOSED_U_DIAERESIS)
  end

  test "passes ASCII characters without a rule through unchanged" do
    transliterator = RuleSet.new({}, :on_unknown => :error)
    assert_equal "abc ABC 123!", transliterator.transliterate("abc ABC 123!")
  end

  test "replaces unknown non-ASCII characters with the default replacement" do
    assert_equal "a?", RuleSet.new.transliterate("aſ")
  end

  test "replaces unknown non-ASCII characters with a configured replacement" do
    transliterator = RuleSet.new({}, :replacement => "#")
    assert_equal "a#", transliterator.transliterate("aſ")
    assert_equal "a~", transliterator.transliterate("aſ", "~")
  end

  test "keeps unknown characters when replacement is :none" do
    assert_equal "aſ", RuleSet.new.transliterate("aſ", :none)
  end

  test "keeps unknown characters with the :keep policy" do
    transliterator = RuleSet.new({}, :on_unknown => :keep)
    assert_equal "aſ", transliterator.transliterate("aſ")
  end

  test "raises on unknown characters with the :error policy" do
    transliterator = RuleSet.new({}, :on_unknown => :error)
    assert_raises(I18n::ArgumentError) { transliterator.transliterate("aſ") }
  end

  test "raises on conflicting rules with the :error policy" do
    assert_composed_and_decomposed
    assert_raises(I18n::ArgumentError) do
      RuleSet.new({ COMPOSED_U_DIAERESIS => "x", DECOMPOSED_U_DIAERESIS => "y" }, :normalize => :nfd)
    end
  end

  test "conflict resolution is deterministic and independent of insertion order" do
    assert_composed_and_decomposed
    forward = { COMPOSED_U_DIAERESIS => "x", DECOMPOSED_U_DIAERESIS => "y" }
    reverse = { DECOMPOSED_U_DIAERESIS => "y", COMPOSED_U_DIAERESIS => "x" }

    a = RuleSet.new(forward, :normalize => :nfd, :on_conflict => :first)
    b = RuleSet.new(reverse, :normalize => :nfd, :on_conflict => :first)
    assert_equal a.transliterate(COMPOSED_U_DIAERESIS), b.transliterate(COMPOSED_U_DIAERESIS)

    c = RuleSet.new(forward, :normalize => :nfd, :on_conflict => :last)
    d = RuleSet.new(reverse, :normalize => :nfd, :on_conflict => :last)
    assert_equal c.transliterate(COMPOSED_U_DIAERESIS), d.transliterate(COMPOSED_U_DIAERESIS)
    refute_equal a.transliterate(COMPOSED_U_DIAERESIS), c.transliterate(COMPOSED_U_DIAERESIS)
  end

  test "results do not depend on the insertion order of the rules hash" do
    assert_composed_and_decomposed
    rules = { "sch" => "sh", "ch" => "kh", "ü" => "ue", "æ" => "ae", "̈" => "" }
    sample = "schüæüſ ach"
    outputs = rules.to_a.permutation.map do |pairs|
      RuleSet.new(pairs.to_h, :normalize => :nfd).transliterate(sample)
    end
    assert_equal 1, outputs.uniq.length
    assert_equal "shueaeue? akh", outputs.first
  end

  test "transliterate_with_mapping maps output fragments to source ranges" do
    transliterator = RuleSet.new({ "sch" => "sh", "ü" => "ue" })
    output, fragments = transliterator.transliterate_with_mapping("schüx")
    assert_equal "shuex", output
    assert_equal ["sh", "ue", "x"], fragments.map(&:text)
    assert_equal [0...2, 2...4, 4...5], fragments.map(&:output_range)
    assert_equal [0...3, 3...4, 4...5], fragments.map(&:source_range)
  end

  test "mapping refers to the original string when normalizing composed input" do
    transliterator = RuleSet.new({ COMPOSED_U_DIAERESIS => "ue" }, :normalize => :nfd)
    output, fragments = transliterator.transliterate_with_mapping("a#{COMPOSED_U_DIAERESIS}b")
    assert_equal "aueb", output
    assert_equal [1...2], fragments.select { |f| f.text == "ue" }.map(&:source_range)
  end

  test "mapping refers to the original string when normalizing decomposed input" do
    transliterator = RuleSet.new({ COMPOSED_U_DIAERESIS => "ue" }, :normalize => :nfd)
    output, fragments = transliterator.transliterate_with_mapping("a#{DECOMPOSED_U_DIAERESIS}b")
    assert_equal "aueb", output
    assert_equal [1...3], fragments.select { |f| f.text == "ue" }.map(&:source_range)
  end

  test "deleted input produces no fragments but consumes source" do
    transliterator = RuleSet.new({ "̈" => "" })
    output, fragments = transliterator.transliterate_with_mapping("#{DECOMPOSED_U_DIAERESIS}x")
    assert_equal "ux", output
    assert_equal ["u", "x"], fragments.map(&:text)
    assert_equal [0...1, 2...3], fragments.map(&:source_range)
  end

  test "fragments tile the whole output" do
    transliterator = RuleSet.new({ "sch" => "sh", "̈" => "" }, :normalize => :nfd)
    output, fragments = transliterator.transliterate_with_mapping("sch#{DECOMPOSED_U_DIAERESIS}ſ")
    assert_equal output, fragments.map(&:text).join
    assert_equal output, transliterator.transliterate("sch#{DECOMPOSED_U_DIAERESIS}ſ")
  end

  test "can be used as a transliteration rule via Transliterator.get" do
    rule_set = RuleSet.new({ "sch" => "sh" })
    assert_equal rule_set, I18n::Backend::Transliterator.get(rule_set)
    store_translations(:xx, :i18n => { :transliterate => { :rule => rule_set } })
    assert_equal "sh", I18n.backend.transliterate(:xx, "sch")
  end

  test "raises on empty rule keys" do
    assert_raises(I18n::ArgumentError) { RuleSet.new({ "" => "x" }) }
  end

  test "raises on invalid options" do
    assert_raises(I18n::ArgumentError) { RuleSet.new({}, :on_unknown => :bogus) }
    assert_raises(I18n::ArgumentError) { RuleSet.new({}, :on_conflict => :bogus) }
    assert_raises(I18n::ArgumentError) { RuleSet.new({}, :normalize => :bogus) }
  end
end
