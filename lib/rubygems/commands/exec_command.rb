# frozen_string_literal: true

require_relative "../command"
require_relative "../dependency_installer"
require_relative "../gem_runner"
require_relative "../local_remote_options"
require_relative "../package"
require_relative "../request_set"
require_relative "../resolver"
require_relative "../version_option"

class Gem::Commands::ExecCommand < Gem::Command
  include Gem::VersionOption
  include Gem::LocalRemoteOptions

  ##
  # The outcome of running an executable through #isolated. The standard
  # streams the child produced are available as #stdout and #stderr, and the
  # child's Process::Status as #status.

  class IsolatedResult
    attr_reader :stdout, :stderr, :status

    def initialize(stdout, stderr, status)
      @stdout = stdout
      @stderr = stderr
      @status = status
    end

    ##
    # The exit code of the child process, or +nil+ if it was terminated by a
    # signal.

    def exitstatus
      @status&.exitstatus
    end

    alias_method :exit_code, :exitstatus

    def success?
      @status ? @status.success? : false
    end

    def signaled?
      @status ? @status.signaled? : false
    end
  end

  ##
  # The executable that the child process runs. It activates the resolved gem
  # versions by exact version, then loads the executable from the requested
  # gem. It lives in a separate process so nothing it does can leak into the
  # process that invoked +gem exec+.

  ISOLATED_SCRIPT = <<~'RUBY'
    require "rubygems"

    ENV.fetch("GEM_EXEC_ISOLATED_PLAN", "").each_line do |line|
      name, _, version = line.strip.partition("=")
      next if name.empty?

      gem name, "= #{version}"
    end

    if ENV["GEM_EXEC_ISOLATED_GEM"] == "1"
      require "rubygems/gem_runner"
      Gem::GemRunner.new.run ARGV.dup
    else
      $0 = ENV.fetch("GEM_EXEC_ISOLATED_EXECUTABLE")
      load ENV.fetch("GEM_EXEC_ISOLATED_PATH")
    end
  RUBY

  ##
  # The last IsolatedResult produced by #isolated, if any.

  attr_reader :isolated_result

  def initialize
    super "exec", "Run a command from a gem", {
      version: Gem::Requirement.default,
    }

    add_source_option
    add_version_option
    add_prerelease_option "to be installed"

    add_option "-g", "--gem GEM", "run the executable from the given gem" do |value, options|
      options[:gem_name] = value
    end

    add_option(:"Install/Update", "--conservative",
      "Prefer the most recent installed version, ",
      "rather than the latest version overall") do |_value, options|
      options[:conservative] = true
    end

    add_option "--isolated",
      "Run the resolved executable in a separate Ruby process so that this ",
      "process keeps its own activated gems, loaded files and ARGV" do |_value, options|
      options[:isolated] = true
    end
  end

  def arguments # :nodoc:
    "COMMAND  the executable command to run"
  end

  def defaults_str # :nodoc:
    "--version '#{Gem::Requirement.default}'"
  end

  def description # :nodoc:
    <<-EOF
The exec command handles installing (if necessary) and running an executable
from a gem, regardless of whether that gem is currently installed.

The exec command can be thought of as a shortcut to running `gem install` and
then the executable from the installed gem.

For example, `gem exec rails new .` will run `rails new .` in the current
directory, without having to manually run `gem install rails`.
Additionally, the exec command ensures the most recent version of the gem
is used (unless run with `--conservative`), and that the gem is not installed
to the same gem path as user-installed gems.

