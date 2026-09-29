# encoding: utf-8
require 'test_helper'

class I18nBackendTransliteratorRuleSetTest < I18n::TestCase
  RuleSet = I18n::Backend::Transliterator::RuleSet

  def setup
    super
    I18n.backend = I18n::Backend::Simple.new
  end

  test "applies multi-character rules with longest match" do
    rules = RuleSet.new({ "a" => "X", "ab" => "Y", "abc" => "Z" })
    assert_equal "ZZYX", rules.transliterate("abcabcaba")
  end

  test "overlapping rules of the same prefix resolve by longest match" do
    rules = RuleSet.new({ "sch" => "sh", "ch" => "kh", "c" => "ts" })
    assert_equal "sholat", rules.transliterate("scholat", :none)
  end

  test "result does not depend on hash insertion order" do
    forward = RuleSet.new({ "a" => "X", "ab" => "Y", "abc" => "Z" })
    backward = RuleSet.new({ "abc" => "Z", "ab" => "Y", "a" => "X" })
    assert_equal forward.transliterate("abcab"), backward.transliterate("abcab")
    assert_equal forward.rules, backward.rules
  end

  test "result does not depend on pair order for array rules" do
    pairs = [["a", "X"], ["ab", "Y"], ["a", "W"]]
    assert_equal RuleSet.new(pairs).transliterate("ab"), RuleSet.new(pairs.reverse).transliterate("ab")
  end

  test "duplicate keys default to the lexicographically smallest replacement" do
    rules = RuleSet.new([["a", "b"], ["a", "a"]])
    assert_equal "a", rules.transliterate("a")
    assert_equal "a", RuleSet.new([["a", "a"], ["a", "b"]]).transliterate("a")
  end

  test "duplicate keys can be resolved with :first and :last" do
    assert_equal "1", RuleSet.new([["a", "1"], ["a", "2"]], :on_conflict => :first).transliterate("a")
    assert_equal "2", RuleSet.new([["a", "1"], ["a", "2"]], :on_conflict => :last).transliterate("a")
  end

  test "duplicate keys can raise with :raise" do
    assert_raises I18n::ArgumentError do
      RuleSet.new([["a", "1"], ["a", "2"]], :on_conflict => :raise)
    end
  end

  test "duplicate keys with identical replacements are not a conflict" do
    assert_nothing_raised do
      RuleSet.new([["a", "1"], ["a", "1"]], :on_conflict => :raise)
    end
  end

  test "supports one-to-many replacements" do
    rules = RuleSet.new({ "ü" => "ue", "ß" => "ss" })
    assert_equal "uess", rules.transliterate("üß")
  end

  test "supports deletion rules with empty replacements" do
    rules = RuleSet.new({ "h" => "", "l" => "L" })
    assert_equal "eLLo", rules.transliterate("hello", :none)
  end

  test "supports ascii rule keys" do
    rules = RuleSet.new({ "th" => "T" })
    assert_equal "Te", rules.transliterate("the", :none)
  end

  test "unknown characters are replaced with the replacement char by default" do
    rules = RuleSet.new({ "a" => "A" })
    assert_equal "A?", rules.transliterate("ab")
    assert_equal "A#", rules.transliterate("ab", "#")
  end

  test "unknown characters are kept when replacement is :none" do
    rules = RuleSet.new({ "a" => "A" })
    assert_equal "Ab", rules.transliterate("ab", :none)
  end

  test "unknown characters are kept with the :keep strategy" do
    rules = RuleSet.new({ "a" => "A" }, :unknown => :keep)
    assert_equal "Ab", rules.transliterate("ab")
  end

  test "unknown characters raise with the :raise strategy" do
    rules = RuleSet.new({ "a" => "A" }, :unknown => :raise)
    assert_raises(I18n::ArgumentError) { rules.transliterate("ab") }
    assert_equal "A", rules.transliterate("a")
  end

  test "default replacement char can be configured" do
    rules = RuleSet.new({ "a" => "A" }, :replacement => "!")
    assert_equal "A!", rules.transliterate("ab")
  end

  test "decomposed combining marks do not match composed keys without normalization" do
    rules = RuleSet.new({ "ü" => "ue" })
    refute_equal "ue", rules.transliterate([117, 776].pack("U*"), :none)
  end

  test "decomposed combining marks match composed keys with normalization" do
    rules = RuleSet.new({ "ü" => "ue" }, :normalize => :nfc)
    assert_equal "ue", rules.transliterate([117, 776].pack("U*"))
  end

  test "mapping tracks output fragments back to input ranges" do
    rules = RuleSet.new({ "ab" => "XYZ" }, :unknown => :keep)
    output, segments = rules.transliterate_with_map("ab!")
    assert_equal "XYZ!", output
    assert_equal 2, segments.length

    assert_equal "XYZ", segments[0].output
    assert_equal (0...2), segments[0].input_range
    assert_equal (0...3), segments[0].output_range

    assert_equal "!", segments[1].output
    assert_equal (2...3), segments[1].input_range
    assert_equal (3...4), segments[1].output_range
  end

  test "mapping records empty output ranges for deletion rules" do
    rules = RuleSet.new({ "h" => "" })
    output, segments = rules.transliterate_with_map("hi", :none)
    assert_equal "i", output
    assert_equal "", segments[0].output
    assert_equal (0...1), segments[0].input_range
    assert_equal (0...0), segments[0].output_range
    assert_equal (1...2), segments[1].input_range
  end

  test "mapping refers to the original string when normalizing" do
    input = "xu\u0308y" # "x", "u", COMBINING DIAERESIS, "y"
    rules = RuleSet.new({ "ü" => "ue" }, :normalize => :nfc, :unknown => :keep)
    output, segments = rules.transliterate_with_map(input)
    assert_equal "xuey", output
    assert_equal 3, segments.length

    ue = segments[1]
    assert_equal "ue", ue.output
    assert_equal (1...3), ue.input_range
    assert_equal "u\u0308", input[ue.input_range]
    assert_equal (1...3), ue.output_range
  end

  test "mapping joins multi-character input ranges for composed characters" do
    input = [117, 776].pack("U*") # "u" + COMBINING DIAERESIS
    rules = RuleSet.new({ "ü" => "ue" }, :normalize => :nfc)
    _, segments = rules.transliterate_with_map(input)
    assert_equal (0...2), segments[0].input_range
  end

  test "mapping is consistent with the plain transliterate output" do
    rules = RuleSet.new({ "ab" => "X", "c" => "" }, :normalize => :nfc)
    input = "abcu\u0308"
    output, segments = rules.transliterate_with_map(input, :none)
    assert_equal output, rules.transliterate(input, :none)
    assert_equal output, segments.map(&:output).join
  end

  test "rule keys must not be empty" do
    assert_raises(I18n::ArgumentError) { RuleSet.new({ "" => "x" }) }
  end

  test "unknown strategy must be valid" do
    assert_raises(I18n::ArgumentError) { RuleSet.new({}, :unknown => :bogus) }
  end

  test "conflict strategy must be valid" do
    assert_raises(I18n::ArgumentError) { RuleSet.new({}, :on_conflict => :bogus) }
  end

  test "normalization form must be valid" do
    assert_raises(I18n::ArgumentError) { RuleSet.new({}, :normalize => :bogus) }
  end

  test "rule keys and values are coerced to strings" do
    rules = RuleSet.new({ :a => 1 })
    assert_equal "1", rules.transliterate("a")
  end

  test "Transliterator.get returns a RuleSet as is" do
    rules = RuleSet.new({ "a" => "A" })
    assert_equal rules, I18n::Backend::Transliterator.get(rules)
  end

  test "transliteration rule can be a RuleSet" do
    rules = RuleSet.new({ "sch" => "sh" })
    store_translations(:xx, :i18n => { :transliterate => { :rule => rules } })
    assert_equal "sh", I18n.backend.transliterate(:xx, "sch")
  end
end
