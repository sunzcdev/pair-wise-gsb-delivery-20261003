# frozen_string_literal: true

require_relative "helper"

##
# Builds a candidate decision record from the real resolver for an offline
# repository that contains, under one gem name + version:
#
# * a native x64-mingw-ucrt variant whose required_ruby_version excludes the
#   running Ruby (never compatible: dropped by Gem::Resolver#filter_specs
#   before the solver ever sees it),
# * two ruby-platform candidates where the newest one's transitive
#   requirement conflicts with a second root dependency (selectable at
#   first, rolled back once the later constraint lands),
#
# plus the explicit-single-file install path (Gem::Source::SpecificFile),
# where the caller-supplied .gem file is used as-is and a mismatching file
# reports its own constraint conflict.

class TestGemResolverCandidateDecisions < Gem::TestCase
  ##
  # Records every spec Gem::Resolver#filter_specs drops (with the reason)
  # and every PubGrub solver event, so a test can replay the decision order.

  class RecordingResolver < Gem::Resolver
    attr_reader :filter_events, :solver_events

    def initialize(*args)
      @filter_events = []
      @solver_events = []
      super
    end

    private

    def filter_specs(specs)
      filtered = super

      specs.each do |s|
        next if filtered.include?(s)

        actual = s.respond_to?(:spec) ? s.spec : s
        reasons = []
        unless Gem::Platform.installable?(s)
          reasons << "platform #{actual.platform} is not installable on #{Gem::Platform.local}"
        end
        unless actual.required_ruby_version.satisfied_by?(Gem.ruby_version)
          reasons << "requires Ruby #{actual.required_ruby_version}, current is #{Gem.ruby_version}"
        end
        @filter_events << [actual.full_name, reasons]
      end

      filtered
    end

    def make_logger
      resolver = self

      Class.new do
        define_method :info do |&block|
          resolver.solver_events << block.call if block
        end
        alias_method :debug, :info
        alias_method :warn, :info
        alias_method :error, :info
      end.new
    end
  end

  def setup
    super

    @repo_dir = File.join @tempdir, "offline-repo"
    FileUtils.mkdir_p @repo_dir

    # Native variant of demo 2.0: matches x64-mingw-ucrt hosts by platform
    # but requires a Ruby older than any supported one.
    @native_spec, @native_file = build_gem_file "demo", "2.0" do |s|
      s.platform = "x64-mingw-ucrt"
      s.required_ruby_version = "< 3.0"
    end

    # Newest ruby-platform candidate: its sub-dependency becomes
    # unresolvable once the second root dependency is added.
    @demo2_spec, @demo2_file = build_gem_file "demo", "2.0" do |s|
      s.add_dependency "shared", "= 1"
    end

    # Older ruby-platform candidate: compatible with both root dependencies.
    @demo1_spec, @demo1_file = build_gem_file "demo", "1.0" do |s|
      s.add_dependency "shared", "= 2"
    end

    @shared1_spec, @shared1_file = build_gem_file "shared", "1.0"
    @shared2_spec, @shared2_file = build_gem_file "shared", "2.0"
  end

  ##
  # Automatic resolution over the offline repository with two root
  # dependencies. Produces and verifies the candidate decision record.

  def test_decision_record_for_automatic_resolution
    set = StaticSet.new [
      file_spec(@native_spec, @native_file),
      file_spec(@demo2_spec, @demo2_file),
      file_spec(@demo1_spec, @demo1_file),
      file_spec(@shared1_spec, @shared1_file),
      file_spec(@shared2_spec, @shared2_file),
    ]

    demo_dep   = Gem::Dependency.new "demo"
    shared_dep = Gem::Dependency.new "shared", "= 2"

    resolver = RecordingResolver.new [demo_dep, shared_dep], set

    activated = resolver.resolve

    # -- Final activated sources --------------------------------------
    activated_by_name = activated.map {|req| [req.spec.name, req.spec] }.to_h

    assert_equal %w[demo shared], activated_by_name.keys.sort

    assert_equal "demo-1.0", activated_by_name["demo"].spec.full_name
    assert_equal File.expand_path(@demo1_file), activated_by_name["demo"].source.path

    assert_equal "shared-2.0", activated_by_name["shared"].spec.full_name
    assert_equal File.expand_path(@shared2_file), activated_by_name["shared"].source.path

    # -- Never compatible: the native variant is dropped while filtering
    filtered_names = resolver.filter_events.map(&:first)
    assert_includes filtered_names, "demo-2.0-x64-mingw-ucrt"
    refute_includes filtered_names, "demo-2.0"
    refute_includes filtered_names, "demo-1.0"

    native_reasons = resolver.filter_events.assoc("demo-2.0-x64-mingw-ucrt").last
    refute_empty native_reasons

    expected_reasons = []
    unless Gem::Platform.match_gem?(Gem::Platform.new("x64-mingw-ucrt"), "demo")
      expected_reasons << :platform
    end
    expected_reasons << :ruby_version # "< 3.0" never satisfies the running Ruby
    assert_equal expected_reasons.length, native_reasons.length

    # -- Selectable at first, rolled back later: demo-2.0 (ruby) ------
    events = resolver.solver_events

    attempted_demo2 = events.index("attempting demo 2.0")
    assert attempted_demo2, "solver should try the newest ruby candidate demo 2.0 first"

    conflict = events.index {|e| e.start_with? "conflict:" }
    assert conflict, "solver should hit a conflict once shared = 2 is combined with demo 2.0"
    assert_operator conflict, :>, attempted_demo2

    rolled_back = events.index("derived: not demo >= 2.0")
    assert rolled_back, "solver should rule demo 2.0 out again after the conflict"
    assert_operator rolled_back, :>, conflict

    attempted_demo1 = events.index("attempting demo 1.0")
    assert attempted_demo1, "solver should fall back to demo 1.0"
    assert_operator attempted_demo1, :>, rolled_back

    solution = events.index {|e| e.start_with? "Solution found" }
    assert solution, "solver should find a solution after rolling back demo 2.0"
    assert_includes events[solution..], "* demo 1.0"
    refute_includes events[solution..], "* demo 2.0"
  end

  ##
  # Explicit single-file install: the caller-supplied .gem file is used
  # as-is (Gem::Source::SpecificFile), even though the repository offers
  # the same version and a newer one.

  def test_explicit_local_file_is_used_as_given
    installer_set = Gem::Resolver::InstallerSet.new :local
    source = Gem::Source::SpecificFile.new @demo1_file
    installer_set.add_local @demo1_file, source.spec, source

    dependency = Gem::Dependency.new source.spec.name, source.spec.version
    installer_set.add_always_install dependency

    repo_set = StaticSet.new [
      file_spec(@demo2_spec, @demo2_file),
      file_spec(@demo1_spec, @demo1_file),
      file_spec(@shared1_spec, @shared1_file),
      file_spec(@shared2_spec, @shared2_file),
    ]

    set = Gem::Resolver.compose_sets installer_set, repo_set
    resolver = Gem::Resolver.new [dependency], set

    activated = resolver.resolve
    demo = activated.find {|req| req.spec.name == "demo" }

    assert_kind_of Gem::Resolver::LocalSpecification, demo.spec
    assert_equal File.expand_path(@demo1_file), demo.spec.source.path
    assert demo.spec.installable_platform?,
      "a caller-supplied gem file is treated as installable for its source"

    shared = activated.find {|req| req.spec.name == "shared" }
    assert_equal "shared-2.0", shared.spec.spec.full_name
  end

  ##
  # Explicit single-file install where the file's own constraints do not
  # match: the platform check is bypassed for the given file, but the
  # Ruby version conflict is reported against the file itself.

  def test_explicit_local_file_reports_constraint_conflict
    installer_set = Gem::Resolver::InstallerSet.new :local
    source = Gem::Source::SpecificFile.new @native_file
    installer_set.add_local @native_file, source.spec, source

    local_spec = Gem::Resolver::LocalSpecification.new installer_set, source.spec, source
    assert local_spec.installable_platform?,
      "platform check is bypassed for a caller-supplied gem file"

    dependency = Gem::Dependency.new source.spec.name, source.spec.version

    error = assert_raise Gem::RuntimeRequirementNotMetError do
      installer_set.add_always_install dependency
    end

    assert_includes error.message, "demo-2.0-x64-mingw-ucrt"
    assert_includes error.message, "requires Ruby version < 3.0"
    assert_includes error.message, Gem.ruby_version.to_s
  end

  private

  def build_gem_file(name, version)
    spec = Gem::Specification.new do |s|
      s.name        = name
      s.version     = version
      s.platform    = Gem::Platform::RUBY
      s.authors     = ["A User"]
      s.email       = "example@example.com"
      s.homepage    = "http://example.com"
      s.summary     = "summary"
      s.description = "description"
      s.files       = ["lib/#{name}.rb"]

      yield s if block_given?
    end

    build_dir = File.join @tempdir, "build", spec.full_name
    FileUtils.mkdir_p File.join(build_dir, "lib")
    File.write File.join(build_dir, "lib", "#{name}.rb"), "# #{name}"

    built = nil
    Dir.chdir build_dir do
      use_ui Gem::MockGemUi.new do
        built = Gem::Package.build spec
      end
      FileUtils.mv built, @repo_dir
    end

    [spec, File.join(@repo_dir, built)]
  end

  ##
  # A resolver spec whose source is the exact .gem file, so the decision
  # record can name the file each activated candidate came from.

  def file_spec(spec, gem_file)
    Gem::Resolver::SpecSpecification.new nil, spec, Gem::Source::SpecificFile.new(gem_file)
  end
end