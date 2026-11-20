# frozen_string_literal: true

require_relative "helper"
require "rubygems/commands/pristine_command"
require "rubygems/ext"

class TestGemCommandsPristineCommand < Gem::TestCase
  def setup
    super
    common_installer_setup

    @cmd = Gem::Commands::PristineCommand.new
  end

  def test_execute_content_addressed_gem
    _, a_gem = util_gem("a", 2) do |spec|
      spec.required_ruby_version = "~> #{Gem.ruby_abi}.0"
      spec.platform = Gem::Platform.local.to_s
    end

    address = Digest::SHA256.file(a_gem).hexdigest[0, 8]
    filename = File.join(File.dirname(a_gem), "a-2-#{address}.gem")
    FileUtils.cp a_gem, filename

    Gem::Installer.at(filename, force: true).install
    Gem::Specification.reset

    gem_dir = File.join(@gemhome, "gems", "a-2-#{address}")
    gemspec = File.join(@gemhome, "specifications", Gem.ruby_abi, "a-2-#{address}.gemspec")
    assert_path_exist gem_dir
    assert_path_exist gemspec

    FileUtils.rm_rf gem_dir

    @cmd.options[:args] = %w[a]

    use_ui @ui do
      @cmd.execute
    end

    assert_path_exist gem_dir
    assert_path_exist gemspec
    assert_path_not_exist File.join(@gemhome, "specifications", "a-2-#{address}.gemspec")
  end

  def test_execute
    a = util_spec "a" do |s|
      s.executables = %w[foo]
      s.files = %w[bin/foo lib/a.rb]
    end

    write_file File.join(@tempdir, "lib", "a.rb") do |fp|
      fp.puts "puts __FILE__"
    end
    write_file File.join(@tempdir, "bin", "foo") do |fp|
      fp.puts "#!/usr/bin/ruby"
    end

    install_gem a

    foo_path  = File.join @gemhome, "gems", a.full_name, "bin", "foo"
    a_rb_path = File.join @gemhome, "gems", a.full_name, "lib", "a.rb"

    write_file foo_path do |io|
      io.puts "I changed it!"
    end

    write_file a_rb_path do |io|
      io.puts "I changed it!"
    end

    @cmd.options[:args] = %w[a]

    use_ui @ui do
      @cmd.execute
    end

    assert_equal "#!/usr/bin/ruby\n", File.read(foo_path), foo_path
    assert_equal "puts __FILE__\n", File.read(a_rb_path), a_rb_path

    out = @ui.output.split "\n"

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Restored #{a.full_name}", out.shift
    assert_empty out, out.inspect
  end

  def test_execute_user_install
    FileUtils.chmod 0o555, @gemhome

    a = util_spec "a" do |s|
      s.executables = %w[foo]
      s.files = %w[bin/foo lib/a.rb]
    end

    write_file File.join(@tempdir, "lib", "a.rb") do |fp|
      fp.puts "puts __FILE__"
    end

    write_file File.join(@tempdir, "bin", "foo") do |fp|
      fp.puts "#!/usr/bin/ruby"
    end

    install_gem_user(a)

    Gem::Specification.dirs = [Gem.dir, Gem.user_dir]

    foo_path  = File.join(Gem.user_dir, "gems", a.full_name, "bin", "foo")
    a_rb_path = File.join(Gem.user_dir, "gems", a.full_name, "lib", "a.rb")

    write_file foo_path do |io|
      io.puts("I changed it!")
    end

    write_file a_rb_path do |io|
      io.puts("I changed it!")
    end

    @cmd.options[:args] = %w[a]

    use_ui @ui do
      @cmd.execute
    end

    assert_equal "#!/usr/bin/ruby\n", File.read(foo_path), foo_path
    assert_equal "puts __FILE__\n", File.read(a_rb_path), a_rb_path

    out = @ui.output.split("\n")

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Restored #{a.full_name} in #{Gem.user_dir}", out.shift
    assert_empty out, out.inspect
  ensure
    FileUtils.chmod(0o755, @gemhome)
  end

  def test_execute_all
    a = util_spec "a" do |s|
      s.executables = %w[foo]
    end

    write_file File.join(@tempdir, "bin", "foo") do |fp|
      fp.puts "#!/usr/bin/ruby"
    end

    install_gem a

    gem_bin  = File.join @gemhome, "gems", a.full_name, "bin", "foo"
    gem_stub = File.join @gemhome, "bin", "foo"

    FileUtils.rm gem_bin
    FileUtils.rm gem_stub

    @cmd.handle_options %w[--all]

    use_ui @ui do
      @cmd.execute
    end

    assert_path_exist gem_bin
    assert_path_exist gem_stub

    out = @ui.output.split "\n"

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Restored #{a.full_name}", out.shift
    assert_empty out, out.inspect
  end

  def test_execute_env_shebang
    a = util_spec "a" do |s|
      s.executables = %w[foo]
      s.files = %w[bin/foo]
    end
    write_file File.join(@tempdir, "bin", "foo") do |fp|
      fp.puts "#!/usr/bin/ruby"
    end

    install_gem a

    gem_exec = File.join @gemhome, "bin", "foo"

    FileUtils.rm gem_exec

    @cmd.handle_options %w[--all --env-shebang]

    use_ui @ui do
      @cmd.execute
    end

    assert_path_exist gem_exec

    ruby_exec = format Gem.default_exec_format, "ruby"

    bin_env = Gem.win_platform? ? "" : %w[/usr/bin/env /bin/env].find {|f| File.executable?(f) } + " "

    assert_match(/\A#!\s*#{bin_env}#{ruby_exec}/, File.read(gem_exec))
  end

  def test_execute_extensions_explicit
    a = util_spec "a" do |s|
      s.extensions << "ext/a/extconf.rb"
    end

    ext_path = File.join @tempdir, "ext", "a", "extconf.rb"
    write_file ext_path do |io|
      io.write <<-'RUBY'
      File.open "Makefile", "w" do |f|
        f.puts "clean:\n\techo cleaned\n"
        f.puts "all:\n\techo built\n"
        f.puts "install:\n\techo installed\n"
      end
      RUBY
    end

    b = util_spec "b"

    install_gem a
    install_gem b

    @cmd.options[:extensions]     = true
    @cmd.options[:extensions_set] = true
    @cmd.options[:args] = []

    use_ui @ui do
      @cmd.execute
    end

    out = @ui.output.split "\n"

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Restoring extensions for Ruby ABI #{Gem.ruby_abi}", out.shift
    assert_equal "Building native extensions. This could take a while...",
                 out.shift
    assert_equal "Restored #{a.full_name}", out.shift
    assert_empty out, out.inspect
  end

  def test_execute_extensions_only_missing_extensions
    a = util_spec "a" do |s|
      s.extensions << "ext/a/extconf.rb"
    end

    ext_path = File.join @tempdir, "ext", "a", "extconf.rb"
    write_file ext_path do |io|
      io.write <<-'RUBY'
      File.open "Makefile", "w" do |f|
        f.puts "clean:\n\techo cleaned\n"
        f.puts "all:\n\techo built\n"
        f.puts "install:\n\techo installed\n"
      end
      RUBY
    end

    b = util_spec "b" do |s|
      s.extensions << "ext/b/extconf.rb"
    end

    ext_path = File.join @tempdir, "ext", "b", "extconf.rb"
    write_file ext_path do |io|
      io.write <<-'RUBY'
      File.open "Makefile", "w" do |f|
        f.puts "clean:\n\techo cleaned\n"
        f.puts "all:\n\techo built\n"
        f.puts "install:\n\techo installed\n"
      end
      RUBY
    end

    install_gem a
    install_gem b

    # Remove the extension files for b
    FileUtils.rm_rf b.gem_build_complete_path

    @cmd.options[:only_missing_extensions] = true
    @cmd.options[:args] = []

    use_ui @ui do
      @cmd.execute
    end

    refute_includes @ui.output, "Restored #{a.full_name}"

    if Gem.java_platform?
      refute_includes @ui.output, "Restored #{b.full_name}"
      assert_includes @ui.output, "No gems with missing extensions to restore"
    else
      assert_includes @ui.output, "Restoring extensions for Ruby ABI #{Gem.ruby_abi} (only missing)"
      assert_includes @ui.output, "Restored #{b.full_name}"
    end
  end

  def test_execute_extensions_scoped_to_current_ruby_abi
    a = util_spec_with_unbuilt_extension "a"

    # Another Ruby on the same runner left its own native build of the
    # same gem behind, in the extension directory of its own ABI.
    other_extension_dir = File.join(
      @gemhome, "extensions", Gem::Platform.local.to_s, "0.0.0-other", a.full_name
    )
    FileUtils.mkdir_p other_extension_dir
    FileUtils.touch File.join(other_extension_dir, "gem.build_complete")
    FileUtils.touch File.join(other_extension_dir, "a.so")

    built_dirs = []

    @cmd.options[:args] = %w[a]

    Gem::Ext::Builder.stub(:new, recording_builder_factory(built_dirs)) do
      use_ui @ui do
        @cmd.execute
      end
    end

    # Only the current Ruby ABI's extension directory was written
    assert_equal [a.extension_dir], built_dirs
    assert_path_exist a.gem_build_complete_path

    # The other Ruby's extension directory was left untouched
    assert_equal %w[a.so gem.build_complete], Dir.children(other_extension_dir).sort

    out = @ui.output.split "\n"

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Restoring extensions for Ruby ABI #{Gem.ruby_abi}", out.shift
    assert_equal "Restored #{a.full_name}", out.shift
    assert_empty out, out.inspect
  end

  def test_execute_extensions_with_current_ruby_abi_option
    a = util_spec_with_unbuilt_extension "a"

    built_dirs = []

    @cmd.handle_options ["a", "--ruby-abi", Gem.ruby_abi]

    Gem::Ext::Builder.stub(:new, recording_builder_factory(built_dirs)) do
      use_ui @ui do
        @cmd.execute
      end
    end

    assert_equal [a.extension_dir], built_dirs
    assert_path_exist a.gem_build_complete_path

    assert_includes @ui.output, "Restoring extensions for Ruby ABI #{Gem.ruby_abi}"
    assert_includes @ui.output, "Restored #{a.full_name}"
  end

  def test_execute_extensions_with_other_ruby_abi_is_skipped
    a = util_spec_with_unbuilt_extension "a"

    other_abi = Gem.ruby_abi == "9.9" ? "8.8" : "9.9"

    built_dirs = []

    @cmd.handle_options ["a", "--ruby-abi", other_abi]

    Gem::Ext::Builder.stub(:new, recording_builder_factory(built_dirs)) do
      use_ui @ui do
        @cmd.execute
      end
    end

    # Nothing was built: the gem's extensions belong to the running Ruby,
    # not to the ABI chosen on the command line
    assert_empty built_dirs
    assert_path_not_exist a.gem_build_complete_path

    out = @ui.output.split "\n"

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Restoring extensions for Ruby ABI #{other_abi}", out.shift
    assert_equal "Skipped #{a.full_name}, its extensions belong to Ruby ABI #{Gem.ruby_abi}",
                 out.shift
    assert_empty out, out.inspect
  end

  def test_execute_only_missing_extensions_scoped_to_current_ruby_abi
    a = util_spec_with_unbuilt_extension "a"
    b = util_spec_with_unbuilt_extension "b"

    # b's artifacts for the current ABI are present, a's are missing
    FileUtils.mkdir_p b.extension_dir
    FileUtils.touch b.gem_build_complete_path

    built_dirs = []

    @cmd.options[:only_missing_extensions] = true
    @cmd.options[:args] = []

    Gem::Ext::Builder.stub(:new, recording_builder_factory(built_dirs)) do
      use_ui @ui do
        @cmd.execute
      end
    end

    assert_equal [a.extension_dir], built_dirs
    assert_path_exist a.gem_build_complete_path
    assert_path_exist b.gem_build_complete_path

    assert_includes @ui.output,
                    "Restoring extensions for Ruby ABI #{Gem.ruby_abi} (only missing)"
    assert_includes @ui.output, "Restored #{a.full_name}"
    refute_includes @ui.output, "Restored #{b.full_name}"
  end

  def test_execute_failed_extension_build_leaves_no_build_complete
    a = util_spec_with_unbuilt_extension "a"

    @cmd.options[:args] = %w[a]

    Gem::Ext::Builder.stub(:new, failing_builder_factory) do
      use_ui @ui do
        assert_raise(Gem::Ext::BuildError) do
          @cmd.execute
        end
      end
    end

    # A failed build must not leave the "extensions are built" marker
    # behind, nor may the gem be reported as restored
    assert_path_not_exist a.gem_build_complete_path
    refute_includes @ui.output, "Restored #{a.full_name}"
  end

  def test_execute_no_extension
    a = util_spec "a" do |s|
      s.extensions << "ext/a/extconf.rb"
    end

    ext_path = File.join @tempdir, "ext", "a", "extconf.rb"
    write_file ext_path do |io|
      io.write "# extconf.rb\nrequire 'mkmf'; create_makefile 'a'"
    end

    install_gem a

    @cmd.options[:args] = %w[a]
    @cmd.options[:extensions] = false

    use_ui @ui do
      @cmd.execute
    end

    out = @ui.output.split "\n"

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Skipped #{a.full_name}, it needs to compile an extension",
                 out.shift
    assert_empty out, out.inspect
  end

  def test_execute_with_extension_with_build_args
    a = util_spec "a" do |s|
      s.extensions << "ext/a/extconf.rb"
    end

    ext_path = File.join @tempdir, "ext", "a", "extconf.rb"
    write_file ext_path do |io|
      io.write <<-'RUBY'
      File.open "Makefile", "w" do |f|
        f.puts "clean:\n\techo cleaned\n"
        f.puts "all:\n\techo built\n"
        f.puts "install:\n\techo installed\n"
      end
      RUBY
    end

    build_args = %w[--with-awesome=true --sweet]

    install_gem a, build_args: build_args

    @cmd.options[:args] = %w[a]

    use_ui @ui do
      @cmd.execute
    end

    out = @ui.output.split "\n"

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Restoring extensions for Ruby ABI #{Gem.ruby_abi}", out.shift
    assert_equal "Building native extensions with: '--with-awesome=true --sweet'", out.shift
    assert_equal "This could take a while...", out.shift
    assert_equal "Restored #{a.full_name}", out.shift
    assert_empty out, out.inspect
  end

  def test_execute_many
    a = util_spec "a"
    b = util_spec "b"

    install_gem a
    install_gem b

    @cmd.options[:args] = %w[a b]

    use_ui @ui do
      @cmd.execute
    end

    out = @ui.output.split "\n"

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Restored #{a.full_name}", out.shift
    assert_equal "Restored #{b.full_name}", out.shift
    assert_empty out, out.inspect
  end

  def test_skip
    a = util_spec "a"
    b = util_spec "b"

    install_gem a
    install_gem b

    @cmd.options[:args] = %w[a b]
    @cmd.options[:skip] = "a"

    use_ui @ui do
      @cmd.execute
    end

    out = @ui.output.split "\n"

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Skipped #{a.full_name}, it was given through options", out.shift
    assert_equal "Restored #{b.full_name}", out.shift
    assert_empty out, out.inspect
  end

  def test_skip_many_gems
    a = util_spec "a"
    b = util_spec "b"
    c = util_spec "c"

    install_gem a
    install_gem b
    install_gem c

    @cmd.options[:args] = %w[a b c]
    @cmd.options[:skip] = ["a", "c"]

    use_ui @ui do
      @cmd.execute
    end

    out = @ui.output.split "\n"

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Skipped #{a.full_name}, it was given through options", out.shift
    assert_equal "Restored #{b.full_name}", out.shift
    assert_equal "Skipped #{c.full_name}, it was given through options", out.shift
    assert_empty out, out.inspect
  end

  def test_execute_many_multi_repo
    a = util_spec "a"
    install_gem a

    Gem.clear_paths
    gemhome2 = File.join @tempdir, "gemhome2"
    Gem.use_paths gemhome2, [gemhome2, @gemhome]

    b = util_spec "b"
    install_gem b

    assert_path_exist File.join(gemhome2, "gems", "b-2")
    assert_path_not_exist File.join(@gemhome, "gems", "b-2")

    @cmd.options[:args] = %w[a b]

    use_ui @ui do
      @cmd.execute
    end

    out = @ui.output.split "\n"

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Restored #{a.full_name} in #{@gemhome}", out.shift
    assert_equal "Restored #{b.full_name}", out.shift
    assert_empty out, out.inspect

    assert_path_exist File.join(@gemhome, "gems", "a-2")
    assert_path_not_exist File.join(gemhome2, "gems", "a-2")
    assert_path_exist File.join(gemhome2, "gems", "b-2")
    assert_path_not_exist File.join(@gemhome, "gems", "b-2")
  end

  def test_execute_missing_cache_gem
    specs = spec_fetcher do |fetcher|
      fetcher.gem "a", 1
      fetcher.gem "a", 2
      fetcher.gem "a", 3
      fetcher.gem "a", "3.a"
    end

    FileUtils.rm specs["a-2"].cache_file

    @cmd.options[:args] = %w[a]

    use_ui @ui do
      @cmd.execute
    end

    out = @ui.output.split "\n"

    [
      "Restoring gems to pristine condition...",
      "Restored a-1",
      "Cached gem for a-2 not found, attempting to fetch...",
      "Restored a-2",
      "Restored a-3.a",
      "Restored a-3",
    ].each do |line|
      assert_equal line, out.shift
    end

    assert_empty out, out.inspect
  end

  def test_execute_missing_cache_gem_when_multi_repo
    specs = spec_fetcher do |fetcher|
      fetcher.gem "a", 1
      fetcher.gem "b", 1
    end

    FileUtils.rm_rf File.join(@gemhome, "gems", "a-1")
    FileUtils.rm_rf File.join(@gemhome, "gems", "b-1")

    install_gem specs["a-1"]
    FileUtils.rm File.join(@gemhome, "cache", "a-1.gem")

    Gem.clear_paths
    gemhome2 = File.join(@tempdir, "gemhome2")
    Gem.use_paths gemhome2, [gemhome2, @gemhome]

    install_gem specs["b-1"]
    FileUtils.rm File.join(gemhome2, "cache", "b-1.gem")
    Gem::Specification.reset

    @cmd.options[:args] = %w[a b]

    use_ui @ui do
      @cmd.execute
    end

    out = @ui.output.split "\n"

    [
      "Restoring gems to pristine condition...",
      "Cached gem for a-1 in #{@gemhome} not found, attempting to fetch...",
      "Restored a-1 in #{@gemhome}",
      "Restored b-1 in #{@gemhome}",
      "Cached gem for b-1 not found, attempting to fetch...",
      "Restored b-1",
    ].each do |line|
      assert_equal line, out.shift
    end

    assert_empty out, out.inspect
    assert_empty @ui.error

    assert_path_exist File.join(@gemhome, "cache", "a-1.gem")
    assert_path_not_exist File.join(gemhome2, "cache", "a-2.gem")
    assert_path_exist File.join(@gemhome, "gems", "a-1")
    assert_path_not_exist File.join(gemhome2, "gems", "a-1")

    assert_path_exist File.join(gemhome2, "cache", "b-1.gem")
    assert_path_not_exist File.join(@gemhome, "cache", "b-2.gem")
    assert_path_exist File.join(gemhome2, "gems", "b-1")
    assert_path_exist File.join(@gemhome, "gems", "b-1")
  end

  def test_execute_no_gem
    @cmd.options[:args] = %w[]

    e = assert_raise Gem::CommandLineError do
      use_ui @ui do
        @cmd.execute
      end
    end

    assert_match(/at least one gem name/, e.message)
  end

  def test_execute_only_executables
    a = util_spec "a" do |s|
      s.executables = %w[foo]
      s.files = %w[bin/foo lib/a.rb]
    end
    write_file File.join(@tempdir, "lib", "a.rb") do |fp|
      fp.puts "puts __FILE__"
    end
    write_file File.join(@tempdir, "bin", "foo") do |fp|
      fp.puts "#!/usr/bin/ruby"
    end

    install_gem a

    gem_lib  = File.join @gemhome, "gems", a.full_name, "lib", "a.rb"
    gem_exec = File.join @gemhome, "bin", "foo"

    FileUtils.rm gem_exec
    FileUtils.rm gem_lib

    @cmd.handle_options %w[--all --only-executables]

    use_ui @ui do
      @cmd.execute
    end

    assert_path_exist gem_exec
    assert_path_not_exist gem_lib
  end

  def test_execute_only_plugins
    a = util_spec "a" do |s|
      s.executables = %w[foo]
      s.files = %w[bin/foo lib/a.rb lib/rubygems_plugin.rb]
    end
    write_file File.join(@tempdir, "lib", "a.rb") do |fp|
      fp.puts "puts __FILE__"
    end
    write_file File.join(@tempdir, "lib", "rubygems_plugin.rb") do |fp|
      fp.puts "# do nothing"
    end
    write_file File.join(@tempdir, "bin", "foo") do |fp|
      fp.puts "#!/usr/bin/ruby"
    end

    install_gem a

    gem_lib = File.join @gemhome, "gems", a.full_name, "lib", "a.rb"
    gem_plugin = File.join @gemhome, "plugins", "a_plugin.rb"
    gem_exec = File.join @gemhome, "bin", "foo"

    FileUtils.rm gem_exec
    FileUtils.rm gem_plugin
    FileUtils.rm gem_lib

    @cmd.handle_options %w[--all --only-plugins]

    use_ui @ui do
      @cmd.execute
    end

    assert_path_not_exist gem_exec
    assert_path_exist gem_plugin
    assert_path_not_exist gem_lib
  end

  def test_execute_bindir
    a = util_spec "a" do |s|
      s.name = "test_gem"
      s.executables = %w[foo]
      s.files = %w[bin/foo]
    end

    write_file File.join(@tempdir, "bin", "foo") do |fp|
      fp.puts "#!/usr/bin/ruby"
    end

    write_file File.join(@tempdir, "test_bin", "foo") do |fp|
      fp.puts "#!/usr/bin/ruby"
    end

    install_gem a

    gem_exec = File.join @gemhome, "bin", "foo"
    gem_bindir = File.join @tempdir, "test_bin", "foo"

    FileUtils.rm gem_exec
    FileUtils.rm gem_bindir

    @cmd.handle_options ["--all", "--only-executables", "--bindir", gem_bindir.to_s]

    use_ui @ui do
      @cmd.execute
    end

    assert_path_not_exist gem_exec
    assert_path_exist gem_bindir
  end

  def test_execute_unknown_gem_at_remote_source
    install_specs util_spec "a"

    @cmd.options[:args] = %w[a]

    use_ui @ui do
      @cmd.execute
    end

    assert_equal([
      "Restoring gems to pristine condition...",
      "Cached gem for a-2 not found, attempting to fetch...",
      "Skipped a-2, it was not found from cache and remote sources",
    ], @ui.output.split("\n"))

    assert_empty @ui.error
  end

  def test_execute_default_gem
    default_gem_spec = new_default_spec("default", "2.0.0.0",
                                        nil, "exe/executable")
    default_gem_spec.executables = "executable"
    install_default_gems default_gem_spec

    exe = File.join @gemhome, "bin", "executable"

    assert_path_exist exe, "default gem's executable not installed"

    content_with_replaced_shebang = File.read(exe).gsub(/^#![^\n]+ruby/, "#!/usr/bin/env ruby_executable_hooks")
    File.write(exe, content_with_replaced_shebang)

    @cmd.options[:args] = %w[default]

    use_ui @ui do
      @cmd.execute
    end

    assert_equal(
      [
        "Restoring gems to pristine condition...",
        "Restored default-2.0.0.0",
      ],
      @ui.output.split("\n")
    )
    assert_empty(@ui.error)

    refute_includes "ruby_executable_hooks", File.read(exe)
  end

  def test_execute_default_gem_and_regular_gem
    a_default = new_default_spec("a", "1.2.0")

    a = util_spec "a" do |s|
      s.extensions << "ext/a/extconf.rb"
    end

    ext_path = File.join @tempdir, "ext", "a", "extconf.rb"
    write_file ext_path do |io|
      io.write <<-'RUBY'
      File.open "Makefile", "w" do |f|
        f.puts "clean:\n\techo cleaned\n"
        f.puts "all:\n\techo built\n"
        f.puts "install:\n\techo installed\n"
      end
      RUBY
    end

    install_default_gems a_default
    install_gem a

    # Remove the extension files for a
    FileUtils.rm_rf a.gem_build_complete_path

    @cmd.options[:args] = %w[a]

    use_ui @ui do
      @cmd.execute
    end

    assert_includes @ui.output, "Restored #{a.full_name}"

    # Check extension files for a were restored
    assert_path_exist a.gem_build_complete_path
  end

  def test_execute_multi_platform
    a = util_spec "a" do |s|
      s.extensions << "ext/a/extconf.rb"
    end

    b = util_spec "b" do |s|
      s.extensions << "ext/a/extconf.rb"
      s.platform = Gem::Platform.new("java")
    end

    ext_path = File.join @tempdir, "ext", "a", "extconf.rb"
    write_file ext_path do |io|
      io.write <<-'RUBY'
      File.open "Makefile", "w" do |f|
        f.puts "clean:\n\techo cleaned\n"
        f.puts "all:\n\techo built\n"
        f.puts "install:\n\techo installed\n"
      end
      RUBY
    end

    install_gem a
    install_gem b

    @cmd.options[:extensions]     = true
    @cmd.options[:extensions_set] = true
    @cmd.options[:args] = []

    util_set_arch "x86_64-darwin" do
      use_ui @ui do
        @cmd.execute
      end
    end

    out = @ui.output.split "\n"

    assert_equal "Restoring gems to pristine condition...", out.shift
    assert_equal "Restoring extensions for Ruby ABI #{Gem.ruby_abi}", out.shift
    assert_equal "Building native extensions. This could take a while...",
                 out.shift
    assert_equal "Restored #{a.full_name}", out.shift
    assert_empty out, out.inspect
  end

  def test_handle_options
    @cmd.handle_options %w[]

    refute @cmd.options[:all]

    assert @cmd.options[:extensions]
    refute @cmd.options[:extensions_set]

    assert_equal Gem::Requirement.default, @cmd.options[:version]
  end

  def test_handle_options_extensions
    @cmd.handle_options %w[--extensions]

    assert @cmd.options[:extensions]
    assert @cmd.options[:extensions_set]
  end

  def test_handle_options_ruby_abi
    @cmd.handle_options ["--ruby-abi", Gem.ruby_abi]

    assert_equal Gem.ruby_abi, @cmd.options[:ruby_abi]
  end

  def test_handle_options_ruby_abi_invalid
    e = assert_raise Gem::OptionParser::InvalidArgument do
      @cmd.handle_options ["--ruby-abi", "1"]
    end

    assert_match(/Ruby ABI must be in X.Y format/, e.message)
  end

  private

  ##
  # Installs +name+ with a native extension into the temporary gem home
  # without building it, like a gem whose artifacts for the running Ruby's
  # ABI were never produced on this machine.

  def util_spec_with_unbuilt_extension(name)
    spec = util_spec name do |s|
      s.extensions << "ext/#{name}/extconf.rb"
    end

    write_file File.join(@tempdir, "ext", name, "extconf.rb") do |io|
      io.write "# extconf.rb\n"
    end

    install_gem spec, build_extension: false

    spec
  end

  ##
  # A controllable stand-in for Gem::Ext::Builder: records the extension
  # directory each build writes into +built_dirs+ and marks the build
  # complete, like a successful build would. Returns a replacement for
  # Gem::Ext::Builder.new.

  def recording_builder_factory(built_dirs)
    lambda do |spec, *|
      fake = Object.new
      fake.define_singleton_method(:build_extensions) do
        built_dirs << spec.extension_dir

        FileUtils.mkdir_p spec.extension_dir
        FileUtils.touch spec.gem_build_complete_path
      end
      fake
    end
  end

  ##
  # A controllable stand-in for Gem::Ext::Builder whose build always fails.

  def failing_builder_factory
    lambda do |*_|
      fake = Object.new
      fake.define_singleton_method(:build_extensions) do
        raise Gem::Ext::BuildError, "ERROR: Failed to build gem native extension."
      end
      fake
    end
  end
end
