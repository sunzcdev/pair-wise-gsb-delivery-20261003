# frozen_string_literal: true

require_relative "../command"
require_relative "../version_option"

class Gem::Commands::ContentsCommand < Gem::Command
  include Gem::VersionOption

  def initialize
    super "contents", "Display the contents of the installed gems",
          specdirs: [], lib_only: false, prefix: true,
          show_install_dir: false

    add_version_option

    add_option("--all",
               "Contents for all gems") do |all, options|
      options[:all] = all
    end

    add_option("-s", "--spec-dir a,b,c", Array,
               "Search for gems under specific paths") do |spec_dirs, options|
      options[:specdirs] = spec_dirs
    end

    add_option("-l", "--[no-]lib-only",
               "Only return files in the Gem's lib_dirs") do |lib_only, options|
      options[:lib_only] = lib_only
    end

    add_option("--[no-]prefix",
               "Don't include installed path prefix") do |prefix, options|
      options[:prefix] = prefix
    end

    add_option("--[no-]show-install-dir",
               "Show only the gem install dir") do |show, options|
      options[:show_install_dir] = show
    end

    @path_kind = nil
    @spec_dirs = nil
    @version   = nil
  end

  def arguments # :nodoc:
    "GEMNAME       name of gem to list contents for"
  end

  def defaults_str # :nodoc:
    "--no-lib-only --prefix"
  end

  def description # :nodoc:
    <<-EOF
The contents command lists the files in an installed gem.  The listing can
be given as full file names, file names without the installed directory
prefix or only the files that are requireable.
    EOF
  end

  def usage # :nodoc:
    "#{program_name} GEMNAME [GEMNAME ...]"
  end

  def execute
    @version   = options[:version] || Gem::Requirement.default
    @spec_dirs = specification_directories
    @path_kind = path_description @spec_dirs

    names = gem_names

    names.each do |name|
      found =
        if options[:show_install_dir]
          gem_install_dir name
        else
          gem_contents name
        end

      terminate_interaction 1 unless found || names.length > 1
    end
  end

  def files_in(spec)
    if spec.default_gem?
      files_in_default_gem spec
    else
      files_in_gem spec
    end
  end

  def files_in_gem(spec)
    gem_path = spec.full_gem_path

    if options[:lib_only]
      # raw_require_paths rather than require_paths, since the latter prepends
      # the absolute extension_dir, which would escape the glob base
      require_paths = spec.raw_require_paths
      return [] if require_paths.empty?

      glob = "{#{require_paths.join ","}}/**/*"
    else
      glob = "**/*"
    end

    files = Dir.glob(glob, base: gem_path).map do |file|
      [gem_path, file]
    end

    report_missing_files(spec, files, gem_path) unless options[:lib_only]

    files
  end

  def files_in_default_gem(spec)
    spec.files.filter_map do |file|
      if file.start_with?("#{spec.bindir}/")
        [RbConfig::CONFIG["bindir"], file.delete_prefix("#{spec.bindir}/")]
      else
        gem spec.name, spec.version

        require_path = spec.require_paths.find do |path|
          file.start_with?("#{path}/")
        end

        requirable_part = file.delete_prefix("#{require_path}/")

        resolve = $LOAD_PATH.resolve_feature_path(requirable_part)&.last
        unless resolve
          alert_warning "#{spec.full_name}: #{file} is declared in the gemspec " \
                        "but cannot be found on the load path"
          next
        end

        [resolve.delete_suffix(requirable_part), requirable_part]
      end
    end
  end

  def gem_contents(name)
    specs = specs_for name

    return false if specs.empty?

    specs.each_with_index do |spec, index|
      # The same name and version can be installed under more than one Gem
      # home. When that happens, label each section with its actual source so
      # every reported path can be traced back to the tree it came from.
      if specs.length > 1
        say if index > 0
        say source_label spec
      end

      files = files_in spec

      show_files files
    end

    true
  end

  def gem_install_dir(name)
    spec = spec_for name

    return false unless spec

    say spec.gem_dir

    true
  end

  def gem_names # :nodoc:
    if options[:all]
      Gem::Specification.map(&:name).uniq
    else
      get_all_gem_names
    end
  end

  def path_description(spec_dirs) # :nodoc:
    if spec_dirs.empty?
      "default gem paths"
    else
      "specified path"
    end
  end

  def show_files(files)
    files.sort.each do |prefix, basename|
      absolute_path = File.join(prefix, basename)
      next if File.directory? absolute_path

      if options[:prefix]
        say absolute_path
      else
        say basename
      end
    end
  end

  # Where the spec's files are actually read from. Default gems keep their
  # code outside the Gem home (typically the Ruby load path), so callers can
  # distinguish a real install tree from a default gem location.

  def source_label(spec)
    label = "#{spec.full_name} in #{spec.base_dir}"
    label = "#{label} (default gem)" if spec.default_gem?
    label
  end

  # Warns about files declared by the gemspec that are not present in the
  # install tree. Globbed listings silently drop these, which makes a broken
  # install look identical to a complete one.

  def report_missing_files(spec, files, gem_path)
    present = files.map(&:last)

    spec.files.each do |declared|
      next if present.include?(declared)
      next if File.exist?(File.join(gem_path, declared))

      alert_warning "#{spec.full_name}: #{declared} is declared in the gemspec " \
                    "but missing from #{gem_path}"
    end
  end

  def spec_for(name)
    specs_for(name).first
  end

  def specs_for(name)
    specs = Gem::Specification.find_all_by_name(name, @version)

    return specs unless specs.empty?

    say "Unable to find gem '#{name}' in #{@path_kind}"

    if Gem.configuration.verbose
      say "\nDirectories searched:"
      @spec_dirs.sort.each {|dir| say dir }
    end

    []
  end

  def specification_directories # :nodoc:
    options[:specdirs].flat_map do |i|
      Gem::SpecificationRecord.dirs_with_abi([i, File.join(i, "specifications")])
    end
  end
end
