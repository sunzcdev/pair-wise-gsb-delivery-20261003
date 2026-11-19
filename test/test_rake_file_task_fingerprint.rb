# frozen_string_literal: true
require File.expand_path("../helper", __FILE__)
require "fileutils"
require "json"

class TestRakeFileTaskFingerprint < Rake::TestCase # :nodoc:
  include Rake
  include RubyRunner

  SOURCE = "source.txt"
  OUTPUT = "output.txt"
  MANIFEST = "fingerprints.json"

  def setup
    super

    Task.clear
    Rake::FingerprintManifest.reset_cache
    @runs = 0
    delete_file SOURCE
    delete_file OUTPUT
    delete_file MANIFEST
  end

  def teardown
    Rake::FingerprintManifest.reset_cache
    Rake::FileTask.fingerprint_mode = false
    Rake::FileTask.fingerprint_manifest_path = nil

    super
  end

  def test_same_mtime_different_content_triggers_rebuild
    write_timed SOURCE, "one"
    build

    assert_equal 1, @runs
    refute Task[OUTPUT].needed?, "unchanged input and output must be up to date"

    # Replace the content while preserving the timestamps exactly.
    write_timed SOURCE, "two"

    assert Task[OUTPUT].needed?, "same mtime content replacement must be noticed"
    assert_equal :input_changed, Task[OUTPUT].fingerprint_status[:reason]

    rebuild

    assert_equal 2, @runs
    assert_equal "two", File.read(OUTPUT)
  end

  def test_timestamp_mode_is_still_the_default_and_can_be_selected
    write_timed SOURCE, "one"

    file OUTPUT => SOURCE do |t|
      @runs += 1
      File.write(t.name, File.read(t.source))
    end

    Task[OUTPUT].invoke
    Task[OUTPUT].reenable

    write_timed SOURCE, "two"

    refute Task[OUTPUT].needed?, "mtime mode cannot see same timestamp replacements"
    refute Task[OUTPUT].fingerprint_mode?
  end

  def test_fingerprint_mode_can_be_disabled_per_task
    Rake.application.options.fingerprint = true

    file OUTPUT => SOURCE, fingerprint: false do |t|
      @runs += 1
      File.write(t.name, File.read(t.source))
    end
    write_timed SOURCE, "one"
    Task[OUTPUT].invoke
    Task[OUTPUT].reenable

    write_timed SOURCE, "two"

    refute Task[OUTPUT].needed?, "explicit timestamp mode must keep mtime semantics"
  end

  def test_explicit_fingerprint_mode_wins_over_global_disable
    Rake.application.options.fingerprint = false

    file OUTPUT => SOURCE, fingerprint: true do |t|
      @runs += 1
      File.write(t.name, File.read(t.source))
    end

    assert Task[OUTPUT].fingerprint_mode?
  end

  def test_reusable_manifest_records_inputs_parameters_and_output_identity
    write_timed SOURCE, "one"
    build(parameters: { "flavor" => "debug" })

    manifest = read_manifest

    assert_equal [OUTPUT], manifest.keys
    entry = manifest[OUTPUT]

    assert_equal [SOURCE], entry["inputs"].keys
    assert_equal Digest::SHA256.hexdigest("one"), entry["inputs"][SOURCE]
    assert_equal Digest::SHA256.hexdigest("one"), entry["output"]["digest"]
    assert_equal 3, entry["output"]["size"]
    assert entry["parameters"]

    Task[OUTPUT].reenable

    refute Task[OUTPUT].needed?, "recorded manifest must be reusable"
    assert_equal 1, @runs
  end

  def test_parameter_change_triggers_rebuild_and_values_are_not_stored
    write_timed SOURCE, "one"
    build(parameters: { "secret" => "s3cr3t-value" })
    Task[OUTPUT].reenable

    refute Task[OUTPUT].needed?

    Task[OUTPUT].fingerprint_parameters = { "secret" => "other-value" }

    assert Task[OUTPUT].needed?
    assert_equal :parameters_changed, Task[OUTPUT].fingerprint_status[:reason]

    raw = File.read(MANIFEST)

    refute_includes raw, "s3cr3t-value"
    refute_includes raw, "other-value"
    refute_includes raw, "secret"
  end

  def test_environment_values_are_hashed_not_written
    ENV["RAKE_FINGERPRINT_TEST_SECRET"] = "env-secret-value"

    write_timed SOURCE, "one"
    build(parameters: -> {
      { "token" => ENV["RAKE_FINGERPRINT_TEST_SECRET"] }
    })

    raw = File.read(MANIFEST)

    refute_includes raw, "env-secret-value"
    assert_includes raw, "parameters"
  ensure
    ENV.delete "RAKE_FINGERPRINT_TEST_SECRET"
  end

  def test_replaced_output_triggers_rebuild
    write_timed SOURCE, "one"
    build
    Task[OUTPUT].reenable

    write_timed OUTPUT, "tampered"

    assert Task[OUTPUT].needed?
    assert_equal :output_changed, Task[OUTPUT].fingerprint_status[:reason]
  end

  def test_disappearing_source_triggers_rebuild
    write_timed SOURCE, "one"
    build
    Task[OUTPUT].reenable

    delete_file SOURCE

    assert Task[OUTPUT].needed?
    assert_equal :missing_input, Task[OUTPUT].fingerprint_status[:reason]
    assert_equal SOURCE, Task[OUTPUT].fingerprint_status[:detail]
  end

  def test_missing_output_triggers_rebuild
    write_timed SOURCE, "one"
    build
    Task[OUTPUT].reenable

    delete_file OUTPUT

    assert Task[OUTPUT].needed?
    assert_equal :missing_output, Task[OUTPUT].fingerprint_status[:reason]
  end

  def test_failed_action_does_not_register_success
    write_timed SOURCE, "one"

    file OUTPUT => SOURCE, fingerprint: { manifest: MANIFEST } do |t|
      @runs += 1
      File.write(t.name, "partial")
      fail "boom" if @runs == 1
    end

    assert_raises(RuntimeError) { Task[OUTPUT].invoke }

    manifest = read_manifest

    refute manifest.key?(OUTPUT), "failed build must not record a success fingerprint"
    refute File.exist?(MANIFEST) && read_manifest.key?(OUTPUT)

    Task[OUTPUT].reenable

    assert Task[OUTPUT].needed?
    assert_equal :no_fingerprint, Task[OUTPUT].fingerprint_status[:reason]

    Task[OUTPUT].invoke

    assert_equal 2, @runs
    assert read_manifest.key?(OUTPUT)
  end

  def test_failed_rebuild_invalidates_previous_success
    write_timed SOURCE, "one"
    build
    Task[OUTPUT].reenable

    Task[OUTPUT].clear_actions
    Task[OUTPUT].enhance do |t|
      @runs += 1
      fail "boom"
    end

    write_timed SOURCE, "two"

    assert_raises(RuntimeError) { Task[OUTPUT].invoke }

    refute read_manifest.key?(OUTPUT),
      "a failed rebuild must drop the earlier success fingerprint"
  end

  def test_manifest_only_covers_declared_scope
    write_timed SOURCE, "one"
    write_timed "unrelated.txt", "unrelated"

    build

    manifest = read_manifest

    assert_equal [SOURCE], manifest[OUTPUT]["inputs"].keys
    refute_includes File.read(MANIFEST), "unrelated.txt"
  end

  def test_removing_a_source_from_the_declaration_invalidates
    write_timed SOURCE, "one"
    write_timed "other.txt", "other"

    file OUTPUT, fingerprint: { inputs: [SOURCE, "other.txt"], manifest: MANIFEST } do |t|
      @runs += 1
      File.write(t.name, "built")
    end
    Task[OUTPUT].invoke
    Task[OUTPUT].reenable

    Task[OUTPUT].fingerprint_inputs = [SOURCE]

    assert Task[OUTPUT].needed?
    assert_equal :scope_changed, Task[OUTPUT].fingerprint_status[:reason]
  end

  def test_explicit_inputs_are_used_when_declared
    write_timed SOURCE, "one"
    write_timed "extra.txt", "extra"

    file OUTPUT, fingerprint: { inputs: [SOURCE, "extra.txt"], manifest: MANIFEST } do |t|
      @runs += 1
      File.write(t.name, "built")
    end

    Task[OUTPUT].invoke
    Task[OUTPUT].reenable

    assert_equal ["extra.txt", SOURCE], read_manifest[OUTPUT]["inputs"].keys.sort

    write_timed "extra.txt", "changed"

    assert Task[OUTPUT].needed?
    assert_equal :input_changed, Task[OUTPUT].fingerprint_status[:reason]
  end

  def test_dry_run_does_not_record_a_fingerprint
    write_timed SOURCE, "one"

    file OUTPUT => SOURCE, fingerprint: { manifest: MANIFEST } do |t|
      @runs += 1
      File.write(t.name, "one")
    end

    Rake.application.options.dryrun = true
    Task[OUTPUT].invoke

    assert_equal 0, @runs
    refute File.exist?(MANIFEST), "dry run must not claim a successful build"
  end

  def test_rule_synthesized_task_uses_declared_sources
    Rake.application.options.fingerprint = true
    Rake::FileTask.fingerprint_manifest_path = MANIFEST

    write_timed "a.src", "one"

    rule ".out" => ".src" do |t|
      @runs += 1
      File.write(t.name, File.read(t.source))
    end
    task :default => "a.out"

    Task["a.out"].invoke
    assert_equal 1, @runs
    Task["a.out"].reenable

    refute Task["a.out"].needed?

    write_timed "a.src", "two"

    assert Task["a.out"].needed?
    assert_equal :input_changed, Task["a.out"].fingerprint_status[:reason]

    Task["a.out"].invoke
    assert_equal 2, @runs
    assert_equal "two", File.read("a.out")
  end

  def test_manifest_records_entries_independently
    write_timed SOURCE, "one"
    build

    other = Rake::FingerprintManifest.for(MANIFEST)
    other.record("other.txt", {
      "scope" => [],
      "inputs" => {},
      "parameters" => nil,
      "output" => { "digest" => "x", "size" => 0 }
    })

    manifest = read_manifest

    assert_equal ["other.txt", OUTPUT], manifest.keys.sort
    assert_equal [SOURCE], manifest[OUTPUT]["inputs"].keys
  end

  def test_damaged_manifest_is_treated_as_empty
    write_timed SOURCE, "one"
    File.write(MANIFEST, "{not json")

    manifest = Rake::FingerprintManifest.for(MANIFEST)

    assert_equal({}, manifest.entries)
    assert_nil manifest.entry(OUTPUT)
  end

  def test_parameter_normalization_is_order_independent
    a = Rake::FingerprintManifest.digest_parameters("a" => 1, "b" => [2, 3])
    b = Rake::FingerprintManifest.digest_parameters("b" => [2, 3], "a" => 1)

    assert_equal a, b
    refute_equal a, Rake::FingerprintManifest.digest_parameters("a" => 1, "b" => [3, 2])
    assert_nil Rake::FingerprintManifest.digest_parameters(nil)
    assert_nil Rake::FingerprintManifest.digest_parameters({})
  end

  def test_directory_tasks_ignore_fingerprint_mode
    Rake.application.options.fingerprint = true

    directory "subdir"
    task = Task["subdir"]

    assert !task.fingerprint_mode?
    assert task.needed?
    task.invoke
    assert File.directory?("subdir")
    refute File.exist?(MANIFEST), "creation tasks must not record fingerprints"
  end

  def test_unchanged_content_with_newer_mtime_does_not_rebuild
    write_timed SOURCE, "one"
    build
    Task[OUTPUT].reenable

    # Touch without changing the content: timestamps would trigger, content
    # fingerprints do not.
    future = Time.now + 600
    File.utime(future, future, SOURCE)

    refute Task[OUTPUT].needed?
    assert_nil Task[OUTPUT].fingerprint_status[:reason]
  end

  def test_task_named_fingerprint_is_not_treated_as_an_option
    t = file fingerprint: ["dep"]
    assert_equal "fingerprint", t.name
    assert_equal ["dep"], t.prerequisites
    assert !t.fingerprint_mode?
  end

  def test_unknown_fingerprint_option_is_rejected
    error = assert_raises(ArgumentError) do
      file OUTPUT => SOURCE, fingerprint: { inputz: [SOURCE] }
    end

    assert_match(/inputz/, error.message)
  end

  def test_manifest_shows_up_changed_inputs
    write_timed SOURCE, "one"
    build
    Task[OUTPUT].reenable

    write_timed SOURCE, "two"

    Task[OUTPUT].needed?

    assert_equal [SOURCE], Task[OUTPUT].fingerprint_status[:detail]
  end

  def test_command_line_option_enables_fingerprint_mode
    rakefile <<~RAKEFILE
      file "out.txt" => "in.txt" do |t|
        File.write(t.name, File.read(t.source))
        puts "BUILT"
      end
      task :default => "out.txt"
    RAKEFILE
    write_timed "in.txt", "one"

    rake "--fingerprint"
    assert_match(/BUILT/, @out)
    assert File.exist?(Rake::FingerprintManifest::DEFAULT_PATH)

    write_timed "in.txt", "two"
    rake "--fingerprint"
    assert_match(/BUILT/, @out)

    write_timed "in.txt", "three"
    rake "--fingerprint", "--fingerprint-manifest=fp.json"
    assert_match(/BUILT/, @out)
    assert File.exist?("fp.json")

    write_timed "in.txt", "four"
    rake "--no-fingerprint", "--fingerprint-manifest=fp.json"
    refute_match(/BUILT/, @out,
      "--no-fingerprint must keep timestamp semantics for same timestamp content")

    write_timed "in.txt", "five", Time.now + 120
    rake
    assert_match(/BUILT/, @out, "plain timestamp mode is still the default")

    write_timed "in.txt", "six", Time.now + 180
    rake "--fingerprint", "--fingerprint-manifest=fp.json"
    assert_match(/BUILT/, @out, "the changed manifest path has no recorded entry")
  end

  def test_summary_is_reusable_across_processes
    rakefile <<~RAKEFILE
      file "out.txt" => "in.txt", fingerprint: { manifest: "fp.json" } do |t|
        File.write(t.name, File.read(t.source))
        puts "BUILT"
      end
      task :default => "out.txt"
    RAKEFILE
    write_timed "in.txt", "one"

    rake
    assert_match(/BUILT/, @out)

    rake
    refute_match(/BUILT/, @out)

    write_timed "in.txt", "two"
    rake
    assert_match(/BUILT/, @out)
    assert_equal "two", File.read("out.txt")

    rake
    refute_match(/BUILT/, @out)
  end

  private

  def build(parameters: nil)
    file_opts = { manifest: MANIFEST }
    file_opts[:parameters] = parameters if parameters

    file OUTPUT => SOURCE, fingerprint: file_opts do |t|
      @runs += 1
      File.write(t.name, File.read(t.source))
    end
    Task[OUTPUT].invoke
  end

  def rebuild
    Task[OUTPUT].reenable
    Task[OUTPUT].invoke
  end

  def read_manifest
    return {} unless File.exist?(MANIFEST)
    JSON.parse(File.read(MANIFEST))["entries"] || {}
  end

  # Write +content+ and pin the mtime to a fixed instant so that content
  # changes are invisible to timestamp based freshness checks.
  def write_timed(path, content, time=Time.at(1_500_000_000))
    FileUtils.mkdir_p(File.dirname(path))
    File.write(path, content)
    File.utime(time, time, path)
  end
end
