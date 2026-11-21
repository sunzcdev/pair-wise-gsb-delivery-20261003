# frozen_string_literal: true

require_relative "helper"
require "rubygems"

class TestGemRequire < Gem::TestCase
  class Latch
    def initialize(count = 1)
      @count = count
      @lock  = Monitor.new
      @cv    = @lock.new_cond
    end

    def release
      @lock.synchronize do
        @count -= 1 if @count > 0
        @cv.broadcast if @count.zero?
      end
    end

    def await
      @lock.synchronize do
        @cv.wait_while { @count > 0 }
      end
    end
  end

  def assert_require(path)
    assert require(path), "'#{path}' was already required"
  end

  def refute_require(path)
    refute require(path), "'#{path}' was not yet required"
  end

  def test_respect_loaded_features_caching_like_standard_require
    dir = Dir.mktmpdir("test_require", @tempdir)

    lp1 = File.join dir, "foo1"
    foo1 = File.join lp1, "foo.rb"

    FileUtils.mkdir_p lp1
    File.open(foo1, "w") {|f| f.write "class Object; HELLO = 'foo1' end" }

    lp = $LOAD_PATH.dup

    $LOAD_PATH.unshift lp1
    assert_require "foo"
    assert_equal "foo1", ::Object::HELLO

    lp2 = File.join dir, "foo2"
    foo2 = File.join lp2, "foo.rb"

    FileUtils.mkdir_p lp2
    File.open(foo2, "w") {|f| f.write "class Object; HELLO = 'foo2' end" }

    $LOAD_PATH.unshift lp2
    refute_require "foo"
    assert_equal "foo1", ::Object::HELLO
  ensure
    $LOAD_PATH.replace lp
    Object.send :remove_const, :HELLO if Object.const_defined? :HELLO
  end

  # Providing -I on the commandline should always beat gems
  def test_dash_i_beats_gems
    a1 = util_spec "a", "1", { "b" => "= 1" }, "lib/test_gem_require_a.rb"
    b1 = util_spec "b", "1", { "c" => "> 0" }, "lib/b/c.rb"
    c1 = util_spec "c", "1", nil, "lib/c/c.rb"
    c2 = util_spec "c", "2", nil, "lib/c/c.rb"

    install_specs c1, c2, b1, a1

    dir = Dir.mktmpdir("test_require", @tempdir)
    dash_i_arg = File.join dir, "lib"

    c_rb = File.join dash_i_arg, "b", "c.rb"

    FileUtils.mkdir_p File.dirname c_rb
    File.open(c_rb, "w") {|f| f.write "class Object; HELLO = 'world' end" }

    # Pretend to provide a commandline argument that overrides a file in gem b
    $LOAD_PATH.unshift dash_i_arg

    assert_require "test_gem_require_a"
    assert_require "b/c" # this should be required from -I
    assert_equal "world", ::Object::HELLO
    assert_equal %w[a-1 b-1], loaded_spec_names
  ensure
    Object.send :remove_const, :HELLO if Object.const_defined? :HELLO
  end

  def create_sync_thread
    Thread.new do
      yield
    ensure
      FILE_ENTERED_LATCH.release
      FILE_EXIT_LATCH.await
    end
  end

  # Providing -I on the commandline should always beat gems
  def test_dash_i_beats_default_gems
    a1 = new_default_spec "a", "1", { "b" => "= 1" }, "test_gem_require_a.rb"
    b1 = new_default_spec "b", "1", { "c" => "> 0" }, "b/c.rb"
    c1 = new_default_spec "c", "1", nil, "c/c.rb"
    c2 = new_default_spec "c", "2", nil, "c/c.rb"

    install_default_gems c1, c2, b1, a1

    dir = Dir.mktmpdir("test_require", @tempdir)
    dash_i_arg = File.join dir, "lib"

    c_rb = File.join dash_i_arg, "c", "c.rb"

    FileUtils.mkdir_p File.dirname c_rb
    File.open(c_rb, "w") {|f| f.write "class Object; HELLO = 'world' end" }

    assert_require "test_gem_require_a"

    # Pretend to provide a commandline argument that overrides a file in gem b
    $LOAD_PATH.unshift dash_i_arg

    assert_require "b/c"
    assert_require "c/c" # this should be required from -I
    assert_equal "world", ::Object::HELLO
    assert_equal %w[a-1 b-1], loaded_spec_names
  ensure
    Object.send :remove_const, :HELLO if Object.const_defined? :HELLO
  end

  def test_dash_i_respects_default_library_extension_priority
    pend "needs investigation" if Gem.java_platform?
    pend "not installed yet" unless RbConfig::TOPDIR

    dash_i_ext_arg = util_install_extension_file("a")
    dash_i_lib_arg = util_install_ruby_file("a")

    $LOAD_PATH.unshift dash_i_lib_arg
    $LOAD_PATH.unshift dash_i_ext_arg
    assert_require "a"
    assert_match(/a\.rb$/, $LOADED_FEATURES.last)
  end

  def test_concurrent_require
    Object.const_set :FILE_ENTERED_LATCH, Latch.new(2)
    Object.const_set :FILE_EXIT_LATCH, Latch.new(1)

    a1 = util_spec "a#{$$}", "1", nil, "lib/a#{$$}.rb"
    b1 = util_spec "b#{$$}", "1", nil, "lib/b#{$$}.rb"

    install_specs a1, b1

    t1 = create_sync_thread { assert_require "a#{$$}" }
    t2 = create_sync_thread { assert_require "b#{$$}" }

    # wait until both files are waiting on the exit latch
    FILE_ENTERED_LATCH.await

    # now let them finish
    FILE_EXIT_LATCH.release

    assert t1.join, "thread 1 should exit"
    assert t2.join, "thread 2 should exit"
  ensure
    Object.send :remove_const, :FILE_ENTERED_LATCH if Object.const_defined? :FILE_ENTERED_LATCH
    Object.send :remove_const, :FILE_EXIT_LATCH if Object.const_defined? :FILE_EXIT_LATCH
  end

  def test_require_is_not_lazy_with_exact_req
    a1 = util_spec "a", "1", { "b" => "= 1" }, "lib/test_gem_require_a.rb"
    b1 = util_spec "b", "1", nil, "lib/b/c.rb"
    b2 = util_spec "b", "2", nil, "lib/b/c.rb"

    install_specs b1, b2, a1

    assert_require "test_gem_require_a"
    assert_equal %w[a-1 b-1], loaded_spec_names
    assert_equal unresolved_names, []

    assert_require "b/c"
    assert_equal %w[a-1 b-1], loaded_spec_names
  end

  def test_require_is_not_lazy_with_shadowed_default_gem
    b1_default = new_default_spec("b", "1", nil, "foo.rb")
    install_default_gems b1_default

    a1 = util_spec "a", "1", { "b" => ">= 1" }, "lib/test_gem_require_a.rb"
    b1 = util_spec("b", "1", nil, "lib/foo.rb")
    install_specs b1, a1

    # Load default ruby gems fresh as if we've just started a ruby script.
    Gem::Specification.reset

    assert_require "test_gem_require_a"
    assert_equal %w[a-1 b-1], loaded_spec_names
    assert_equal unresolved_names, []
  end

  def test_require_is_lazy_with_inexact_req
    a1 = util_spec "a", "1", { "b" => ">= 1" }, "lib/test_gem_require_a.rb"
    b1 = util_spec "b", "1", nil, "lib/b/c.rb"
    b2 = util_spec "b", "2", nil, "lib/b/c.rb"

    install_specs b1, b2, a1

    assert_require "test_gem_require_a"
    assert_equal %w[a-1], loaded_spec_names
    assert_equal unresolved_names, ["b (>= 1)"]

    assert_require "b/c"
    assert_equal %w[a-1 b-2], loaded_spec_names
  end

  def test_require_is_not_lazy_with_one_possible
    a1 = util_spec "a", "1", { "b" => ">= 1" }, "lib/test_gem_require_a.rb"
    b1 = util_spec "b", "1", nil, "lib/b/c.rb"

    install_specs b1, a1

    assert_require "test_gem_require_a"
    assert_equal %w[a-1 b-1], loaded_spec_names
    assert_equal unresolved_names, []

    assert_require "b/c"
    assert_equal %w[a-1 b-1], loaded_spec_names
  end

  def test_require_can_use_a_pathname_object
    a1 = util_spec "a", "1", nil, "lib/test_gem_require_a.rb"

    install_specs a1

    assert_require Pathname.new "test_gem_require_a"
    assert_equal %w[a-1], loaded_spec_names
    assert_equal unresolved_names, []
  end

  def test_activate_via_require_respects_loaded_files
    pend "Not sure what's going on. If another spec creates a 'a' gem before
      this test, somehow require will load the erb in b, and ignore that the
      stdlib one is already in $LOADED_FEATURES?. Reproducible by running the
      spaceship_specific_file test before this one" if Gem.java_platform?

    pend "not installed yet" unless RbConfig::TOPDIR

    lib_dir = File.expand_path("../lib", __dir__)
    rubylibdir = File.realdirpath(RbConfig::CONFIG["rubylibdir"])
    if rubylibdir == lib_dir
      # testing in the ruby repository where RubyGems' lib/ == stdlib lib/
      # In that case we want to move the stdlib lib/ to still be after b-2 in $LOAD_PATH
      lp = $LOAD_PATH.dup
      $LOAD_PATH.delete lib_dir
      $LOAD_PATH.push lib_dir
      load_path_changed = true
    end

    require "erb" # the stdlib

    a1 = util_spec "a", "1", { "b" => ">= 1" }, "lib/test_gem_require_a.rb"
    b1 = util_spec "b", "1", nil, "lib/erb.rb"
    b2 = util_spec "b", "2", nil, "lib/erb.rb"

    install_specs b1, b2, a1

    # Activates a-1, but not b-1 and b-2
    assert_require "test_gem_require_a"
    assert_equal %w[a-1], loaded_spec_names
    assert $LOAD_PATH.include? a1.full_require_paths[0]
    refute $LOAD_PATH.include? b1.full_require_paths[0]
    refute $LOAD_PATH.include? b2.full_require_paths[0]

    assert_equal unresolved_names, ["b (>= 1)"]

    # The require('erb') below will activate b-2. However, its
    # lib/erb.rb won't ever be loaded. The reason is MRI sees that even
    # though b-2 is earlier in $LOAD_PATH it already loaded a erb.rb file
    # and that still exists in $LOAD_PATH (further down),
    # and as a result #gem_original_require returns false.
    refute require("erb"), "the erb stdlib should be recognized as already loaded"

    assert_includes $LOAD_PATH, b2.full_require_paths[0]
    assert_includes $LOAD_PATH, rubylibdir
    message = proc {
      "this test relies on the b-2 gem lib/ to be before stdlib to make sense\n" +
        $LOAD_PATH.pretty_inspect
    }
    assert_operator $LOAD_PATH.index(b2.full_require_paths[0]), :<, $LOAD_PATH.index(rubylibdir), message

    # We detected that we should activate b-2, so we did so, but
    # then #gem_original_require decided "I've already got some erb.rb" loaded.
    # This case is fine because our lazy loading provided exactly
    # the same behavior as eager loading would have.

    assert_equal %w[a-1 b-2], loaded_spec_names
  ensure
    $LOAD_PATH.replace lp if load_path_changed
  end

  def test_activate_via_require_respects_loaded_default_from_default_gems
    a1 = new_default_spec "a", "1", nil, "a.rb"

    # simulate requiring a default gem before rubygems is loaded
    Kernel.send(:gem_original_require, "a")

    # simulate registering default specs on loading rubygems
    install_default_gems a1

    a2 = util_spec "a", "2", nil, "lib/a.rb"

    install_specs a2

    refute_require "a"

    assert_equal %w[a-1], loaded_spec_names
  end

  # A registered default gem only says Ruby ships it. Until one of its files
  # is required, another version can be selected by the usual version rules.

  def test_registered_default_gem_does_not_block_selecting_another_version
    default_gem_spec = new_default_spec "shared", "1.0", nil, "shared.rb"
    install_default_gems default_gem_spec

    Gem::Specification.reset

    # Only registered, nothing loaded yet.
    refute_predicate default_gem_spec, :files_loaded?
    assert_nil default_gem_spec.loaded_files_description

    normal_gem_spec = util_spec "shared", "2.0", nil, "lib/shared.rb"
    install_specs normal_gem_spec

    assert_require "shared"
    assert_equal %w[shared-2.0], loaded_spec_names
    refute Gem.loaded_specs["shared"].default_gem?
  end

  def test_loaded_default_gem_reports_which_version_served_the_files
    default_gem_spec = new_default_spec "shared", "1.0", nil, "shared.rb"
    install_default_gems default_gem_spec

    Gem::Specification.reset

    assert_require "shared"
    assert_equal %w[shared-1.0], loaded_spec_names

    loaded = Gem.loaded_specs["shared"]
    assert_predicate loaded, :files_loaded?
    assert_includes loaded.loaded_files_description, "shared-1.0"

    normal_gem_spec = util_spec "shared", "2.0", nil, "lib/shared.rb"
    install_specs normal_gem_spec

    e = assert_raise Gem::LoadError do
      gem "shared", "= 2.0"
    end

    assert_includes e.message, "already activated shared-1.0"
    assert_includes e.message, "files already loaded from shared-1.0"
    assert_equal "shared-1.0", Gem.loaded_specs["shared"].full_name
  end

  def test_requiring_a_file_only_the_default_gem_has_does_not_mix_versions
    default_gem_spec = new_default_spec "shared", "1.0", nil, "shared.rb", "shared/extra.rb"
    install_default_gems default_gem_spec

    normal_gem_spec = util_spec "shared", "2.0", nil, "lib/shared.rb"
    install_specs normal_gem_spec

    Gem::Specification.reset

    gem "shared", "= 2.0"
    assert_require "shared"
    assert_equal %w[shared-2.0], loaded_spec_names

    e = assert_raise Gem::LoadError do
      require "shared/extra"
    end

    assert_includes e.message, "shared-1.0"
    assert_includes e.message, "shared-2.0"
    assert_equal %w[shared-2.0], loaded_spec_names
    refute_includes $LOADED_FEATURES, File.join(default_gem_spec.full_gem_path, "lib", "shared/extra.rb")
  end

  def test_requiring_a_file_both_versions_have_goes_to_the_activated_version
    default_gem_spec = new_default_spec "shared", "1.0", nil, "shared.rb"
    install_default_gems default_gem_spec

    normal_gem_spec = util_spec "shared", "2.0", nil, "lib/shared.rb"
    install_specs normal_gem_spec

    Gem::Specification.reset

    gem "shared", "= 2.0"
    assert_require "shared"

    assert_equal %w[shared-2.0], loaded_spec_names
    loaded = $LOADED_FEATURES.grep(%r{/shared\.rb$}).map {|feature| File.expand_path(feature) }
    assert_includes loaded, File.join(normal_gem_spec.full_gem_path, "lib", "shared.rb")
    refute_includes loaded, File.join(default_gem_spec.full_gem_path, "lib", "shared.rb")
  end

  def test_already_activated_direct_conflict
    a1 = util_spec "a", "1", { "b" => "> 0" }
    b1 = util_spec "b", "1", { "c" => ">= 1" }, "lib/ib.rb"
    b2 = util_spec "b", "2", { "c" => ">= 2" }, "lib/ib.rb"
    c1 = util_spec "c", "1", nil, "lib/d.rb"
    c2 = util_spec("c", "2", nil, "lib/d.rb")

    install_specs c1, c2, b1, b2, a1

    a1.activate
    c1.activate
    assert_equal %w[a-1 c-1], loaded_spec_names
    assert_equal ["b (> 0)"], unresolved_names

    assert require("ib")

    assert_equal %w[a-1 b-1 c-1], loaded_spec_names
    assert_equal [], unresolved_names
  end

  def test_multiple_gems_with_the_same_path
    a1 = util_spec "a", "1", { "b" => "> 0", "x" => "> 0" }
    b1 = util_spec "b", "1", { "c" => ">= 1" }, "lib/ib.rb"
    b2 = util_spec "b", "2", { "c" => ">= 2" }, "lib/ib.rb"
    x1 = util_spec "x", "1", nil, "lib/ib.rb"
    x2 = util_spec "x", "2", nil, "lib/ib.rb"
    c1 = util_spec "c", "1", nil, "lib/d.rb"
    c2 = util_spec("c", "2", nil, "lib/d.rb")

    install_specs c1, c2, x1, x2, b1, b2, a1

    a1.activate
    c1.activate
    assert_equal %w[a-1 c-1], loaded_spec_names
    assert_equal ["b (> 0)", "x (> 0)"], unresolved_names

    e = assert_raise(Gem::LoadError) do
      require("ib")
    end

    assert_equal "ib found in multiple gems: b, x", e.message
  end

  def test_unable_to_find_good_unresolved_version
    a1 = util_spec "a", "1", { "b" => "> 0" }
    b1 = util_spec "b", "1", { "c" => ">= 2" }, "lib/ib.rb"
    b2 = util_spec "b", "2", { "c" => ">= 3" }, "lib/ib.rb"

    c1 = util_spec "c", "1", nil, "lib/d.rb"
    c2 = util_spec "c", "2", nil, "lib/d.rb"
    c3 = util_spec "c", "3", nil, "lib/d.rb"

    install_specs c1, c2, c3, b1, b2, a1

    a1.activate
    c1.activate
    assert_equal %w[a-1 c-1], loaded_spec_names
    assert_equal ["b (> 0)"], unresolved_names

    e = assert_raise(Gem::LoadError) do
      require("ib")
    end

    assert_equal "unable to find a version of 'b' to activate", e.message
  end

  def test_require_works_after_cleanup
    a1 = new_default_spec "a", "1.0", nil, "a/b.rb"
    b1 = new_default_spec "b", "1.0", nil, "b/c.rb"
    b2 = new_default_spec "b", "2.0", nil, "b/d.rb"

    install_default_gems a1
    install_default_gems b1
    install_default_gems b2

    # Load default ruby gems fresh as if we've just started a ruby script.
    Gem::Specification.reset
    require "rubygems"
    Gem::Specification.stubs

    # Remove an old default gem version directly from disk as if someone ran
    # gem cleanup.
    FileUtils.rm_rf(File.join(@gemhome, b1.full_name.to_s))
    FileUtils.rm_rf(File.join(@gemhome, "specifications", "default", "#{b1.full_name}.gemspec"))

    # Require gems that have not been removed.
    assert_require "a/b"
    assert_equal %w[a-1.0], loaded_spec_names
    assert_require "b/d"
    assert_equal %w[a-1.0 b-2.0], loaded_spec_names
  end

  def test_require_doesnt_traverse_development_dependencies
    a = util_spec("a#{$$}", "1", nil, "lib/a#{$$}.rb")
    z = util_spec("z", "1", "w" => "> 0")
    w1 = util_spec("w", "1") {|s| s.add_development_dependency "non-existent" }
    w2 = util_spec("w", "2") {|s| s.add_development_dependency "non-existent" }

    install_specs a, w1, w2, z

    assert gem("z")
    assert_equal %w[z-1], loaded_spec_names
    assert_equal ["w (> 0)"], unresolved_names

    assert require("a#{$$}")
  end

  def test_default_gem_only
    default_gem_spec = new_default_spec("default", "2.0.0.0",
                                        nil, "default/gem.rb")
    install_default_gems(default_gem_spec)
    assert_require "default/gem"
    assert_equal %w[default-2.0.0.0], loaded_spec_names
  end

  def test_default_gem_listing_a_rubygems_file_is_not_activated_by_requiring_it
    rubygems_file = "rubygems/test_default_gem_#{$$}"
    default_bundler = new_default_spec("bundler", "4.1.0", nil, "#{rubygems_file}.rb")
    install_default_gems(default_bundler)

    assert_require rubygems_file
    assert_empty loaded_spec_names
  end

  def test_multiple_gems_with_the_same_path_the_non_activated_spec_is_chosen
    a1 = util_spec "a", "1", nil, "lib/ib.rb"
    a2 = util_spec "a", "2", nil, "lib/foo.rb"
    b1 = util_spec "b", "1", nil, "lib/ib.rb"

    install_specs a1, a2, b1

    a2.activate

    assert_equal %w[a-2], loaded_spec_names
    assert_empty unresolved_names

    assert_require "ib"
    assert_equal %w[a-2 b-1], loaded_spec_names
  end

  def test_default_gem_require_activates_just_once
    default_gem_spec = new_default_spec("default", "2.0.0.0",
                                        nil, "default/gem.rb")
    install_default_gems(default_gem_spec)

    assert_require "default/gem"

    times_called = 0

    Kernel.stub(:gem, ->(_name, _requirement) { times_called += 1 }) do
      refute_require "default/gem"
    end

    assert_equal 0, times_called
  end

  def test_second_gem_require_does_not_resolve_path_manually_before_going_through_standard_require
    a1 = util_spec "a", "1", nil, "lib/test_gem_require_a.rb"
    install_gem a1

    assert_require "test_gem_require_a"

    stub(:gem_original_require, ->(path) { assert_equal "test_gem_require_a", path }) do
      require "test_gem_require_a"
    end
  end

  def test_realworld_default_gem
    omit "this test can't work under ruby-core setup" if ruby_repo?
    omit "JRuby on Windows does not register json as a default gem the same way" if Gem.win_platform? && Gem.java_platform?

    cmd = <<-RUBY
      $stderr = $stdout
      require "json"
      puts Gem.loaded_specs["json"]
    RUBY
    output = Gem::Util.popen(*ruby_with_rubygems_in_load_path, "-e", cmd).strip
    assert Process.last_status.success?
    refute_empty output
  end

  def test_realworld_upgraded_default_gem
    omit "this test can't work under ruby-core setup" if ruby_repo?

    newer_json = util_spec("json", "999.99.9", nil, ["lib/json.rb"])
    install_gem newer_json

    path = "#{@tempdir}/test_realworld_upgraded_default_gem.rb"
    code = <<-RUBY
      $stderr = $stdout
      require "json"
      puts Gem.loaded_specs["json"].version
      puts $LOADED_FEATURES
    RUBY
    File.write(path, code)

    output = Gem::Util.popen({ "GEM_HOME" => @gemhome }, *ruby_with_rubygems_in_load_path, path).strip
    refute_empty output
    assert_equal "999.99.9", output.lines[0].chomp
    # Make sure only files from the newer json gem are loaded, and no files from the default json gem
    assert_equal ["#{@gemhome}/gems/json-999.99.9/lib/json.rb"], output.lines.grep(%r{/gems/json-}).map(&:chomp)
    assert Process.last_status.success?
  end

  # The two orders a CI launcher can meet: the project's constraint first, or
  # the shipped file first. Each runs in its own short Ruby process.

  def test_realworld_default_gem_file_provenance_constraint_first
    omit "this test can't work under ruby-core setup" if ruby_repo?
    omit "JRuby on Windows does not register json as a default gem the same way" if Gem.win_platform? && Gem.java_platform?

    newer_json = util_spec("json", "999.99.9", nil, ["lib/json.rb"])
    install_gem newer_json

    code = <<-'RUBY'
      $stderr = $stdout
      raise "no default json" unless Gem.find_default_spec("json")

      # The project declares the newer gem before anything is required.
      gem "json", "= 999.99.9"
      require "json"

      puts "spec=#{Gem.loaded_specs["json"].full_name}"
      puts "loaded=#{$LOADED_FEATURES.grep(%r{/json(\.rb|/)}).sort.inspect}"

      # A file only the shipped copy has must not be pulled in now that the
      # newer version runs.
      begin
        require "json/add/core"
        puts "extra=no_error"
      rescue Gem::LoadError => e
        puts "extra=#{e.message}"
      end

      puts "mixed=#{$LOADED_FEATURES.any? {|feature| feature.include?("json/add/core") }}"
    RUBY
    output = Gem::Util.popen({ "GEM_HOME" => @gemhome }, *ruby_with_rubygems_in_load_path, "-e", code).strip
    assert Process.last_status.success?, "subprocess failed: #{output}"

    lines = output.lines.map(&:chomp)
    assert_equal "spec=json-999.99.9", lines.find {|line| line.start_with?("spec=") }

    loaded = lines.find {|line| line.start_with?("loaded=") }
    assert_includes loaded, "#{@gemhome}/gems/json-999.99.9/lib/json.rb"
    refute_includes loaded, "ruby/4.0.0/json"

    extra = lines.find {|line| line.start_with?("extra=") }
    assert_includes extra, "can't load 'json/add/core'"
    assert_includes extra, "json-999.99.9 is already activated"
    assert_equal "mixed=false", lines.find {|line| line.start_with?("mixed=") }
  end

  def test_realworld_default_gem_file_provenance_loaded_first
    omit "this test can't work under ruby-core setup" if ruby_repo?
    omit "JRuby on Windows does not register json as a default gem the same way" if Gem.win_platform? && Gem.java_platform?

    newer_json = util_spec("json", "999.99.9", nil, ["lib/json.rb"])
    install_gem newer_json

    code = <<-'RUBY'
      $stderr = $stdout
      default = Gem.find_default_spec("json")
      raise "no default json" unless default

      # The launcher used the shipped copy, so its files really ran.
      gem "json", "= #{default.version}"
      require "json"
      puts "loaded_spec=#{Gem.loaded_specs["json"].full_name}"
      puts "provenance=#{Gem.loaded_specs["json"].loaded_files_description}"

      begin
        gem "json", "= 999.99.9"
        puts "activation=no_error"
      rescue Gem::LoadError => e
        puts "activation=#{e.message}"
      end

      # Loading another file of the same shipped version is still fine.
      puts "same_version=#{require("json/version").inspect}"

      puts "final=#{Gem.loaded_specs["json"].full_name}"
    RUBY
    output = Gem::Util.popen({ "GEM_HOME" => @gemhome }, *ruby_with_rubygems_in_load_path, "-e", code).strip
    assert Process.last_status.success?, "subprocess failed: #{output}"

    lines = output.lines.map(&:chomp)
    loaded_spec = lines.find {|line| line.start_with?("loaded_spec=") }
    provenance = lines.find {|line| line.start_with?("provenance=") }
    activation = lines.find {|line| line.start_with?("activation=") }
    final = lines.find {|line| line.start_with?("final=") }

    refute_nil loaded_spec, "missing loaded_spec in #{output}"
    default_full_name = loaded_spec.sub("loaded_spec=", "")
    refute_equal "json-999.99.9", default_full_name

    # The report must say which version served the loaded files.
    refute_nil provenance, "missing provenance in #{output}"
    assert_includes provenance, default_full_name
    assert_includes provenance, "json.rb"

    # Activating the other version is refused and points at the loaded files.
    refute_nil activation, "missing activation in #{output}"
    assert_includes activation, "already activated #{default_full_name}"
    assert_includes activation, "files already loaded from #{default_full_name}"

    assert_equal "final=#{default_full_name}", final
  end

  def test_default_gem_and_normal_gem
    default_gem_spec = new_default_spec("default", "2.0.0.0",
                                        nil, "default/gem.rb")
    install_default_gems(default_gem_spec)
    normal_gem_spec = util_spec("default", "3.0", nil,
                               "lib/default/gem.rb")
    install_specs(normal_gem_spec)
    assert_require "default/gem"
    assert_equal %w[default-3.0], loaded_spec_names
  end

  def test_default_gem_and_normal_gem_same_version
    default_gem_spec = new_default_spec("default", "3.0",
                                        nil, "default/gem.rb")
    install_default_gems(default_gem_spec)
    normal_gem_spec = util_spec("default", "3.0", nil,
                               "lib/default/gem.rb")
    install_specs(normal_gem_spec)

    # Load default ruby gems fresh as if we've just started a ruby script.
    Gem::Specification.reset

    assert_require "default/gem"
    assert_equal %w[default-3.0], loaded_spec_names
    refute Gem.loaded_specs["default"].default_gem?
  end

  def test_normal_gem_does_not_shadow_default_gem
    default_gem_spec = new_default_spec("foo", "2.0", nil, "foo.rb")
    install_default_gems(default_gem_spec)

    normal_gem_spec = util_spec("fake-foo", "3.0", nil, "lib/foo.rb")
    install_specs(normal_gem_spec)

    assert_require "foo"
    assert_equal %w[foo-2.0], loaded_spec_names
  end

  def test_normal_gems_with_overridden_load_error_message
    normal_gem_spec = util_spec("normal", "3.0", nil, "lib/normal/gem.rb")

    install_specs(normal_gem_spec)

    File.write("require_with_overridden_load_error_message.rb", <<-RUBY)
      LoadError.class_eval do
        def message
          "Overridden message"
        end
      end

      require 'normal/gem'
    RUBY

    require "open3"

    output, exit_status = Open3.capture2e(
      { "GEM_HOME" => Gem.paths.home },
      *ruby_with_rubygems_in_load_path,
      "-r",
      "./require_with_overridden_load_error_message.rb"
    )

    assert exit_status.success?, "Require failed due to #{output}"
  end

  def test_default_gem_prerelease
    default_gem_spec = new_default_spec("default", "2.0.0",
                                        nil, "default/gem.rb")
    install_default_gems(default_gem_spec)

    normal_gem_higher_prerelease_spec = util_spec("default", "3.0.0.rc2", nil,
                                                  "lib/default/gem.rb")
    install_default_gems(normal_gem_higher_prerelease_spec)

    assert_require "default/gem"
    assert_equal %w[default-3.0.0.rc2], loaded_spec_names
  end

  def test_default_gem_with_unresolved_gems_depending_on_it
    my_http_old = util_spec "my-http", "0.1.1", nil, "lib/my/http.rb"
    install_gem my_http_old

    my_http_default = new_default_spec "my-http", "0.3.0", nil, "my/http.rb"
    install_default_gems my_http_default

    faraday_1 = util_spec "faraday", "1", { "my-http" => ">= 0" }
    install_gem faraday_1

    faraday_2 = util_spec "faraday", "2", { "my-http" => ">= 0" }
    install_gem faraday_2

    chef = util_spec "chef", "1", { "faraday" => [">= 1", "< 3"] }, "lib/chef.rb"
    install_gem chef

    assert_require "chef"
    assert_require "my/http"
  end

  def test_default_gem_required_circulary_with_unresolved_gems_depending_on_it
    my_http_old = util_spec "my-http", "0.1.1", nil, "lib/my/http.rb"
    install_gem my_http_old

    my_http_default = new_default_spec "my-http", "0.3.0", nil, "my/http.rb"
    my_http_default_path = File.join(@tempdir, "default_gems", "lib", "my/http.rb")
    install_default_gems my_http_default
    File.write(my_http_default_path, 'require "my/http"')

    faraday_1 = util_spec "faraday", "1", { "my-http" => ">= 0" }
    install_gem faraday_1

    faraday_2 = util_spec "faraday", "2", { "my-http" => ">= 0" }
    install_gem faraday_2

    chef = util_spec "chef", "1", { "faraday" => [">= 1", "< 3"] }, "lib/chef.rb"
    install_gem chef

    assert_require "chef"

    out, err = capture_output do
      assert_require "my/http"
    end

    assert_empty out

    circular_require_warning = false

    err_lines = err.split("\n").reject do |line|
      if line.include?("circular require")
        circular_require_warning = true
      elsif circular_require_warning # ignore backtrace lines for circular require warning
        circular_require_warning = line.start_with?(/[\s]/)
      end
    end

    assert_empty err_lines
  end

  def loaded_spec_names
    Gem.loaded_specs.values.map(&:full_name).sort
  end

  def unresolved_names
    Gem::Specification.unresolved_deps.values.map(&:to_s).sort
  end

  def test_try_activate_error_unlocks_require_monitor
    silence_warnings do
      class << ::Gem
        alias_method :old_try_activate, :try_activate
        def try_activate(*)
          raise "raised from try_activate"
        end
      end
    end

    require "does_not_exist_for_try_activate_test"
  rescue RuntimeError => e
    assert_match(/raised from try_activate/, e.message)
    assert Kernel::RUBYGEMS_ACTIVATION_MONITOR.try_enter, "require monitor was not unlocked when try_activate raised"
  ensure
    silence_warnings do
      class << ::Gem
        alias_method :try_activate, :old_try_activate
      end
    end
    Kernel::RUBYGEMS_ACTIVATION_MONITOR.exit
  end

  def test_require_when_gem_defined
    default_gem_spec = new_default_spec("default", "2.0.0.0",
                                        nil, "default/gem.rb")
    install_default_gems(default_gem_spec)
    c = Class.new do
      def self.gem(*args)
        raise "received #gem with #{args.inspect}"
      end
    end
    assert c.send(:require, "default/gem")
    assert_equal %w[default-2.0.0.0], loaded_spec_names
  end

  def test_require_default_when_gem_defined
    a = util_spec("a#{$$}", "1", nil, "lib/a#{$$}.rb")
    install_specs a
    c = Class.new do
      def self.gem(*args)
        raise "received #gem with #{args.inspect}"
      end
    end
    assert c.send(:require, "a#{$$}")
    assert_equal %W[a#{$$}-1], loaded_spec_names
  end

  def test_require_bundler
    b1 = util_spec("bundler", "1", nil, "lib/bundler/setup.rb")
    b2a = util_spec("bundler", "2.a", nil, "lib/bundler/setup.rb")
    install_specs b1, b2a

    require "rubygems/bundler_version_finder"
    $:.clear
    assert_require "bundler/setup"
    assert_equal %w[bundler-2.a], loaded_spec_names
    assert_empty unresolved_names
  end

  ["", "Kernel."].each do |prefix|
    define_method "test_no_kernel_require_in_#{prefix.tr(".", "_")}warn_with_uplevel" do
      Dir.mktmpdir("warn_test") do |dir|
        File.write(dir + "/sub.rb", "#{prefix}warn 'uplevel', 'test', uplevel: 1\n")
        File.write(dir + "/main.rb", "require 'sub'\n")
        _, err = capture_subprocess_io do
          system(*ruby_with_rubygems_in_load_path, "-w", "--disable=gems", "-C", dir, "-I", dir, "main.rb")
        end
        assert_match(/main\.rb:1: warning: uplevel\ntest\n$/, err)
        _, err = capture_subprocess_io do
          system(*ruby_with_rubygems_in_load_path, "-w", "--enable=gems", "-C", dir, "-I", dir, "main.rb")
        end
        assert_match(/main\.rb:1: warning: uplevel\ntest\n$/, err)
      end
    end

    define_method "test_no_other_behavioral_changes_with_#{prefix.tr(".", "_")}warn" do
      Dir.mktmpdir("warn_test") do |dir|
        File.write(dir + "/main.rb", "#{prefix}warn({x:1}, {y:2}, [])\n")
        _, err = capture_subprocess_io do
          system(*ruby_with_rubygems_in_load_path, "-w", "--disable=gems", "-C", dir, "main.rb")
        end
        assert_match(/#{{ x: 1 }.inspect}\n#{{ y: 2 }.inspect}\n$/, err)
        _, err = capture_subprocess_io do
          system(*ruby_with_rubygems_in_load_path, "-w", "--enable=gems", "-C", dir, "main.rb")
        end
        assert_match(/#{{ x: 1 }.inspect}\n#{{ y: 2 }.inspect}\n$/, err)
      end
    end
  end

  def test_no_crash_when_overriding_warn_with_warning_module
    Dir.mktmpdir("warn_test") do |dir|
      File.write(dir + "/main.rb", "module Warning; def warn(str); super; end; end; warn 'Foo Bar'")
      _, err = capture_subprocess_io do
        system(*ruby_with_rubygems_in_load_path, "-w", "--disable=gems", "-C", dir, "main.rb")
      end
      assert_match(/Foo Bar\n$/, err)
      _, err = capture_subprocess_io do
        system(*ruby_with_rubygems_in_load_path, "-w", "--enable=gems", "-C", dir, "main.rb")
      end
      assert_match(/Foo Bar\n$/, err)
    end
  end

  def test_expected_backtrace_location_when_inheriting_from_basic_object_and_including_kernel
    Dir.mktmpdir("warn_test") do |dir|
      File.write(dir + "/main.rb", "\nrequire 'sub'\n")
      File.write(dir + "/sub.rb", <<-'RUBY')
        require 'rubygems'
        class C < BasicObject
          include ::Kernel
          def deprecated
            warn "This is a deprecated method", uplevel: 2
          end
        end
        C.new.deprecated
      RUBY

      _, err = capture_subprocess_io do
        system(*ruby_with_rubygems_in_load_path, "-w", "--disable=gems", "-C", dir, "-I", dir, "main.rb")
      end
      assert_match(/main\.rb:2: warning: This is a deprecated method$/, err)
      _, err = capture_subprocess_io do
        system(*ruby_with_rubygems_in_load_path, "-w", "--enable=gems", "-C", dir, "-I", dir, "main.rb")
      end
      assert_match(/main\.rb:2: warning: This is a deprecated method$/, err)
    end
  end

  def test_require_does_not_crash_when_utilizing_bundler_version_finder
    omit "JRuby on Windows hits a different require path" if Gem.win_platform? && Gem.java_platform?

    a1 = util_spec "a", "1.1", { "bundler" => ">= 0" }
    a2 = util_spec "a", "1.2", { "bundler" => ">= 0" }
    b1 = util_spec "bundler", "2.3.7"
    b2 = util_spec "bundler", "2.3.24"
    c = util_spec "c", "1", { "a" => [">= 1.1", "< 99.0"] }, "lib/test_gem_require_c.rb"

    install_specs a1, a2, b1, b2, c

    cmd = <<-RUBY
      require "test_gem_require_c"
      require "json"
    RUBY
    out = Gem::Util.popen({ "GEM_HOME" => @gemhome }, *ruby_with_rubygems_in_load_path, "-e", cmd)
    assert_predicate $?, :success?, "Require failed due to #{out}"
  end

  private

  def util_install_extension_file(name)
    spec = quick_gem name
    util_build_gem spec

    spec.extensions << "extconf.rb"
    write_file File.join(@tempdir, "extconf.rb") do |io|
      io.write <<-RUBY
        require "mkmf"
        CONFIG['LDSHARED'] = '$(TOUCH) $@ ||'
        create_makefile("#{name}")
      RUBY
    end

    write_file File.join(@tempdir, "#{name}.c") do |io|
      io.write <<-C
        void Init_#{name}() { }
      C
    end

    write_file File.join(@tempdir, "depend")

    spec.files += ["extconf.rb", "depend", "#{name}.c"]

    extension_file = File.join(spec.extension_dir, "#{name}.#{RbConfig::CONFIG["DLEXT"]}")
    assert_path_not_exist extension_file

    path = Gem::Package.build spec
    installer = Gem::Installer.at path
    installer.install
    assert_path_exist extension_file

    spec.gem_dir
  end

  def util_install_ruby_file(name)
    dir_lib = Dir.mktmpdir("test_require_lib", @tempdir)
    dash_i_lib_arg = File.join dir_lib

    a_rb = File.join dash_i_lib_arg, "#{name}.rb"

    FileUtils.mkdir_p File.dirname a_rb
    File.open(a_rb, "w") {|f| f.write "# #{name}.rb" }

    dash_i_lib_arg
  end
end
