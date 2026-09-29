require 'test_helper'

class I18nMessagePatternTest < I18n::TestCase
  def parse(source)
    I18n::MessagePattern.parse(source)
  end

  def setup
    super
    I18n::MessagePattern.clear_cache
  end

  def teardown
    I18n::MessagePattern.plural_rule = I18n::MessagePattern::DEFAULT_PLURAL_RULE
    super
  end

  test "renders plain text unchanged" do
    assert_equal "hello world", parse("hello world").render
  end

  test "interpolates parameters" do
    assert_equal "hello world", parse("hello {name}").render(:name => "world")
  end

  test "accepts string keys as well as symbol keys" do
    assert_equal "hello world", parse("hello {name}").render("name" => "world")
  end

  test "raises MissingInterpolationArgument for missing parameters" do
    assert_raises(I18n::MissingInterpolationArgument) { parse("hello {name}").render }
  end

  test "calls callable values when rendered" do
    assert_equal "hello world", parse("hello {name}").render(:name => ->(_values) { "world" })
  end

  test "selects a branch by value" do
    pattern = parse("{gender, select, male{he} female{she} other{they}}")
    assert_equal "he", pattern.render(:gender => :male)
    assert_equal "she", pattern.render(:gender => "female")
  end

  test "falls back to the other branch when no select key matches" do
    pattern = parse("{gender, select, male{he} female{she} other{they}}")
    assert_equal "they", pattern.render(:gender => :nonbinary)
  end

  test "selects a plural branch by category" do
    pattern = parse("{count, plural, one{one message} other{# messages}}")
    assert_equal "one message", pattern.render(:count => 1)
    assert_equal "5 messages", pattern.render(:count => 5)
  end

  test "prefers exact =n plural branches over categories" do
    pattern = parse("{count, plural, =0{no messages} one{one message} other{# messages}}")
    assert_equal "no messages", pattern.render(:count => 0)
    assert_equal "one message", pattern.render(:count => 1)
    assert_equal "2 messages", pattern.render(:count => 2)
  end

  test "supports a custom plural rule" do
    I18n::MessagePattern.plural_rule = ->(count) { count.zero? ? :zero : :other }
    pattern = parse("{count, plural, zero{nothing} other{# things}}")
    assert_equal "nothing", pattern.render(:count => 0)
    assert_equal "1 things", pattern.render(:count => 1)
  end

  test "renders # literally outside of plural branches" do
    assert_equal "hash # tag", parse("hash # tag").render
  end

  test "nests placeholders inside branches" do
    pattern = parse(
      "{gender, select, " \
        "male{{count, plural, one{He has one message} other{He has # messages}}} " \
        "female{{count, plural, one{She has one message} other{She has # messages}}} " \
        "other{{count, plural, one{They have one message} other{They have # messages}}}}"
    )
    assert_equal "He has one message", pattern.render(:gender => :male, :count => 1)
    assert_equal "She has 3 messages", pattern.render(:gender => :female, :count => 3)
    assert_equal "They have 2 messages", pattern.render(:gender => :unknown, :count => 2)
  end

  test "nests parameters inside deeply nested branches" do
    pattern = parse("{a, select, x{{b, select, y{{who} wins} other{nobody}}} other{none}}")
    assert_equal "codex wins", pattern.render(:a => "x", :b => "y", :who => "codex")
    assert_equal "nobody", pattern.render(:a => "x", :b => "z")
    assert_equal "none", pattern.render(:a => "q")
  end

  test "does not evaluate unselected branches" do
    evaluated = false
    pattern = parse("{kind, select, a{fine} other{{explode}}}")
    assert_equal "fine", pattern.render(:kind => "a", :explode => ->(_v) { evaluated = true })
    assert_equal false, evaluated
  end

  test "does not require missing keys of unselected branches" do
    pattern = parse("{kind, select, a{fine} other{{missing}}}")
    assert_nothing_raised { pattern.render(:kind => "a") }
  end

  test "escapes literal braces and backslashes" do
    assert_equal "a {brace} here", parse('a \{brace\} here').render
    assert_equal 'a \ backslash', parse('a \\\\ backslash').render
    assert_equal "not a {placeholder}", parse('not a \{placeholder\}').render
  end

  test "allows whitespace around placeholder tokens" do
    assert_equal " hi there", parse("{ name , select , x{ hi } other{ ho } }{suffix}").render(:name => "x", :suffix => "there")
  end

  test "caches parsed patterns by source" do
    assert_same I18n::MessagePattern.parse("hi {name}"), I18n::MessagePattern.parse("hi {name}")
  end

  test "new instances are not cached but parse is" do
    I18n::MessagePattern.parse("hi {name}")
    assert_equal 1, I18n::MessagePattern.cache_size
    I18n::MessagePattern.new("hi {name}")
    assert_equal 1, I18n::MessagePattern.cache_size
  end

  test "syntax errors point at the exact position" do
    error = assert_raises(I18n::MessagePattern::SyntaxError) do
      parse("{count, plural, one{x}")
    end
    assert_equal 22, error.position
    assert_equal 1, error.line
    assert_equal 23, error.column
    assert_includes error.message, "unexpected end of input, expected a branch key or `}` at line 1, column 23"
    assert_includes error.message, "{count, plural, one{x}\n                      ^"
  end

  test "syntax errors report line and column of later lines" do
    error = assert_raises(I18n::MessagePattern::SyntaxError) do
      parse("line one\nline } two")
    end
    assert_equal 2, error.line
    assert_equal 6, error.column
    assert_includes error.message, "line } two\n     ^"
  end

  test "unclosed brace is a syntax error" do
    assert_raises(I18n::MessagePattern::SyntaxError) { parse("hello {name") }
  end

  test "lone closing brace is a syntax error" do
    error = assert_raises(I18n::MessagePattern::SyntaxError) { parse("hello }") }
    assert_equal 6, error.position
    assert_includes error.message, "unexpected `}`"
  end

  test "unknown placeholder type is a syntax error" do
    error = assert_raises(I18n::MessagePattern::SyntaxError) { parse("{n, choice, other{x}}") }
    assert_includes error.message, "unknown placeholder type `choice`"
  end

  test "missing other branch is a syntax error" do
    error = assert_raises(I18n::MessagePattern::SyntaxError) { parse("{n, select, a{x}}") }
    assert_includes error.message, "missing required `other` branch for `n`"
  end

  test "unknown plural category is a syntax error" do
    error = assert_raises(I18n::MessagePattern::SyntaxError) { parse("{n, plural, single{x} other{y}}") }
    assert_includes error.message, "unknown plural category `single`"
    assert_equal 12, error.position
  end

  test "duplicate branches are a syntax error" do
    error = assert_raises(I18n::MessagePattern::SyntaxError) { parse("{n, select, a{x} a{y} other{z}}") }
    assert_includes error.message, "duplicate branch `a`"
  end

  test "dangling escape is a syntax error" do
    error = assert_raises(I18n::MessagePattern::SyntaxError) { parse('ends with\\') }
    assert_includes error.message, "dangling escape"
  end

  test "empty placeholder name is a syntax error" do
    error = assert_raises(I18n::MessagePattern::SyntaxError) { parse("{, select, a{x} other{y}}") }
    assert_includes error.message, "expected a placeholder name"
  end
end
