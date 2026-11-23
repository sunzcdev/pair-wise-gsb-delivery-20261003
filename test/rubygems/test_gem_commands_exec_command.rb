# frozen_string_literal: true

require_relative "helper"
require "rubygems/commands/exec_command"

class TestGemCommandsExecCommand < Gem::TestCase
  def setup
    @orig_args = Gem::Command.build_args
    @orig_specific_extra_args = Gem::Command.specific_extra_args_hash.dup
    @orig_extra_args = Gem::Command.extra_args.dup

    super
    common_installer_setup

    remove_probe_constants

    @cmd = Gem::Commands::ExecCommand.new

    @gem_home = Gem.dir
    @gem_path = Gem.path
    @test_arch = RbConfig::CONFIG["arch"]

    @installed_specs = []
    Gem.post_install {|installer| @installed_specs << installer.spec }
  end

  def teardown
    super

    common_installer_teardown

    remove_probe_constants

    Gem::Command.build_args = @orig_args
    Gem::Command.specific_extra_args_hash = @orig_specific_extra_args
    Gem::Command.extra_args = @orig_extra_args
    Gem.configuration = nil
  end

  def invoke(*args)
    @ui.outs.truncate(0)
    @ui.outs.rewind
    @ui.errs.truncate(0)
    @ui.errs.rewind
    @installed_specs.clear

    @cmd.invoke(*args)
  ensure
    Gem::Specification.unresolved_deps.clear
    Gem.loaded_specs.clear
    Gem.instance_variable_set(:@activated_gem_paths, 0)
    Gem.clear_default_specs
    Gem.use_paths(@gem_home, @gem_path)
    Gem.refresh
  end

  def test_error_with_no_arguments
    e = assert_raise Gem::CommandLineError do
      @cmd.invoke
    end
    assert_equal "Please specify an executable to run (e.g. gem exec COMMAND)",
      e.message
  end

  def test_error_with_no_executable
    e = assert_raise Gem::CommandLineError do
      @cmd.invoke "--verbose", "--gem", "GEM", "--version", "< 10", "--conservative"
    end
    assert_equal "Please specify an executable to run (e.g. gem exec COMMAND)",
      e.message
  end

  def test_full_option_parsing
    @cmd.when_invoked do |options|
      assert_equal options, {
        args: ["install", "--no-color", "--help", "--verbose"],
        executable: "pod",
        explicit_prerelease: false,
        gem_name: "cocoapods",
        prerelease: false,
        version: Gem::Requirement.new(["> 1", "< 1.3"]),
        build_args: nil,
      }
    end
    @cmd.invoke "--gem", "cocoapods", "-v", "> 1", "--version", "< 1.3", "--verbose", "--", "pod", "install", "--no-color", "--help", "--verbose"
  end

  def test_single_arg_parsing
    @cmd.when_invoked do |options|
      assert_equal options, {
        args: [],
        executable: "rails",
        gem_name: "rails",
        version: Gem::Requirement.new([">= 0"]),
        build_args: nil,
      }
    end
    @cmd.invoke "rails"
  end

  def test_single_arg_parsing_with_version
    @cmd.when_invoked do |options|
      assert_equal options, {
        args: [],
        executable: "rails",
        gem_name: "rails",
        version: Gem::Requirement.new(["= 7.1"]),
        build_args: nil,
      }
    end
    @cmd.invoke "rails:7.1"
  end

  def test_gem_without_executable
    spec_fetcher do |fetcher|
      fetcher.gem "a", 2
    end

    util_clear_gems

    use_ui @ui do
      e = assert_raise Gem::MockGemUi::TermError, @ui.error do
        @cmd.invoke "a:2"
      end
      assert_equal 1, e.exit_code
      assert_equal "ERROR:  Failed to load executable `a`, are you sure the gem `a` contains it?\n", @ui.error
    end
  end

  def test_gem_with_executable
    spec_fetcher do |fetcher|
      fetcher.gem "a", 2 do |s|
        s.executables = %w[a]
        s.files = %w[bin/a lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "Gem.ui.say #{s.original_name.dump}"
        end
      end
    end

    util_clear_gems

    use_ui @ui do
      @cmd.invoke "a:2"
      assert_equal "a-2\n", @ui.output
    end
  end

  def test_gem_with_executable_from_custom_source
    spec_fetcher "http://other-gems.example.com/" do |fetcher|
      fetcher.gem "a", 2 do |s|
        s.executables = %w[a]
        s.files = %w[bin/a lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "Gem.ui.say #{s.original_name.dump}"
        end
      end
    end

    util_clear_gems

    use_ui @ui do
      @cmd.invoke "--source", "http://other-gems.example.com", "a:2"
      assert_equal "a-2\n", @ui.output
    end

    assert_include Gem.sources.to_a, "http://other-gems.example.com/"
  end

  def test_gem_with_platforms
    spec_fetcher do |fetcher|
      fetcher.download "a", 2 do |s|
        s.executables = %w[a]
        s.files = %w[bin/a lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "Gem.ui.say #{s.original_name.dump}"
        end
      end

      fetcher.download "a", 2 do |s|
        s.executables = %w[a]
        s.files = %w[bin/a lib/a.rb]
        s.platform = "x86_64-darwin"

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "Gem.ui.say #{s.original_name.dump}"
        end
      end
    end

    use_ui @ui do
      invoke "a:2"
      assert_equal "a-2\n", @ui.output
    end

    use_ui @ui do
      util_set_arch "x86_64-darwin-18"
      invoke "a:2"
      assert_equal "a-2-x86_64-darwin\n", @ui.output
    end
  end

  def test_gem_with_platform_dependencies
    spec_fetcher do |fetcher|
      fetcher.download "a", 2 do |s|
        s.executables = %w[a]
        s.files = %w[bin/a lib/a.rb]
        s.add_dependency "with_platform"

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << 'require "with_platform"' << "\n"
          f << 'Gem.ui.say Gem.loaded_specs.each_value.map(&:original_name).sort.join("\n")'
        end
      end

      fetcher.download "with_platform", 2 do |s|
        s.files = %w[lib/with_platform.rb]
        s.platform = Gem::Platform.local
      end

      fetcher.download "with_platform", 2 do |s|
        s.files = %w[lib/with_platform.rb]
      end
    end

    use_ui @ui do
      util_set_arch "unknown-unknown"
      invoke "a"
      assert_equal "a-2\nwith_platform-2\n", @ui.output
    end

    use_ui @ui do
      util_set_arch @test_arch
      invoke "a"
      assert_empty @ui.error
      assert_equal "a-2\nwith_platform-2-#{Gem::Platform.local}\n", @ui.output
    end
  end

  def test_gem_with_platform_and_platform_dependencies
    pend "needs investigation" if Gem.java_platform?
    pend "terminates on mswin" if vc_windows? && ruby_repo?

    spec_fetcher do |fetcher|
      fetcher.download "a", 2 do |s|
        s.executables = %w[a]
        s.files = %w[bin/a lib/a.rb]
        s.add_dependency "with_platform"
        s.platform = Gem::Platform.local.to_s

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << 'require "with_platform"' << "\n"
          f << 'Gem.ui.say Gem.loaded_specs.each_value.map(&:original_name).sort.join("\n")'
        end
      end

      fetcher.download "a", 2 do |s|
        s.executables = %w[a]
        s.files = %w[bin/a lib/a.rb extconf.rb]
        s.add_dependency "with_platform"

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << 'require "with_platform"' << "\n"
          f << 'Gem.ui.say Gem.loaded_specs.each_value.map(&:original_name).sort.join("\n")'
        end

        s.extensions = %w[extconf.rb]
        write_file File.join(*%W[gems #{s.original_name} extconf.rb]) do |f|
          f.write <<-RUBY
            gem('with_platform', '~> 2.0')
            require 'with_platform'
            gem 'sometimes_used'
            require 'sometimes_used'
            require "mkmf"
            create_makefile("#{s.name}")
          RUBY
        end
      end

      fetcher.download "with_platform", 2 do |s|
        s.files = %w[lib/with_platform.rb]
        s.platform = Gem::Platform.local.to_s
      end

      fetcher.download "with_platform", 2 do |s|
        s.files = %w[lib/with_platform.rb]
        s.add_dependency "sometimes_used"
      end

      fetcher.download "sometimes_used", 2 do |s|
        s.files = %w[lib/sometimes_used.rb]
      end
    end

    use_ui @ui do
      util_set_arch "unknown-unknown"
      invoke "a"
      assert_empty @ui.error
      assert_equal "Building native extensions. This could take a while...\na-2\nsometimes_used-2\nwith_platform-2\n", @ui.output
    end

    use_ui @ui do
      util_set_arch @test_arch
      invoke "a"
      assert_empty @ui.error
      assert_equal "a-2-#{Gem::Platform.local}\nwith_platform-2-#{Gem::Platform.local}\n", @ui.output
    end
  end

  def test_gem_with_other_executable_name
    spec_fetcher do |fetcher|
      fetcher.gem "a", 2 do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump}"
        end
      end
    end

    util_clear_gems

    use_ui @ui do
      @cmd.invoke "a:2"
      assert_equal "a-2\n", @ui.output
    end
  end

  def test_gem_with_executable_error
    spec_fetcher do |fetcher|
      fetcher.gem "a", 2 do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "raise #{s.original_name.dump}"
        end
      end
    end

    util_clear_gems

    use_ui @ui do
      e = assert_raise RuntimeError do
        @cmd.invoke "a:2"
      end
      assert_equal "a-2", e.message
      assert_empty @ui.error
    end
  end

  def test_gem_with_multiple_executables_one_match
    spec_fetcher do |fetcher|
      fetcher.gem "a", 2 do |s|
        s.executables = %w[foo a]
        s.files = %w[bin/foo bin/a lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    util_clear_gems

    use_ui @ui do
      @cmd.invoke "a:2"
      assert_equal "a-2 a\n", @ui.output
    end
  end

  def test_gem_with_multiple_executables_no_match
    spec_fetcher do |fetcher|
      fetcher.gem "a", 2 do |s|
        s.executables = %w[foo bar]
        s.files = %w[bin/foo bin/bar lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end

        write_file File.join(*%W[gems #{s.original_name} bin bar]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    util_clear_gems

    use_ui @ui do
      e = assert_raise Gem::MockGemUi::TermError do
        @cmd.invoke "a:2"
      end
      assert_equal 1, e.exit_code
      assert_equal "ERROR:  Ambiguous which executable from gem `a` should be run: the options are [\"bar\", \"foo\"], specify one via COMMAND, and use `-g` and `-v` to specify gem and version\n", @ui.error
    end
  end

  def test_gem_dependency_contains_executable
    spec_fetcher do |fetcher|
      fetcher.gem "a", 2 do |s|
        s.executables = %w[]
        s.files = %w[lib/a.rb]

        s.add_dependency "b"
      end

      fetcher.gem "b", 2 do |s|
        s.executables = %w[a]
        s.files = %w[bin/a lib/b.rb]

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    util_clear_gems

    use_ui @ui do
      @cmd.invoke "a:2"
      assert_equal "b-2 a\n", @ui.output
    end
  end

  def test_gem_dependency_contains_other_executable
    spec_fetcher do |fetcher|
      fetcher.gem "a", 2 do |s|
        s.executables = %w[]
        s.files = %w[lib/a.rb]

        s.add_dependency "b"
      end

      fetcher.gem "b", 2 do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/b.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    util_clear_gems

    use_ui @ui do
      e = assert_raise Gem::MockGemUi::TermError do
        @cmd.invoke "a:2"
      end
      assert_equal 1, e.exit_code
      assert_equal <<~ERR, @ui.error
        ERROR:  Failed to load executable `a`, are you sure the gem `a` contains it?
      ERR
    end
  end

  def test_other_gem_contains_executable
    spec_fetcher do |fetcher|
      fetcher.gem "a", 2 do |s|
        s.executables = %w[]
        s.files = %w[lib/a.rb]
      end

      fetcher.gem "b", 2 do |s|
        s.executables = %w[a]
        s.files = %w[bin/a lib/b.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    util_clear_gems

    use_ui @ui do
      e = assert_raise Gem::MockGemUi::TermError do
        @cmd.invoke "a:2"
      end
      assert_equal 1, e.exit_code
      assert_equal <<~ERR, @ui.error
        ERROR:  Failed to load executable `a`, are you sure the gem `a` contains it?
      ERR
    end
  end

  def test_missing_gem
    spec_fetcher do |fetcher|
    end

    use_ui @ui do
      e = assert_raise Gem::MockGemUi::TermError do
        @cmd.invoke "a"
      end
      assert_equal 2, e.exit_code
      assert_equal <<~ERR, @ui.error
        ERROR:  Could not find a valid gem 'a' (>= 0) in any repository
      ERR
    end
  end

  def test_version_mismatch
    spec_fetcher do |fetcher|
      fetcher.gem "a", 1
    end

    util_clear_gems

    use_ui @ui do
      e = assert_raise Gem::MockGemUi::TermError do
        @cmd.invoke "a:2"
      end
      assert_equal 2, e.exit_code
      assert_equal <<~ERR, @ui.error
        ERROR:  Could not find a valid gem 'a' (= 2) in any repository
      ERR
    end
  end

  def test_pre_argument
    spec_fetcher do |fetcher|
      fetcher.gem "a", 1 do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
      fetcher.gem "a", "1.1.a" do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    util_clear_gems

    use_ui @ui do
      @cmd.invoke "--pre", "a"
      assert_equal "a-1.1.a foo\n", @ui.output
    end
  end

  def test_pre_version_option
    spec_fetcher do |fetcher|
      fetcher.download "a", 1 do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
      fetcher.download "a", "1.1.a" do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    use_ui @ui do
      @cmd.invoke "-v", ">= 0.a", "a"
      assert_equal "a-1.1.a foo\n", @ui.output
    end
  end

  def test_conservative_missing_gem
    spec_fetcher do |fetcher|
      fetcher.gem "a", 1 do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    util_clear_gems

    use_ui @ui do
      e = assert_raise Gem::MockGemUi::TermError do
        @cmd.invoke "--verbose", "--conservative", "a:2"
      end
      assert_equal 2, e.exit_code
      assert_include @ui.output, "a (= 2) not available locally"
      assert_equal <<~ERROR, @ui.error
        ERROR:  Could not find a valid gem 'a' (= 2) in any repository
      ERROR
    end
  end

  def test_conservative
    spec_fetcher do |fetcher|
      fetcher.download "a", 1 do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    use_ui @ui do
      invoke "--verbose", "--conservative", "a"
      assert_include @ui.output, "a (>= 0) not available locally"
      assert_include @ui.output, "a-1 foo"
      assert_equal %w[a-1], @installed_specs.map(&:original_name)
    end

    spec_fetcher do |fetcher|
      fetcher.gem "a", 1 do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end

      fetcher.download "a", 2 do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    use_ui @ui do
      invoke "--verbose", "--conservative", "a"
      assert_not_include @ui.output, "a (>= 0) not available locally"
      assert_include @ui.output, "a-1 foo"
      assert_empty @installed_specs.map(&:original_name)
    end
  end

  def test_uses_newest_version
    spec_fetcher do |fetcher|
      fetcher.download "a", 1 do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    use_ui @ui do
      invoke "a"
      assert_include @ui.output, "a-1 foo"
    end

    spec_fetcher do |fetcher|
      fetcher.download "a", 1 do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end

      fetcher.download "a", 2 do |s|
        s.executables = %w[foo]
        s.files = %w[bin/foo lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    use_ui @ui do
      invoke "--verbose", "a:2"
      refute_predicate @ui, :terminated?
      assert_empty @ui.error
      assert_include @ui.output, "a-2 foo"
      assert_equal %w[a-2], @installed_specs.map(&:original_name)
    end
  end

  def test_uses_newest_version_of_dependency
    spec_fetcher do |fetcher|
      fetcher.gem "a", 1 do |s|
        s.executables = %w[]
        s.files = %w[lib/a.rb]
        s.add_dependency "b"
      end

      fetcher.gem "b", 1 do |s|
        s.executables = %w[a]
        s.files = %w[bin/a lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end

      fetcher.download "b", 2 do |s|
        s.executables = %w[a]
        s.files = %w[bin/a lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    use_ui @ui do
      invoke "a"
      assert_include @ui.output, "b-2 a"
      assert_equal %w[b-2], @installed_specs.map(&:original_name)
    end
  end

  def test_gem_exec_gem_uninstall
    spec_fetcher do |fetcher|
      fetcher.download "a", 2 do |s|
        s.executables = %w[a]
        s.files = %w[bin/a lib/a.rb]
        s.add_dependency "b"

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "Gem.ui.say #{s.original_name.dump}"
        end
      end

      fetcher.download "b", 2 do |s|
        s.files = %w[lib/b.rb]
      end
    end

    use_ui @ui do
      invoke "a:2"
      assert_equal "a-2\n", @ui.output

      invoke "gem", "list", "--local"
      assert_includes @ui.output, "a (2)\n"
      assert_includes @ui.output, "b (2)\n"

      begin
        invoke "gem", "uninstall", "--verbose", "-x", "a"
      rescue StandardError
        nil
      end

      assert_empty @ui.error
      refute_includes @ui.output, "running gem exec with"
      assert_includes @ui.output, "Successfully uninstalled a-2\n"

      invoke "--verbose", "gem", "uninstall", "b"
      assert_includes @ui.output, "Successfully uninstalled b-2\n"

      invoke "gem", "list", "--local"
      assert_empty @ui.error
      assert_match(/\A\s*\** LOCAL GEMS \**\s*\z/m, @ui.output)

      invoke "gem", "env", "GEM_HOME"
      assert_equal "#{@gem_home}\n", @ui.output
    end
  end

  def test_only_prerelease_available
    spec_fetcher do |fetcher|
      fetcher.download "a", "1.a" do |s|
        s.executables = %w[a]
        s.files = %w[lib/a.rb bin/a]

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    use_ui @ui do
      assert_raise Gem::MockGemUi::TermError do
        invoke "a"
      end
      assert_equal "ERROR:  Could not find a valid gem 'a' (>= 0) in any repository\n", @ui.error
      assert_empty @ui.output
      assert_empty @installed_specs
    end

    use_ui @ui do
      invoke "a:1.a"
      assert_empty @ui.error
      assert_equal "a-1.a a\n", @ui.output
      assert_equal %w[a-1.a], @installed_specs.map(&:full_name)
    end

    FileUtils.rm_rf Gem.dir

    use_ui @ui do
      invoke "--version", ">= 1.a", "a"
      assert_empty @ui.error
      assert_equal "a-1.a a\n", @ui.output
      assert_equal %w[a-1.a], @installed_specs.map(&:full_name)
    end

    FileUtils.rm_rf Gem.dir

    use_ui @ui do
      invoke "--pre", "a"
      assert_empty @ui.error
      assert_equal "a-1.a a\n", @ui.output
      assert_equal %w[a-1.a], @installed_specs.map(&:full_name)
    end
  end

  def test_newer_prerelease_available
    spec_fetcher do |fetcher|
      fetcher.download "a", "1" do |s|
        s.executables = %w[a]
        s.files = %w[lib/a.rb bin/a]

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end

      fetcher.download "a", "1.1.a" do |s|
        s.executables = %w[a]
        s.files = %w[lib/a.rb bin/a]

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "Gem.ui.say #{s.original_name.dump} + ' ' + File.basename(__FILE__)"
        end
      end
    end

    use_ui @ui do
      invoke "a"
      assert_empty @ui.error
      assert_equal "a-1 a\n", @ui.output
      assert_equal %w[a-1], @installed_specs.map(&:full_name)
    end

    FileUtils.rm_rf Gem.dir

    use_ui @ui do
      invoke "a:1.1.a"
      assert_empty @ui.error
      assert_equal "a-1.1.a a\n", @ui.output
      assert_equal %w[a-1.1.a], @installed_specs.map(&:full_name)
    end

    FileUtils.rm_rf Gem.dir

    use_ui @ui do
      invoke "--version", ">= 1.a", "a"
      assert_empty @ui.error
      assert_equal "a-1.1.a a\n", @ui.output
      assert_equal %w[a-1.1.a], @installed_specs.map(&:full_name)
    end

    FileUtils.rm_rf Gem.dir

    use_ui @ui do
      invoke "--pre", "a"
      assert_empty @ui.error
      assert_equal "a-1.1.a a\n", @ui.output
      assert_equal %w[a-1.1.a], @installed_specs.map(&:full_name)
    end
  end

  def test_install_dependency_resolution_error
    spec_fetcher do |fetcher|
      fetcher.gem "a", 2 do |s|
        s.executables = %w[a]
        s.add_dependency "b", "~> 1.0"
        s.add_dependency "c", "~> 1.0"
      end
      fetcher.gem "b", 1 do |s|
        s.add_dependency "d", "= 1.0"
      end
      fetcher.gem "c", 1 do |s|
        s.add_dependency "d", "= 2.0"
      end
      fetcher.gem "d", 1
      fetcher.gem "d", 2
    end

    util_clear_gems

    use_ui @ui do
      e = assert_raise Gem::MockGemUi::TermError do
        @cmd.invoke "a:2"
      end
      assert_equal 2, e.exit_code
    end

    assert_match(/ERROR:.*Error installing a:/, @ui.error)
  end

  # Installs two versions of a gem that both provide an executable and both
  # require the same file name, whose constant records the version. The parent
  # is meant to already have 1.x loaded, and each +gem exec+ is expected to
  # observe only its own version.

  def install_isolated_probe_tool
    %w[1.0 2.0].each do |version|
      dep = quick_gem "probe-dep", version do |s|
        s.files = %w[lib/probe-dep.rb]
      end

      write_file File.join(*%W[gems #{dep.full_name} lib probe-dep.rb]) do |f|
        f << "PROBE_DEP_MARK = #{version.dump}\n"
      end

      util_build_gem dep
      install_gem dep, force: true

      spec = quick_gem "probe-tool", version do |s|
        s.executables = %w[probe-tool]
        s.files = %w[bin/probe-tool lib/probe-tool.rb]
        s.add_dependency "probe-dep", "= #{version}"
      end

      write_file File.join(*%W[gems #{spec.full_name} lib probe-tool.rb]) do |f|
        f << "PROBE_TOOL_MARK = #{version.dump}\n"
      end

      write_file File.join(*%W[gems #{spec.full_name} bin probe-tool]) do |f|
        f << <<~RUBY
          #!#{Gem.ruby}
          require "probe-tool"
          require "probe-dep"
          puts PROBE_TOOL_MARK
          puts PROBE_DEP_MARK
          exit(ARGV.include?("boom") ? 7 : 0)
        RUBY
      end

      util_build_gem spec
      install_gem spec, force: true
    end

    Gem::Specification.reset
  end

  def test_isolated_returns_streams_and_exit_status_of_the_tool
    install_isolated_probe_tool

    result = run_isolated "probe-tool:2.0", "--", "boom"

    assert_equal 7, result.exitstatus
    refute_predicate result, :success?
    assert_equal "2.0\n2.0\n", result.stdout
    assert_empty result.stderr
  end

  def test_isolated_honors_each_version_and_its_dependencies_in_turn
    install_isolated_probe_tool

    first = run_isolated "probe-tool:2.0", "--", "boom"
    second = run_isolated "probe-tool:1.0"
    third = run_isolated "probe-tool:2.0", "--", "boom"

    assert_equal "2.0\n2.0\n", first.stdout
    assert_equal "1.0\n1.0\n", second.stdout
    assert_equal "2.0\n2.0\n", third.stdout
    assert_equal [7, 0, 7], [first, second, third].map(&:exitstatus)
  end

  def test_isolated_keeps_parent_activation_loads_and_argv
    install_isolated_probe_tool

    gem "probe-tool", "= 1.0"
    require "probe-tool"
    require "probe-dep"

    parent_mark = Object.const_get(:PROBE_TOOL_MARK)
    parent_dep_mark = Object.const_get(:PROBE_DEP_MARK)
    parent_active = Gem.loaded_specs.fetch("probe-tool").version
    parent_argv = ["--fail", "keep"]

    old_argv = ARGV.dup
    ARGV.replace parent_argv
    parent_gem_files = $LOADED_FEATURES.grep(%r{/gems/})
    specs_before = Gem.loaded_specs.keys.sort

    begin
      first = run_isolated "probe-tool:2.0", "--", "boom"
      second = run_isolated "probe-tool:1.0"

      assert_equal 7, first.exitstatus
      assert_equal "2.0\n2.0\n", first.stdout
      assert_predicate second, :success?
      assert_equal "1.0\n1.0\n", second.stdout

      assert_equal parent_mark, Object.const_get(:PROBE_TOOL_MARK)
      assert_equal parent_dep_mark, Object.const_get(:PROBE_DEP_MARK)
      assert_equal parent_active, Gem.loaded_specs.fetch("probe-tool").version
      assert_equal specs_before, Gem.loaded_specs.keys.sort
      assert_equal parent_argv, ARGV
      assert_equal parent_gem_files, $LOADED_FEATURES.grep(%r{/gems/})
    ensure
      ARGV.replace old_argv
      Gem::Specification.unresolved_deps.clear
      Gem.loaded_specs.clear
      Gem.instance_variable_set(:@activated_gem_paths, 0)
      Gem.use_paths(@gem_home, @gem_path)
      Gem.refresh
    end
  end

  def test_isolated_reports_nonzero_exit_through_terminate_interaction
    install_isolated_probe_tool

    use_ui @ui do
      e = assert_raise Gem::MockGemUi::TermError do
        @cmd.invoke "--isolated", "probe-tool:2.0", "--", "boom"
      end
      assert_equal 7, e.exit_code
    end

    assert_equal "2.0\n2.0\n", @ui.output
    assert_empty @ui.error
  end

  def test_isolated_leaves_parent_untouched_after_a_crashing_tool
    install_isolated_probe_tool

    gem "probe-tool", "= 1.0"
    require "probe-tool"

    old_argv = ARGV.dup
    ARGV.replace ["--fail"]

    begin
      assert_raise Gem::MockGemUi::TermError do
        @cmd.invoke "--isolated", "probe-tool:2.0", "--", "boom"
      end
    ensure
      ARGV.replace old_argv
    end

    assert_equal 7, @cmd.isolated_result.exitstatus
    assert_equal "1.0", Object.const_get(:PROBE_TOOL_MARK)
    assert_equal Gem::Version.new("1.0"), Gem.loaded_specs.fetch("probe-tool").version
  end

  def test_isolated_restores_argv_and_gem_paths_when_the_child_cannot_start
    install_isolated_probe_tool

    original_home = ENV["GEM_HOME"]
    original_path = ENV["GEM_PATH"]
    original_argv = ARGV.dup

    cmd = Gem::Commands::ExecCommand.new
    cmd.send :handle_options, ["probe-tool:1.0"]

    begin
      ARGV.replace ["--fail"]

      cmd.stub :ruby_interpreter, File.join(@tempdir, "missing-ruby") do
        assert_raise Errno::ENOENT do
          cmd.isolated
        end
      end

      assert_equal original_home, ENV["GEM_HOME"]
      assert_equal original_path, ENV["GEM_PATH"]
      assert_equal @gem_home, Gem.dir
      assert_equal ["--fail"], ARGV
    ensure
      ARGV.replace original_argv
    end
  end

  def test_isolated_installs_the_gem_when_it_is_not_local
    spec_fetcher do |fetcher|
      fetcher.download "a", 2 do |s|
        s.executables = %w[a]
        s.files = %w[lib/a.rb bin/a]

        write_file File.join(*%W[gems #{s.original_name} bin a]) do |f|
          f << "puts #{s.original_name.dump}\n"
        end
      end
    end

    use_ui @ui do
      result = run_isolated "a:2"
      assert_predicate result, :success?
      assert_equal "a-2\n", result.stdout
    end

    assert_equal %w[a-2], @installed_specs.map(&:full_name)
  end

  def test_isolated_gem_command_runs_in_a_child_process
    use_ui @ui do
      @cmd.invoke "--isolated", "--gem", "gem", "--", "gem", "env", "GEM_HOME"
    end

    assert_predicate @cmd.isolated_result, :success?
    assert_equal "#{@gem_home}\n", @cmd.isolated_result.stdout.gsub("\r\n", "\n")
  end

  def test_isolated_propagates_a_tool_crash_without_polluting_the_next_run
    %w[1.0 2.0].each do |version|
      spec = quick_gem "probe-crash", version do |s|
        s.executables = %w[probe-crash]
        s.files = %w[bin/probe-crash]
      end

      body = if version == "2.0"
        'warn "about to crash"' + "\n" + 'raise "tool blew up"'
      else
        "puts #{version.dump}"
      end

      write_file File.join(*%W[gems #{spec.full_name} bin probe-crash]) do |f|
        f << "#!#{Gem.ruby}\n#{body}\n"
      end

      util_build_gem spec
      install_gem spec, force: true
    end

    Gem::Specification.reset

    crashed = run_isolated "probe-crash:2.0"
    refute_predicate crashed, :success?
    assert_operator crashed.exitstatus, :>, 0
    assert_include crashed.stderr, "tool blew up"

    healthy = run_isolated "probe-crash:1.0"
    assert_predicate healthy, :success?
    assert_equal "1.0\n", healthy.stdout
    assert_equal 0, healthy.exitstatus
  end

  def test_isolated_reports_ambiguous_executable_like_the_in_process_mode
    spec_fetcher do |fetcher|
      fetcher.gem "a", 2 do |s|
        s.executables = %w[foo bar]
        s.files = %w[bin/foo bin/bar lib/a.rb]

        write_file File.join(*%W[gems #{s.original_name} bin foo]) do |f|
          f << "puts #{s.original_name.dump}\n"
        end

        write_file File.join(*%W[gems #{s.original_name} bin bar]) do |f|
          f << "puts #{s.original_name.dump}\n"
        end
      end
    end

    util_clear_gems

    use_ui @ui do
      e = assert_raise Gem::MockGemUi::TermError do
        @cmd.invoke "--isolated", "a:2"
      end
      assert_equal 1, e.exit_code
    end

    assert_equal <<~ERR, @ui.error
      ERROR:  Ambiguous which executable from gem `a` should be run: the options are ["bar", "foo"], specify one via COMMAND, and use `-g` and `-v` to specify gem and version
    ERR
  end

  def run_isolated(*args)
    cmd = Gem::Commands::ExecCommand.new
    cmd.send :handle_options, args

    result = nil
    ui = Gem::MockGemUi.new
    Gem::DefaultUserInteraction.use_ui(ui) do
      result = cmd.isolated
    end
    ui.close
    result
  end

  def remove_probe_constants
    %i[PROBE_TOOL_MARK PROBE_DEP_MARK].each do |name|
      Object.send :remove_const, name if Object.const_defined?(name, false)
    end
  end
end
