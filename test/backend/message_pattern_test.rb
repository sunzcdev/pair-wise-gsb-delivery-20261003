# frozen_string_literal: true

require 'test_helper'

class I18nBackendMessagePatternTest < I18n::TestCase
  class Backend < I18n::Backend::Simple
    include I18n::Backend::MessagePattern
  end

  class PluralizingBackend < I18n::Backend::Simple
    include I18n::Backend::Pluralization
    include I18n::Backend::MessagePattern
  end

  def setup
    super
    I18n.backend = Backend.new
  end

  test "formats message pattern translations" do
    store_translations(:en, :inbox => I18n::MessagePattern.new(
      '{count, plural, =0{No messages} one{One message} other{# messages}}'
    ))
    assert_equal 'No messages', I18n.t(:inbox, :count => 0)
    assert_equal 'One message', I18n.t(:inbox, :count => 1)
    assert_equal '3 messages', I18n.t(:inbox, :count => 3)
  end

  test "formats patterns given as defaults" do
    default = I18n::MessagePattern.new('{count, plural, one{# car} other{# cars}}')
    assert_equal '2 cars', I18n.t(:missing, :default => default, :count => 2)
  end

  test "formats patterns with nested selects through translate" do
    store_translations(:en, :notice => I18n::MessagePattern.new(
      '{gender, select, female{She has {count, plural, one{# invite} other{# invites}}} ' \
      'other{They have {count, plural, one{# invite} other{# invites}}}}'
    ))
    assert_equal 'She has 1 invite', I18n.t(:notice, :gender => :female, :count => 1)
    assert_equal 'They have 4 invites', I18n.t(:notice, :gender => :x, :count => 4)
  end

  test "plain string translations are not parsed as message patterns" do
    store_translations(:en, :not_a_pattern => '{count, plural, one{x} other{y}}')
    assert_equal '{count, plural, one{x} other{y}}', I18n.t(:not_a_pattern, :count => 1)
  end

  test "regular interpolation still works alongside patterns" do
    store_translations(:en,
      :greeting => 'Hello %{name}',
      :farewell => I18n::MessagePattern.new('Bye {name}'))
    assert_equal 'Hello Sven', I18n.t(:greeting, :name => 'Sven')
    assert_equal 'Bye Sven', I18n.t(:farewell, :name => 'Sven')
  end

  test "formats patterns even when no interpolation values are given" do
    store_translations(:en, :static => I18n::MessagePattern.new('just text'))
    assert_equal 'just text', I18n.t(:static)
  end

  test "uses the locale specific pluralization rule when Pluralization is included" do
    I18n.backend = PluralizingBackend.new
    rule = lambda { |n| n % 10 == 1 && n % 100 != 11 ? :one : (2..4).cover?(n % 10) && !(12..14).cover?(n % 100) ? :few : :other }
    store_translations(:xx, :i18n => { :plural => { :rule => rule } })
    store_translations(:xx, :files => I18n::MessagePattern.new(
      '{count, plural, one{# file} few{# files} other{# files}}'
    ))
    assert_equal '1 file', I18n.t(:files, :locale => :xx, :count => 1)
    assert_equal '3 files', I18n.t(:files, :locale => :xx, :count => 3)
    assert_equal '5 files', I18n.t(:files, :locale => :xx, :count => 5)
  end
end
