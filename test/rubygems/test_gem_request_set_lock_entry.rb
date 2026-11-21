# frozen_string_literal: true

require_relative "helper"
require "rubygems/request_set"
require "rubygems/request_set/lock_entry"
require "rubygems/request_set/lockfile"

class TestGemRequestSetLockEntry < Gem::TestCase
  def setup
    super

    Gem::RemoteFetcher.fetcher = @fetcher = Gem::FakeFetcher.new
    @gem_deps_file = File.join @tempdir, "gem.deps.rb"
  end

  def lockfile(entries, dependencies = nil)
    Gem::RequestSet::Lockfile.from_entries entries, @gem_deps_file, dependencies
  end

  def test_from_activated_records_the_activated_version_only
    v1 = util_spec "tool", "1.0.0"
    v2 = util_spec "tool", "2.0.0"
    install_gem v1
    install_gem v2
    Gem::Specification.reset

    dependency = dep "tool", "= 1.0.0"
    dependency.to_spec.activate

    entries = Gem::RequestSet.new.lock_entries

    assert_equal %w[tool], entries.map(&:name)
    entry = entries.first
    assert_equal v1.version, entry.version
    assert_equal [v2.version], entry.other_installed_versions
    assert_predicate entry, :divergent?
  end

  def test_from_activated_does_not_activate_other_installed_version
    v1 = util_spec "tool", "1.0.0"
    v2 = util_spec "tool", "2.0.0"
    install_gem v1
    install_gem v2
    Gem::Specification.reset

    dep("tool", "= 2.0.0").to_spec.activate

    entries = Gem::RequestSet.new.lock_entries

    assert_equal [v1.version, v2.version].sort, entries.first.installed_versions.sort
    assert_equal [v2.version], entries.map(&:version)
    assert_equal [v1.version], entries.first.other_installed_versions
  end

  def test_installed_but_unactivated_version_gets_no_row
    v1 = util_spec "tool", "1.0.0"
    install_gem v1

    v2 = util_spec "tool", "2.0.0"
    install_gem v2
    Gem::Specification.reset

    # Only 1.0.0 is activated; 2.0.0 is merely installed.
    dep("tool", "= 1.0.0").to_spec.activate
    refute_includes Gem.loaded_specs.values.map(&:full_name), "tool-2.0.0"

    entries = Gem::RequestSet.new.lock_entries

    assert_equal 1, entries.length
    assert_equal v1.version, entries.first.version

    output = Gem::RequestSet.new.lockfile_from_activated(@gem_deps_file).to_s
    refute_includes output, "tool (2.0.0)"
    assert_equal 1, output.scan(/^    tool \(/).length
  end

  def test_lockfile_pins_the_activated_version
    v1 = util_spec "tool", "1.0.0"
    v2 = util_spec "tool", "2.0.0"
    install_gem v1
    install_gem v2
    Gem::Specification.reset

    dep("tool", "= 1.0.0").to_spec.activate

    requests = Gem::RequestSet.new
    output = requests.lockfile_from_activated(@gem_deps_file).to_s

    assert_includes output, "    tool (1.0.0)\n"
    refute_includes output, "tool (2.0.0)"
    assert_includes output, "DEPENDENCIES\n  tool (= 1.0.0)\n"
  end

  def test_lock_entries_preserve_the_native_platform
    spec = util_spec "native", "3.0" do |s|
      s.platform = Gem::Platform.local
    end
    install_gem spec
    Gem::Specification.reset

    Gem::Specification.find_all_by_name("native").first.activate

    entry = Gem::RequestSet.new.lock_entries.first

    assert_equal Gem::Platform.local, entry.platform
    refute_equal Gem::Platform::RUBY, entry.platform

    output = Gem::RequestSet.new.lockfile_from_activated(@gem_deps_file).to_s

    assert_includes output, "    native (3.0-#{Gem::Platform.local})\n"
    assert_includes output, "PLATFORMS\n  #{Gem::Platform.local}\n"

    assert_equal [Gem::Platform.local],
                 Gem::RequestSet.new.lockfile_from_activated(@gem_deps_file).platforms
  end

  def test_lock_entries_preserve_the_remote
    spec_fetcher do |fetcher|
      fetcher.spec "a", 2
    end

    source = Gem::Source.new @gem_repo
    index_spec = Gem::Resolver::IndexSpecification.new(
      nil, "a", Gem::Version.new(2), source, Gem::Platform::RUBY
    )

    entry = Gem::RequestSet.new.lock_entries([index_spec]).first

    assert_equal :gem, entry.source_type
    assert_equal @gem_repo, entry.remote
    assert_empty entry.missing

    output = lockfile([entry]).to_s

    assert_includes output, "GEM\n  remote: #{@gem_repo}\n  specs:\n    a (2)\n"
  end

  def test_lock_entries_show_missing_remote_for_a_bare_install
    spec = util_spec "installed", "1.0"
    install_gem spec
    Gem::Specification.reset

    dep("installed", "= 1.0").to_spec.activate

    entry = Gem::RequestSet.new.lock_entries.first

    assert_equal :installed, entry.source_type
    assert_nil entry.remote
    assert_equal [:remote], entry.missing

    report = lockfile([entry]).missing_information
    assert_equal [{ name: "installed", version: spec.version, missing: [:remote] }], report

    output = lockfile([entry]).to_s
    assert_includes output, "GEM\n  # remote: unknown\n  specs:\n    installed (1.0)\n"
  end
  def test_lock_entries_report_a_completely_unknown_source
    spec = util_spec "orphan", "1.0"

    entry = Gem::RequestSet.new.lock_entries([spec]).first

    assert_equal :unknown, entry.source_type
    assert_equal [:source], entry.missing
  end

  def test_lock_entries_path_source
    name, version, directory = vendor_gem

    vendor_set = Gem::Resolver::VendorSet.new
    vendor_set.add_vendor_gem name, directory
    vendor_spec = vendor_set.find_all(dep(name)).first

    entry = Gem::RequestSet.new.lock_entries([vendor_spec]).first

    assert_equal :path, entry.source_type
    assert_predicate entry, :source_set?
    assert_equal directory, entry.path
    assert_empty entry.missing

    output = lockfile([entry], { name => Gem::Requirement.source_set }).to_s

    assert_includes output, "PATH\n  remote: #{directory}\n  specs:\n    #{name} (#{version})\n"
    assert_includes output, "DEPENDENCIES\n  #{name}!\n"
  end

  def test_lock_entries_path_source_from_an_activation_request
    name, version, directory = vendor_gem

    vendor_set = Gem::Resolver::VendorSet.new
    vendor_set.add_vendor_gem name, directory
    vendor_spec = vendor_set.find_all(dep(name)).first
    dep_request = Gem::Resolver::DependencyRequest.new dep(name), nil
    activation  = Gem::Resolver::ActivationRequest.new vendor_spec, dep_request

    entry = Gem::RequestSet.new.lock_entries([activation]).first

    assert_equal version, entry.version
    assert_equal :path, entry.source_type
    assert_equal directory, entry.path
  end

  def test_lock_entries_git_source
    name, version, repository, head = git_gem

    git_set = Gem::Resolver::GitSet.new
    git_set.root_dir = @tempdir
    git_set.add_git_gem name, repository, "HEAD", false
    git_set.prefetch nil
    git_spec = git_set.specs.fetch name

    entry = Gem::RequestSet.new.lock_entries([git_spec]).first

    assert_equal :git, entry.source_type
    assert_predicate entry, :source_set?
    assert_equal repository, entry.repository
    assert_equal head, entry.revision
    assert_empty entry.missing

    output = lockfile([entry], { name => Gem::Requirement.source_set }).to_s

    assert_includes output, "GIT\n  remote: #{repository}\n  revision: #{head}\n"
    assert_includes output, "DEPENDENCIES\n  #{name}!\n"
  end

  def test_lock_entries_git_missing_revision_is_reported
    source = Gem::Source::Git.new "missing", "https://example.invalid/missing.git", "main"

    def source.rev_parse
      raise Gem::Exception, "no cached revision"
    end

    resolver_spec = Gem::Resolver::GitSpecification.new nil, util_spec("missing", "1"), source

    entry = Gem::RequestSet.new.lock_entries([resolver_spec]).first

    assert_equal :git, entry.source_type
    assert_equal [:revision], entry.missing

    output = lockfile([entry], { "missing" => Gem::Requirement.source_set }).to_s
    assert_includes output, "  # revision: unknown\n"
  end
  def test_lock_entries_dependency_uses_activated_version
    spec = util_spec "withdeps", "4.0" do |s|
      s.add_dependency "base", "~> 1.0"
      s.add_development_dependency "devtool"
    end

    entry = Gem::RequestSet.new.lock_entries([spec]).first

    assert_equal ["base"], entry.dependencies.map(&:name)

    output = lockfile([entry]).to_s
    assert_includes output, "    withdeps (4.0)\n      base (~> 1.0)\n"
    refute_includes output, "devtool"
  end

  def test_lock_entries_accept_activation_requests
    spec_fetcher do |fetcher|
      fetcher.spec "a", 2, "b" => ">= 1"
      fetcher.spec "b", 1
    end

    set = Gem::RequestSet.new
    set.gem "a"
    requests = set.resolve

    entries = set.lock_entries(requests)

    assert_equal %w[a b], entries.map(&:name).sort
    a = entries.find {|entry| entry.name == "a" }
    assert_equal [dep("b", ">= 1")], a.dependencies
    assert_equal @gem_repo, a.remote
  end

  def test_old_build_still_resolves_remotely
    spec_fetcher do |fetcher|
      fetcher.spec "a", 2
    end

    set = Gem::RequestSet.new
    set.gem "a"

    output = Gem::RequestSet::Lockfile.build(set, @gem_deps_file).to_s

    assert_includes output, "GEM\n  remote: #{@gem_repo}\n  specs:\n    a (2)\n"
    assert_includes output, "PLATFORMS\n  ruby\n"
  end

  def test_resolve_current_still_works
    install_gem util_spec("a", "2")
    Gem::Specification.reset

    set = Gem::RequestSet.new
    set.gem "a", "= 2"

    requests = set.resolve_current

    assert_equal ["a-2"], requests.map(&:full_name)

    entries = set.lock_entries(requests)
    assert_equal [Gem::Version.new(2)], entries.map(&:version)
  end

  def test_lock_entries_recover_the_resolved_source_for_an_activated_gem
    spec_fetcher do |fetcher|
      fetcher.spec "a", 2
    end

    set = Gem::RequestSet.new
    set.gem "a"
    set.resolve

    Gem::Specification.find_all_by_name("a").first.activate

    entry = set.lock_entries.first

    assert_equal :gem, entry.source_type
    assert_equal @gem_repo, entry.remote
    assert_empty entry.missing
  end

  def test_lock_entries_bundler_rubygems_source
    require "bundler"
    require "bundler/source/rubygems"

    source = Bundler::Source::Rubygems.new("remotes" => ["https://gems.example/"])
    spec = util_spec "bundled", "1.0"
    spec.source = source

    entry = Gem::RequestSet::LockEntry.from_spec(spec)

    assert_equal :gem, entry.source_type
    assert_equal "https://gems.example/", entry.remote
    assert_empty entry.missing
  end

  def test_lock_entries_bundler_path_source
    require "bundler"
    require "bundler/source/path"

    source = Bundler::Source::Path.new("path" => "vendor/local")
    spec = util_spec "local", "1.0"
    spec.source = source

    entry = Gem::RequestSet::LockEntry.from_spec(spec)

    assert_equal :path, entry.source_type
    assert_equal "vendor/local", entry.path
    refute_predicate entry, :missing?
  end

  def test_lockfile_from_activated_writes_and_replays_the_activated_version
    v1 = util_spec "tool", "1.0.0"
    v2 = util_spec "tool", "2.0.0"
    install_gem v1
    install_gem v2
    Gem::Specification.reset

    dep("tool", "= 1.0.0").to_spec.activate

    set = Gem::RequestSet.new
    lockfile = set.lockfile_from_activated @gem_deps_file
    lockfile.write

    assert_path_exist "#{@gem_deps_file}.lock"

    File.write @gem_deps_file, "gem \"tool\"\n"

    replay_dir = File.join @tempdir, "replay"
    plain_dir  = File.join @tempdir, "plain"
    FileUtils.mkdir_p [replay_dir, plain_dir]

    script = <<~RUBY
      require "rubygems"
      require "rubygems/request_set"

      use_lock = ARGV[0] == "lock"

      set = Gem::RequestSet.new
      set.remote = false

      if use_lock
        set.load_gemdeps "gem.deps.rb"
        set.resolve
        request = set.sorted_requests.find {|r| r.name == "tool" }
      else
        set.gem "tool"
        request = set.resolve_current.find {|r| r.name == "tool" }
      end

      request.full_spec.activate

      puts Gem.loaded_specs["tool"].full_name
      puts Gem.loaded_specs["tool"].loaded_from
    RUBY

    # Both directories are outside the gem home that produced the lock, and
    # each starts from only a gem dependencies file.  One gets the generated
    # lock; the other resolves against what is installed instead.
    File.write File.join(replay_dir, "gem.deps.rb"), "gem \"tool\"\n"
    File.write File.join(plain_dir, "gem.deps.rb"),  "gem \"tool\"\n"
    FileUtils.mv "#{@gem_deps_file}.lock", File.join(replay_dir, "gem.deps.rb.lock")
    File.write File.join(replay_dir, "choose.rb"), script
    File.write File.join(plain_dir, "choose.rb"), script

    env = { "GEM_HOME" => @gemhome, "GEM_PATH" => @gemhome }

    locked = run_choice env, replay_dir, "lock"
    plain  = run_choice env, plain_dir, "plain"

    # Without the lock, the newest installed version starts.
    assert_equal "tool-2.0.0", plain[0]

    # With the lock, the version that was activated when the lock was written
    # starts, even though a newer version is installed next to it.
    assert_equal "tool-1.0.0", locked[0]
    assert_equal File.join(@gemhome, "specifications", "tool-1.0.0.gemspec"),
                 locked[1]
  end

  def run_choice(env, directory, argument)
    Dir.chdir directory do
      Gem::Util.popen env, *ruby_with_rubygems_in_load_path,
        "-e", "require_relative \"choose.rb\"", argument
    end.split "\n"
  end

  def test_generated_lock_reloads_into_a_request_set
    base = util_spec "base", "1.5"
    install_gem base

    spec = util_spec "reload", "5.0" do |s|
      s.add_dependency "base", ">= 1"
    end
    install_gem spec
    Gem::Specification.reset

    dep("reload", "= 5.0").to_spec.activate
    dep("base", "= 1.5").to_spec.activate

    set = Gem::RequestSet.new
    set.lockfile_from_activated(@gem_deps_file).write
    File.write @gem_deps_file, "gem \"reload\"\n"

    reloaded = Gem::RequestSet.new
    reloaded.load_gemdeps @gem_deps_file

    lock_set = reloaded.sets.find {|s| Gem::Resolver::LockSet === s }
    refute_nil lock_set
    assert_equal %w[base-1.5 reload-5.0], lock_set.specs.map(&:full_name).sort

    reload_dep = reloaded.dependencies.find {|d| d.name == "reload" }
    assert_equal Gem::Requirement.new("= 5.0"), reload_dep.requirement
  end

  def test_default_gem_and_installed_version_follow_the_activation
    default_spec = new_default_spec "shared", "1.0"
    install_default_gems default_spec

    regular = util_spec "shared", "2.0"
    install_gem regular
    Gem::Specification.reset

    dep("shared", "= 1.0").to_spec.activate

    entry = Gem::RequestSet.new.lock_entries.first

    assert_equal default_spec.version, entry.version
    assert_equal [regular.version], entry.other_installed_versions
    assert_predicate entry, :divergent?
  end

  def test_activated_default_gem_is_not_displaced_by_a_newer_installed_one
    default_spec = new_default_spec "shared", "1.0"
    install_default_gems default_spec

    regular = util_spec "shared", "2.0"
    install_gem regular
    Gem::Specification.reset

    dep("shared", "= 1.0").to_spec.activate

    output = Gem::RequestSet.new.lockfile_from_activated(@gem_deps_file).to_s

    assert_includes output, "    shared (1.0)\n"
    assert_includes output, "  shared (= 1.0)\n"
    refute_includes output, "shared (2.0)"
  end

  def test_lock_entries_refuses_to_guess_a_missing_source
    entry = Gem::RequestSet::LockEntry.new util_spec("naked", "1.0")

    assert_equal :unknown, entry.source_type
    assert_nil entry.remote
    assert_includes entry.missing, :source

    output = lockfile([entry]).to_s

    assert_includes output, "GEM\n  # remote: unknown\n"
    assert_includes output, "    naked (1.0)\n"
  end

  def test_local_specs_for_sees_versions_that_do_not_match_the_platform
    foreign = util_spec "multi", "1.0" do |s|
      s.platform = "x86_64-linux"
    end
    install_gem foreign, force: true
    Gem::Specification.reset

    specs = Gem::RequestSet.new.local_specs_for "multi"

    assert_equal ["multi-1.0-x86_64-linux"], specs.map(&:full_name)
  end

  def test_divergences_reports_versions_that_were_not_activated
    v1 = util_spec "tool", "1.0.0"
    v2 = util_spec "tool", "2.0.0"
    install_gem v1
    install_gem v2
    Gem::Specification.reset

    dep("tool", "= 2.0.0").to_spec.activate

    set = Gem::RequestSet.new
    lockfile = set.lockfile_from_activated @gem_deps_file

    assert_equal(
      [{ name: "tool", activated: v2.version, installed: [v1.version] }],
      lockfile.divergences
    )
  end

  def test_no_divergence_when_the_only_installed_version_is_activated
    spec = util_spec "solo", "1.0"
    install_gem spec
    Gem::Specification.reset

    dep("solo", "= 1.0").to_spec.activate

    lockfile = Gem::RequestSet.new.lockfile_from_activated @gem_deps_file

    assert_empty lockfile.divergences
    assert_equal [[:remote]], lockfile.missing_information.map {|row| row[:missing] }
  end
end
