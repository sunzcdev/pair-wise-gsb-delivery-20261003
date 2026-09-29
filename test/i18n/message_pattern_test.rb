# frozen_string_literal: true

require 'test_helper'

class I18nMessagePatternTest < I18n::TestCase
  def pattern(source)
    I18n::MessagePattern.new(source)
  end

  test "formats plain literal text" do
    assert_equal 'Hello world', pattern('Hello world').format
  end

  test "interpolates parameters" do
    assert_equal 'Hello Sven!', pattern('Hello {name}!').format(:name => 'Sven')
  end

  test "accepts string keys in values" do
    assert_equal 'Hello Sven!', pattern('Hello {name}!').format('name' => 'Sven')
  end

  test "raises MissingInterpolationArgument for a missing parameter" do
    exception = assert_raises(I18n::MissingInterpolationArgument) { pattern('Hi {name}').format }
    assert_equal :name, exception.key
  end

  test "select picks the branch matching the value" do
    select = pattern('{gender, select, male{He} female{She} other{They}}')
    assert_equal 'He', select.format(:gender => 'male')
    assert_equal 'She', select.format(:gender => :female)
  end

  test "select falls back to the other branch for unknown values" do
    select = pattern('{gender, select, male{He} female{She} other{They}}')
    assert_equal 'They', select.format(:gender => 'robot')
  end

  test "select requires an other branch" do
    exception = assert_raises(I18n::MessagePattern::SyntaxError) do
      pattern('{gender, select, male{He}}')
    end
    assert_match(/requires an 'other' branch/, exception.message)
  end

  test "plural picks a branch by plural category" do
    plural = pattern('{count, plural, one{One message} other{# messages}}')
    assert_equal 'One message', plural.format(:count => 1)
    assert_equal '2 messages', plural.format(:count => 2)
  end

  test "plural prefers exact =N branches over categories" do
    plural = pattern('{count, plural, =0{No messages} one{One message} other{# messages}}')
    assert_equal 'No messages', plural.format(:count => 0)
    assert_equal 'One message', plural.format(:count => 1)
    assert_equal '5 messages', plural.format(:count => 5)
  end

  test "plural uses a custom pluralizer when given" do
    plural = pattern('{count, plural, one{# file} few{# files} other{# files}}')
    russian = lambda { |n| n % 10 == 1 && n % 100 != 11 ? :one : (2..4).cover?(n % 10) && !(12..14).cover?(n % 100) ? :few : :other }
    assert_equal '1 file', plural.format({ :count => 1 }, :pluralizer => russian)
    assert_equal '3 files', plural.format({ :count => 3 }, :pluralizer => russian)
    assert_equal '5 files', plural.format({ :count => 5 }, :pluralizer => russian)
  end

  test "select and plural nest arbitrarily" do
    nested = pattern(
      '{gender, select, ' \
      'male{{count, plural, one{He has # car} other{He has # cars}}} ' \
      'female{{count, plural, one{She has # car} other{She has # cars}}} ' \
      'other{{count, plural, one{They have # car} other{They have # cars}}}}'
    )
    assert_equal 'He has 1 car', nested.format(:gender => :male, :count => 1)
    assert_equal 'She has 3 cars', nested.format(:gender => :female, :count => 3)
    assert_equal 'They have 2 cars', nested.format(:gender => :unknown, :count => 2)
  end

  test "# is literal outside of plural branches" do
    assert_equal 'use #hashtag', pattern('use #hashtag').format
    assert_equal 'use #hashtag', pattern('{g, select, a{use #hashtag} other{no}}').format(:g => :a)
  end

  test "single quotes escape literal braces" do
    assert_equal '{count} items', pattern(%q{'{count}' items}).format
    assert_equal 'a {literal} brace', pattern(%q{a '{'literal'}' brace}).format
  end

  test "two single quotes produce a literal apostrophe" do
    assert_equal "it's", pattern(%q{it''s}).format
  end

  test "a single quote before other characters is literal" do
    assert_equal "don't worry", pattern(%q{don't worry}).format
  end

  test "quoted text can contain braces and hash signs" do
    plural = pattern(%q!{count, plural, one{'{'} other{'# files'}}!)
    assert_equal '{', plural.format(:count => 1)
    assert_equal '# files', plural.format(:count => 2)
  end

  test "only the selected branch evaluates callable values" do
    calls = []
    select = pattern('{kind, select, a{A {value}} other{B {value}}}')
    value = lambda { |_values| calls << :called; 'X' }

    assert_equal 'A X', select.format(:kind => :a, :value => value)
    assert_equal 1, calls.size

    calls.clear
    assert_equal 'B X', select.format(:kind => :unknown, :value => value)
    assert_equal 1, calls.size
  end

  test "callables in unselected branches are never called" do
    called = false
    select = pattern('{kind, select, a{A {unused}} other{B}}')
    assert_equal 'B', select.format(:kind => :b, :unused => lambda { |_v| called = true; 'X' })
    assert_equal false, called
  end

  test "callable select keys are resolved to pick the branch" do
    select = pattern('{kind, select, a{A} other{B}}')
    assert_equal 'A', select.format(:kind => lambda { |_values| :a })
  end

  test "compile caches parsed patterns" do
    first = I18n::MessagePattern.compile('{count, plural, one{x} other{y}}')
    second = I18n::MessagePattern.compile('{count, plural, one{x} other{y}}')
    assert_same first, second
  end

  test "clear_cache! empties the compile cache" do
    cached = I18n::MessagePattern.compile('{a}')
    I18n::MessagePattern.clear_cache!
    refute_same cached, I18n::MessagePattern.compile('{a}')
  end

  test "patterns are frozen and safe to share" do
    assert pattern('{a}').frozen?
  end

  test "syntax error points at the exact position of an unclosed argument" do
    exception = assert_raises(I18n::MessagePattern::SyntaxError) { pattern('Hello {name') }
    assert_equal 11, exception.position
    assert_match(/\(line 1, column 12\)/, exception.message)
    assert_match(/Hello \{name\n +\^/, exception.message)
  end

  test "syntax error points at the exact line and column in multiline patterns" do
    exception = assert_raises(I18n::MessagePattern::SyntaxError) { pattern("line one\n{bad type}") }
    assert_equal 14, exception.position
    assert_match(/expected ',' or '\}' after argument name "bad" \(line 2, column 6\)/, exception.message)
    assert_match(/\{bad type\}\n +\^/, exception.message)
  end

  test "syntax error for an unknown argument type" do
    exception = assert_raises(I18n::MessagePattern::SyntaxError) { pattern('{n, bogus, a{x}}') }
    assert_match(/unknown argument type "bogus", expected 'select' or 'plural'/, exception.message)
  end

  test "syntax error for an unknown plural category" do
    exception = assert_raises(I18n::MessagePattern::SyntaxError) { pattern('{n, plural, single{x} other{y}}') }
    assert_match(/unknown plural category "single"/, exception.message)
  end

  test "syntax error for an exact match selector in a select" do
    exception = assert_raises(I18n::MessagePattern::SyntaxError) { pattern('{n, select, =1{x} other{y}}') }
    assert_match(/only allowed in plural arguments/, exception.message)
  end

  test "syntax error for an unmatched closing brace" do
    exception = assert_raises(I18n::MessagePattern::SyntaxError) { pattern('a}b') }
    assert_equal 1, exception.position
    assert_match(/unmatched '\}'/, exception.message)
  end

  test "syntax error for a duplicate selector" do
    exception = assert_raises(I18n::MessagePattern::SyntaxError) { pattern('{n, select, a{x} a{y} other{z}}') }
    assert_match(/duplicate selector :a/, exception.message)
  end
end
