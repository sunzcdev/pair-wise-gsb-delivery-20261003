require 'test_helper'

class I18nLocaleCheckerTest < I18n::TestCase
  def setup
    super
    @dir = File.join(locales_dir, 'checker')
  end

  def check(**options)
    I18n::LocaleChecker.new(@dir, reference_locale: :en, **options).check
  end

  def issues_for(issues, locale, key)
    issues.select { |issue| issue.locale == locale && issue.key == key }
  end

  test "detects params missing and extra compared to the reference locale" do
    zh_welcome = issues_for(check, :zh, 'welcome')
    types = zh_welcome.group_by(&:type)

    assert_equal %i[user_name], types[:missing_params].first.params
    assert_equal %i[username], types[:extra_params].first.params
  end

  test "reports structured locations for both sides" do
    issue = issues_for(check, :zh, 'welcome').find { |i| i.type == :missing_params }

    assert_equal :zh, issue.locale
    assert_equal 'welcome', issue.key
    assert_nil issue.branch
    assert_match %r{checker/zh\.yml\z}, issue.location.file
    assert_equal 'welcome', issue.location.key
    assert_match %r{checker/en\.yml\z}, issue.reference_location.file
    assert_equal 'welcome', issue.reference_location.key
  end

  test "does not treat escaped percent signs as placeholders" do
    assert_empty issues_for(check, :zh, 'literal_percent')
    assert_empty issues_for(check, :de, 'literal_percent')
  end

  test "understands printf-style name placeholders" do
    assert_empty issues_for(check, :zh, 'formatted')
    assert_empty issues_for(check, :de, 'formatted')
  end

  test "compares plural branches per branch and reports branch differences" do
    issues = check

    zh_branch = issues_for(issues, :zh, 'inbox.messages').find { |i| i.type == :missing_branch }
    assert_equal :one, zh_branch.branch

    de_branch = issues_for(issues, :de, 'inbox.messages').find { |i| i.type == :extra_branch }
    assert_equal :few, de_branch.branch

    assert_empty issues_for(issues, :zh, 'inbox.messages').select { |i| i.branch == :other }
  end

  test "allows configuring legitimate branch differences per locale" do
    issues = check(allowed_branch_differences: {
      zh: [:one],
      de: { 'inbox.messages' => [:few] }
    })

    assert_empty issues.select { |i| %i[missing_branch extra_branch].include?(i.type) }
  end

  test "still reports param mismatches inside configured locales" do
    issues = check(allowed_branch_differences: { zh: [:one] })
    assert_equal 2, issues_for(issues, :zh, 'welcome').size
  end

  test "follows symbol aliases and gives the shared target a traceable location" do
    issues = check
    aliased = issues_for(issues, :de, 'alias_to_farewell')

    missing = aliased.find { |i| i.type == :missing_params }
    assert_equal %i[user_name], missing.params
    assert_equal 'shared.farewell', missing.alias_target
    assert_match %r{checker/de\.yml\z}, missing.alias_location.file
    assert_equal 'shared.farewell', missing.alias_location.key
    assert_equal 'alias_to_farewell', missing.location.key

    assert_equal 2, issues_for(issues, :de, 'second_alias_to_farewell').size

    target = issues_for(issues, :de, 'shared.farewell')
    assert_equal 2, target.size
    assert target.all? { |i| i.alias_target.nil? }
  end

  test "reports aliases that cannot be resolved" do
    issues = check

    %i[zh de].each do |locale|
      issue = issues_for(issues, locale, 'promo_code').find { |i| i.type == :unresolvable_alias }
      assert issue, "expected an unresolvable alias issue for #{locale}"
      assert_equal 'shared.missing', issue.alias_target
      assert_equal 'promo_code', issue.location.key
    end
  end

  test "only reads the directory and never evaluates ruby locale files" do
    checker = I18n::LocaleChecker.new(@dir, reference_locale: :en)

    assert_equal 1, checker.skipped_files.size
    assert_match %r{ignored\.rb\z}, checker.skipped_files.first
    assert_nothing_raised { checker.check }
  end

  test "extract_placeholders uses the interpolator patterns" do
    extract = I18n::LocaleChecker.method(:extract_placeholders)

    assert_equal %i[name], extract.call('Hello %{name}')
    assert_equal [], extract.call('100%%{name} sure')
    assert_equal %i[count], extract.call('%<count>d items')
    assert_equal %i[count], extract.call('%{count} and %{count} again')
  end
end
