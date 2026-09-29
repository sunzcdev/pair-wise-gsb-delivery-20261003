require 'test_helper'

class I18nBackendMessagePatternTest < I18n::TestCase
  class Backend < I18n::Backend::Simple
    include I18n::Backend::MessagePattern
  end

  def setup
    super
    I18n.backend = Backend.new
    I18n::MessagePattern.clear_cache
  end

  test "renders stored MessagePattern translations" do
    store_translations(:en, :inbox => I18n::MessagePattern.parse(
      "{count, plural, one{one message} other{# messages}}"))
    assert_equal "one message", I18n.t(:inbox, :count => 1)
    assert_equal "3 messages", I18n.t(:inbox, :count => 3)
  end

  test "renders stored MessagePattern translations without values" do
    store_translations(:en, :hint => I18n::MessagePattern.parse("no placeholders"))
    assert_equal "no placeholders", I18n.t(:hint)
  end

  test "treats strings as patterns only with message_pattern: true" do
    store_translations(:en, :inbox => "{count, plural, one{one message} other{# messages}}")
    assert_equal "2 messages", I18n.t(:inbox, :count => 2, :message_pattern => true)
  end

  test "caches patterns parsed through message_pattern: true" do
    store_translations(:en, :inbox => "{count, plural, one{one} other{#}}")
    I18n.t(:inbox, :count => 1, :message_pattern => true)
    assert_equal 1, I18n::MessagePattern.cache_size
    I18n.t(:inbox, :count => 2, :message_pattern => true)
    assert_equal 1, I18n::MessagePattern.cache_size
  end

  test "does not treat plain strings as patterns by default" do
    store_translations(:en, :inbox => "{count, plural, one{one message} other{# messages}}")
    assert_equal "{count, plural, one{one message} other{# messages}}", I18n.t(:inbox, :count => 2)
  end

  test "still interpolates plain strings with the regular syntax" do
    store_translations(:en, :greeting => "hello %{name}")
    assert_equal "hello world", I18n.t(:greeting, :name => "world")
  end

  test "combining count and gender in one message" do
    store_translations(:en, :notification => I18n::MessagePattern.parse(
      "{gender, select, " \
        "male{{count, plural, one{He has one message} other{He has # messages}}} " \
        "female{{count, plural, one{She has one message} other{She has # messages}}} " \
        "other{{count, plural, one{They have one message} other{They have # messages}}}}"))
    assert_equal "She has 2 messages", I18n.t(:notification, :gender => :female, :count => 2)
    assert_equal "They have one message", I18n.t(:notification, :gender => :other, :count => 1)
  end

  test "pattern syntax errors raise through message_pattern: true" do
    store_translations(:en, :broken => "{count, plural, one{x}")
    assert_raises(I18n::MessagePattern::SyntaxError) do
      I18n.t(:broken, :count => 1, :message_pattern => true)
    end
  end
end
