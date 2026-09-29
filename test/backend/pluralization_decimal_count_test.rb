require 'test_helper'

class I18nBackendPluralizationDecimalCountTest < I18n::TestCase
  class Backend < I18n::Backend::Simple
    include I18n::Backend::Pluralization
    include I18n::Backend::Fallbacks
  end

  LONG_DECIMAL = "0.123456789012345678901234567890123456789"

  def setup
    super
    I18n.backend = Backend.new

    # A rule that deliberately branches on the number of visible fraction
    # digits - information that only survives when the count is not coerced
    # to a Float on the way to the rule.
    @received = nil
    @rule = lambda do |count|
      @received = count
      case count.fraction_digits
      when 0 then :one
      when 1 then :few
      else :many
      end
    end

    store_translations(:xx, :i18n => { :plural => { :rule => @rule } })
    store_translations(:xx, :things => {
      :one   => '%{count} thing (integer)',
      :few   => '%{count} thing (1 fraction digit)',
      :many  => '%{count} thing (2+ fraction digits)',
      :other => '%{count} things'
    })
  end

  test "distinguishes text input 1, 1.0 and 1.00" do
    assert_equal '1 thing (integer)',           I18n.t(:things, :count => '1',    :locale => :xx)
    assert_equal '1.0 thing (1 fraction digit)',  I18n.t(:things, :count => '1.0',  :locale => :xx)
    assert_equal '1.00 thing (2+ fraction digits)', I18n.t(:things, :count => '1.00', :locale => :xx)
  end

  test "the rule receives an I18n::Count, not a Float" do
    I18n.t(:things, :count => '1.00', :locale => :xx)
    assert_instance_of I18n::Count, @received
    assert_equal '1.00', @received.to_s
  end

  test "interpolation uses the original decimal representation" do
    assert_equal '1.00 thing (2+ fraction digits)', I18n.t(:things, :count => '1.00', :locale => :xx)
  end

  test "negative counts keep their sign and visible fraction digits" do
    assert_equal '-2 thing (integer)',            I18n.t(:things, :count => '-2',    :locale => :xx)
    assert_equal '-2.5 thing (1 fraction digit)',   I18n.t(:things, :count => '-2.5',  :locale => :xx)
    assert_equal '-2.50 thing (2+ fraction digits)', I18n.t(:things, :count => '-2.50', :locale => :xx)
  end

  test "very long fractions reach the rule and the interpolation unshortened" do
    assert_equal "#{LONG_DECIMAL} thing (2+ fraction digits)", I18n.t(:things, :count => LONG_DECIMAL, :locale => :xx)
    assert_equal 39, @received.fraction_digits
    assert_equal Rational("123456789012345678901234567890123456789") / 10**39, @received.to_r
  end

  test "the count flows through fallbacks without being converted" do
    assert_equal '1.00 thing (2+ fraction digits)', I18n.t(:things, :count => '1.00', :locale => :'xx-XX')
    assert_instance_of I18n::Count, @received
    assert_equal 2, @received.fraction_digits
  end

  test "an I18n::Count can be passed directly" do
    assert_equal '1.0 thing (1 fraction digit)', I18n.t(:things, :count => I18n::Count.new('1.0'), :locale => :xx)
  end

  test "explicit 0 and 1 keys still take priority for decimal counts" do
    entry = { :"0" => 'none', :"1" => 'single', :other => 'other' }
    assert_equal 'none',   I18n.t(:count => '0.0',  :default => entry, :locale => :xx)
    assert_equal 'none',   I18n.t(:count => '-0',   :default => entry, :locale => :xx)
    assert_equal 'single', I18n.t(:count => '1',    :default => entry, :locale => :xx)
    assert_equal 'single', I18n.t(:count => '1.00', :default => entry, :locale => :xx)
  end

  test "legacy rules that expect a plain number still work via numeric delegation" do
    legacy_rule = lambda { |n| n % 10 == 1 && n % 100 != 11 ? :one : :other }
    store_translations(:yy, :i18n => { :plural => { :rule => legacy_rule } })
    store_translations(:yy, :things => { :one => 'one', :other => 'other' })

    assert_equal 'one',   I18n.t(:things, :count => '21',   :locale => :yy)
    assert_equal 'other', I18n.t(:things, :count => '21.5', :locale => :yy)
    assert_equal 'other', I18n.t(:things, :count => '111',  :locale => :yy)
  end

  test "legacy rules can unwrap the count through the compatibility entry" do
    unwrapping_rule = lambda { |n| I18n::Count.numeric(n) == 1 ? :one : :other }
    store_translations(:zz, :i18n => { :plural => { :rule => unwrapping_rule } })
    store_translations(:zz, :things => { :one => 'one', :other => 'other' })

    assert_equal 'one',   I18n.t(:things, :count => '1.00', :locale => :zz)
    assert_equal 'other', I18n.t(:things, :count => '2',    :locale => :zz)
  end
end
