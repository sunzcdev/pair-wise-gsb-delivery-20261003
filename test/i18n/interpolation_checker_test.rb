# frozen_string_literal: true

require 'test_helper'
require 'tmpdir'
require 'fileutils'

class I18nInterpolationCheckerTest < I18n::TestCase
  def setup
    super
    @dir = Dir.mktmpdir('i18n-interpolation-checker')
  end

  def teardown
    FileUtils.remove_entry(@dir)
    super
  end

  test "detects mismatched interpolation parameter names" do
    write_locales(
      'en.yml' => { 'en' => { 'greeting' => 'Hello %{user_name}' } },
      'zh.yml' => { 'zh' => { 'greeting' => '你好 %{username}' } }
    )

    issues = checker.check_dir(@dir)

    assert_equal 1, issues.size
    issue = issues.first
    assert_equal :params, issue.kind
    assert_equal 'greeting', issue.key
    assert_equal :zh, issue.locale
    assert_equal :en, issue.reference_locale
    assert_nil issue.branch
    assert_equal %i[user_name], issue.missing
    assert_equal %i[username], issue.extra
    assert_equal File.join(@dir, 'zh.yml') + ':greeting', issue.source
    assert_equal File.join(@dir, 'en.yml') + ':greeting', issue.reference_source
  end

  test "ignores escaped percent signs and literals" do
    write_locales(
      'en.yml' => { 'en' => { 'progress' => '%<done>d%% of %{total} (%%{literal})' } },
      'de.yml' => { 'de' => { 'progress' => '%<done>d%% von %{total} (%%{literal})' } }
    )

    assert_equal [], checker.check_dir(@dir)
  end

  test "detects mismatch in sprintf style placeholders" do
    write_locales(
      'en.yml' => { 'en' => { 'progress' => '%<done>d percent' } },
      'de.yml' => { 'de' => { 'progress' => '%<completed>d Prozent' } }
    )

    issues = checker.check_dir(@dir)
    assert_equal 1, issues.size
    assert_equal %i[done], issues.first.missing
    assert_equal %i[completed], issues.first.extra
  end

  test "compares shared plural branches and ignores locale specific ones" do
    write_locales(
      'en.yml' => { 'en' => { 'inbox' => { 'messages' => {
        'one' => '%{count} message for %{name}',
        'other' => '%{count} messages for %{name}'
      } } } },
      'ru.yml' => { 'ru' => { 'inbox' => { 'messages' => {
        'one' => '%{count} сообщение для %{name}',
        'few' => '%{count} сообщения для %{name}',
        'many' => '%{count} сообщений для %{name}',
        'other' => '%{count} сообщения для %{name}'
      } } } }
    )

    assert_equal [], checker.check_dir(@dir)
  end

  test "reports branch specific mismatches with the branch name" do
    write_locales(
      'en.yml' => { 'en' => { 'inbox' => { 'messages' => {
        'one' => 'one message for %{name}',
        'other' => '%{count} messages for %{name}'
      } } } },
      'zh.yml' => { 'zh' => { 'inbox' => { 'messages' => {
        'other' => '%{count} 条消息'
      } } } }
    )

    issues = checker.check_dir(@dir)
    assert_equal 1, issues.size
    issue = issues.first
    assert_equal 'inbox.messages', issue.key
    assert_equal :other, issue.branch
    assert_equal %i[name], issue.missing
    assert_equal [], issue.extra
  end

  test "compares a plain string against the union of plural branches" do
    write_locales(
      'en.yml' => { 'en' => { 'apples' => { 'one' => '%{count} apple', 'other' => '%{count} apples' } } },
      'ja.yml' => { 'ja' => { 'apples' => 'りんご %{count} 個' } }
    )

    assert_equal [], checker.check_dir(@dir)
  end

  test "allows configured branch differences" do
    write_locales(
      'en.yml' => { 'en' => { 'inbox' => { 'messages' => {
        'one' => 'one message',
        'other' => '%{count} messages'
      } } } },
      'zh.yml' => { 'zh' => { 'inbox' => { 'messages' => {
        'other' => '%{count} 条消息，%{unread} 条未读'
      } } } }
    )

    allowed = { 'inbox.messages' => { other: { extra: %i[unread] } } }
    assert_equal [], checker(allowed_branch_differences: allowed).check_dir(@dir)

    issues = checker.check_dir(@dir)
    assert_equal 1, issues.size
    assert_equal %i[unread], issues.first.extra
  end

  test "allows configured branch differences as array shorthand" do
    write_locales(
      'en.yml' => { 'en' => { 'welcome' => 'Welcome %{name}' } },
      'fr.yml' => { 'fr' => { 'welcome' => 'Bienvenue' } }
    )

    allowed = { 'welcome' => { any: %i[name] } }
    assert_equal [], checker(allowed_branch_differences: allowed).check_dir(@dir)
  end

  test "resolves symbol aliases and reports the shared alias target location" do
    write_locales(
      'en.yml' => { 'en' => {
        'actions' => { 'cancel' => 'Cancel %{thing}', 'cancel_button' => :'actions.cancel' }
      } },
      'de.yml' => { 'de' => {
        'actions' => { 'cancel' => '%{object} abbrechen', 'cancel_button' => :'actions.cancel' }
      } }
    )

    issues = checker.check_dir(@dir)
    assert_equal 2, issues.size

    issue = issues.find { |i| i.key == 'actions.cancel_button' }
    assert issue, 'expected an issue for the aliased key'
    assert_equal %i[thing], issue.missing
    assert_equal %i[object], issue.extra
    assert_equal 'actions.cancel', issue.alias_of
    assert_equal File.join(@dir, 'de.yml') + ':actions.cancel', issue.alias_source

    target = issues.find { |i| i.key == 'actions.cancel' }
    assert target, 'expected an issue for the shared alias target itself'
    assert_equal File.join(@dir, 'de.yml') + ':actions.cancel', target.source
  end

  test "reports unresolved aliases without following cycles" do
    write_locales(
      'en.yml' => { 'en' => { 'a' => 'Hello %{name}' } },
      'de.yml' => { 'de' => { 'a' => :b, 'b' => :a } }
    )

    issues = checker.check_dir(@dir)
    assert_equal 1, issues.size
    issue = issues.first
    assert_equal :unresolved_alias, issue.kind
    assert_equal 'a', issue.key
    assert_equal :de, issue.locale
    assert_equal 'b', issue.alias_of
  end

  test "skips keys missing in other locales" do
    write_locales(
      'en.yml' => { 'en' => { 'only_english' => 'Hello %{name}' } },
      'de.yml' => { 'de' => { 'other' => 'Hallo' } }
    )

    assert_equal [], checker.check_dir(@dir)
  end

  test "reads json files and merges split files per locale" do
    write_locales(
      'en.yml' => { 'en' => { 'greeting' => 'Hello %{name}' } },
      'en.json' => { 'en' => { 'farewell' => 'Bye %{name}' } },
      'de.yml' => { 'de' => { 'greeting' => 'Hallo %{name}', 'farewell' => 'Tschüss %{name}' } }
    )

    assert_equal [], checker.check_dir(@dir)
  end

  test "ignores ruby locale files and never calls callables" do
    write_locales(
      'en.yml' => { 'en' => { 'greeting' => 'Hello %{name}' } },
      'de.yml' => { 'de' => { 'greeting' => 'Hallo %{name}' } }
    )
    File.write(File.join(@dir, 'evil.rb'), 'raise "must never be evaluated"')

    assert_equal [], checker.check_dir(@dir)
  end

  test "skips non string values" do
    write_locales(
      'en.yml' => { 'en' => { 'count' => 5, 'enabled' => true, 'greeting' => 'Hi %{name}' } },
      'de.yml' => { 'de' => { 'count' => 5, 'enabled' => true, 'greeting' => 'Hallo %{name}' } }
    )

    assert_equal [], checker.check_dir(@dir)
  end

  test "raises when the reference locale is missing" do
    write_locales('de.yml' => { 'de' => { 'greeting' => 'Hallo' } })

    assert_raises(I18n::ArgumentError) { checker.check_dir(@dir) }
  end

  test "issues expose a structured hash" do
    write_locales(
      'en.yml' => { 'en' => { 'greeting' => 'Hello %{user_name}' } },
      'zh.yml' => { 'zh' => { 'greeting' => '你好 %{username}' } }
    )

    issue = checker.check_dir(@dir).first
    hash = issue.to_h
    assert_equal 'greeting', hash[:key]
    assert_equal :zh, hash[:locale]
    assert_equal %i[user_name], hash[:missing]
    assert_equal %i[username], hash[:extra]
    assert_match(/user_name/, issue.to_s)
  end

  protected

  def checker(options = {})
    I18n::InterpolationChecker.new(reference_locale: :en, **options)
  end

  def write_locales(files)
    files.each do |name, data|
      path = File.join(@dir, name)
      if name.end_with?('.json')
        File.write(path, JSON.generate(data))
      else
        File.write(path, YAML.dump(data))
      end
    end
  end
end