With `--isolated`, the resolution, activation and loading of the executable
all happen in a separate Ruby process that starts from the versions resolved
for this invocation. The invoking process keeps its own activated gems,
already loaded files and ARGV. The child's standard output and standard error
are passed through, and its exit status becomes the exit status of this
command, so a failing tool reports failure without affecting the state that
the next invocation starts from.
    EOF
  end

  def usage # :nodoc:
    "#{program_name} [options --] COMMAND [args]"
  end

  def execute
    check_executable

    print_command

    if options[:isolated]
      report_isolated isolated(
        args: options[:args],
        gem_runner: gem_runner_invocation?
      )
      return
    end

    if gem_runner_invocation?
      set_gem_exec_install_paths
      Gem::GemRunner.new.run options[:args]
      return
    elsif options[:conservative]
      install_if_needed
    else
      install
      activate!
    end

    load!
  end

  ##
  # Runs the executable for the requested gem in a separate Ruby process. The
  # gem versions resolved for this invocation are activated there by exact
  # version, so the executable and everything it requires come from the
  # requested versions. This process keeps its own activated gems, loaded
  # files and ARGV, and the child's standard output, standard error and exit
  # status are returned as an IsolatedResult.
  #
  # Nothing the child does can leak into the caller, and every invocation
  # starts from a fresh process, so a tool that crashes cannot affect the
  # next invocation.

  def isolated(args: options[:args], gem_runner: false)
    state = snapshot_parent_state

    begin
      set_gem_exec_install_paths if gem_runner

      specs = gem_runner ? [] : resolve_isolated_specs
      executable, spec = gem_runner ? [nil, nil] : resolve_executable(specs)
      @isolated_result = run_child(specs, executable, spec, args, gem_runner: gem_runner)
    ensure
      restore_parent_state state
    end

    @isolated_result
  end

  private

  def gem_runner_invocation?
    options[:gem_name] == "gem" && options[:executable] == "gem"
  end

  ##
  # Writes the child's streams through the command UI and turns a failed child
  # into the same kind of exit the command itself would produce, so callers
  # observe the exit status of the tool rather than of +gem exec+.

  def report_isolated(result)
    write_stream ui.outs, result.stdout
    write_stream ui.errs, result.stderr

    terminate_interaction(result.exitstatus || 1) unless result.success?

    result
  end

  def write_stream(io, data)
    return if data.empty?

    if io.respond_to?(:write)
      io.write data
    else
      io.print data
    end
    io.flush if io.respond_to?(:flush)
  end

  ##
  # Resolves +options[:gem_name]+ at +options[:version]+ against the gems that
  # are already installed. Returns the full dependency closure in activation
  # order, dependencies before dependents.

  def resolve_isolated_specs
    request_set = resolve_local_request_set
    return sorted_specs(request_set) if request_set

    unless options[:conservative]
      dependency = Gem::Dependency.new(options[:gem_name], options[:version])
      verbose "#{dependency} not available locally, installing from remote"
      install

      request_set = resolve_local_request_set
      return sorted_specs(request_set) if request_set
    end

    alert_error "Could not find a valid gem '#{options[:gem_name]}' (#{options[:version]}) locally"
    terminate_interaction 2
  end

  def resolve_local_request_set
    request_set = Gem::RequestSet.new Gem::Dependency.new(options[:gem_name], options[:version])
    request_set.prerelease = options[:prerelease]
    request_set.remote = false
    request_set.resolve_current
    request_set
  rescue Gem::MissingSpecError, Gem::UnsatisfiableDependencyError,
         Gem::DependencyResolutionError
    nil
  end

  def sorted_specs(request_set)
    request_set.sorted_requests.map(&:full_spec)
  end

  ##
  # Chooses which gem's executable should run among +specs+ and returns the
  # executable name together with its spec. Shared by #load! and #isolated so
  # both modes pick the same executable.

  def resolve_executable(specs)
    executable = options[:executable]

    candidates = specs.select {|spec| spec.executables.include? executable }
    candidates.select! {|spec| spec.name == executable } if candidates.any? {|spec| spec.name == executable }

    if candidates.empty?
      spec = specs.find {|s| s.name == executable }

      if spec.nil? || spec.executables.empty?
        alert_error "Failed to load executable `#{executable}`," \
              " are you sure the gem `#{options[:gem_name]}` contains it?"
        terminate_interaction 1
      end

      if spec.executables.size > 1
        alert_error "Ambiguous which executable from gem `#{executable}` should be run: " \
              "the options are #{spec.executables.sort}, specify one via COMMAND, and use `-g` and `-v` to specify gem and version"
        terminate_interaction 1
      end

      candidates << spec
      executable = spec.executable
    end

    if candidates.size > 1
      alert_error "Ambiguous which gem `#{executable}` should come from: " \
            "the options are #{candidates.map(&:name)}, " \
            "specify one via `-g`"
      terminate_interaction 1
    end

    [executable, candidates.first]
  end

  ##
  # Spawns a fresh Ruby process that activates the resolved versions and then
  # runs the executable. The child inherits only the gem paths and the
  # resolution plan, so parent-level activation and load state does not reach
  # it.

  def run_child(specs, executable, spec, args, gem_runner: false)
    require "open3"

    plan = specs.map {|s| "#{s.name}=#{s.version}" }.join("\n")

    # The child must not inherit the parent's Bundler setup or load path
    # preferences, only the gem paths and the versions resolved here.
    env = {
      "GEM_EXEC_ISOLATED_PLAN" => plan,
      "GEM_EXEC_ISOLATED_EXECUTABLE" => executable.to_s,
      "GEM_EXEC_ISOLATED_PATH" => spec ? spec.bin_file(executable).to_s : "",
      "GEM_EXEC_ISOLATED_GEM" => gem_runner ? "1" : "0",
      "GEM_HOME" => Gem.dir,
      "GEM_PATH" => Gem.path.join(File::PATH_SEPARATOR),
      "RUBYOPT" => nil,
      "RUBYLIB" => nil,
      "RUBYGEMS_GEMDEPS" => nil,
      "BUNDLE_GEMFILE" => nil,
      "BUNDLE_BIN_PATH" => nil,
      "BUNDLER_SETUP" => nil,
      "BUNDLER_VERSION" => nil,
      "BUNDLE_WITHOUT" => nil,
      "BUNDLE_WITH" => nil,
    }

    command = [ruby_interpreter]
    lib_dir = rubygems_lib_dir
    command.concat ["-I", lib_dir] if lib_dir
    command.concat ["-e", ISOLATED_SCRIPT, "--", *args]

    stdout, stderr, status = Open3.capture3(env, *command)

    IsolatedResult.new(stdout, stderr, status)
  end

  ##
  # The interpreter to run the child with, without the shell quoting that
  # Gem.ruby may add around paths containing spaces.

  def ruby_interpreter
    ruby = Gem.ruby.to_s
    ruby = ruby[1..-2] if ruby.length > 1 && ruby.start_with?('"') && ruby.end_with?('"')
    ruby
  end

  ##
  # The directory containing the RubyGems that is running this command, so the
  # child resolves and activates gems with the same implementation.

  def rubygems_lib_dir
    feature = $LOADED_FEATURES.find {|f| f.end_with?("/rubygems.rb") }
    feature ? File.dirname(feature) : File.expand_path("../..", __dir__)
  end

  def snapshot_parent_state
    {
      gem_home: ENV["GEM_HOME"],
      gem_path: ENV["GEM_PATH"],
      dir: Gem.dir,
      path: Gem.path.dup,
      loaded_specs: Gem.loaded_specs.dup,
      unresolved: Gem::Specification.unresolved_deps.dup,
      argv: ARGV.dup,
    }
  end

  def restore_parent_state(state)
    ENV["GEM_HOME"] = state[:gem_home]
    ENV["GEM_PATH"] = state[:gem_path]

    if Gem.dir != state[:dir] || Gem.path != state[:path]
      Gem.use_paths(state[:dir], state[:path])
    end

    loaded = Gem.loaded_specs
    loaded.each_key.to_a.each do |name|
      loaded.delete(name) unless state[:loaded_specs][name].equal?(loaded[name])
    end
    state[:loaded_specs].each {|name, spec| loaded[name] ||= spec }

    Gem::Specification.unresolved_deps.replace state[:unresolved]
    ARGV.replace state[:argv]
  end

  def handle_options(args)
    args = add_extra_args(args)
    check_deprecated_options(args)
    @options = Gem::Util.deep_dup @defaults
    parser.order!(args) do |v|
      # put the non-option back at the front of the list of arguments
      args.unshift(v)

      # stop parsing once we hit the first non-option,
      # so you can call `gem exec rails --version` and it prints the rails
      # version rather than rubygem's
      break
    end
    @options[:args] = args

    options[:executable], gem_version = extract_gem_name_and_version(options[:args].shift)
    options[:gem_name] ||= options[:executable]

    if gem_version
      if options[:version].none?
        options[:version] = Gem::Requirement.new(gem_version)
      else
        options[:version].concat [gem_version]
      end
    end

    if options[:prerelease] && !options[:version].prerelease?
      if options[:version].none?
        options[:version] = Gem::Requirement.default_prerelease
      else
        options[:version].concat [Gem::Requirement.default_prerelease]
      end
    end
  end

  def check_executable
    if options[:executable].nil?
      raise Gem::CommandLineError,
        "Please specify an executable to run (e.g. #{program_name} COMMAND)"
    end
  end

  def print_command
    verbose "running #{program_name} with:\n"
    opts = options.reject {|_, v| v.nil? || Array(v).empty? }
    max_length = opts.map {|k, _| k.size }.max
    opts.each do |k, v|
      next if v.nil?
      verbose "\t#{k.to_s.rjust(max_length)}: #{v}"
    end
    verbose ""
  end

  def install_if_needed
    activate!
  rescue Gem::MissingSpecError
    verbose "#{Gem::Dependency.new(options[:gem_name], options[:version])} not available locally, installing from remote"
    install
    activate!
  end

  def set_gem_exec_install_paths
    home = Gem.dir

    ENV["GEM_PATH"] = ([home] + Gem.path).join(File::PATH_SEPARATOR)
    ENV["GEM_HOME"] = home
    Gem.clear_paths
  end

  def install
    set_gem_exec_install_paths

    gem_name = options[:gem_name]
    gem_version = options[:version]

    install_options = options.merge(
      minimal_deps: false,
      wrappers: true
    )

    suppress_always_install do
      dep_installer = Gem::DependencyInstaller.new install_options

      request_set = dep_installer.resolve_dependencies gem_name, gem_version

      verbose "Gems to install:"
      request_set.sorted_requests.each do |activation_request|
        verbose "\t#{activation_request.full_name}"
      end

      request_set.install install_options
    end

    Gem::Specification.reset
  rescue Gem::InstallError => e
    alert_error "Error installing #{gem_name}:\n\t#{e.message}"
    terminate_interaction 1
  rescue Gem::DependencyResolutionError => e
    alert_error "Error installing #{gem_name}:\n\t#{e.message}"
    terminate_interaction 2
  rescue Gem::GemNotFoundException => e
    show_lookup_failure e.name, e.version, e.errors, false

    terminate_interaction 2
  rescue Gem::UnsatisfiableDependencyError => e
    show_lookup_failure e.name, e.version, e.errors, false,
                        "'#{gem_name}' (#{gem_version})"

    terminate_interaction 2
  end

  def activate!
    gem(options[:gem_name], options[:version])
    Gem.finish_resolve

    verbose "activated #{options[:gem_name]} (#{Gem.loaded_specs[options[:gem_name]].version})"
  end

  def load!
    argv = ARGV.clone
    ARGV.replace options[:args]

    executable, spec = resolve_executable(Gem.loaded_specs.values)

    old_exe = $0
    $0 = executable
    load Gem.activate_bin_path(spec.name, executable, ">= 0.a")
  ensure
    $0 = old_exe if old_exe
    ARGV.replace argv
  end

  def suppress_always_install
    name = :always_install
    cls = ::Gem::Resolver::InstallerSet
    method = cls.instance_method(name)
    cls.remove_method(name)
    cls.define_method(name) { [] }

    begin
      yield
    ensure
      cls.remove_method(name)
      cls.define_method(name, method)
    end
  end
end
