# frozen_string_literal: true

##
# Parses a gem.deps.rb.lock file and constructs a LockSet containing the
# dependencies found inside.  If the lock file is missing no LockSet is
# constructed.

class Gem::RequestSet::Lockfile
  ##
  # Raised when a lockfile cannot be parsed

  class ParseError < Gem::Exception
    ##
    # The column where the error was encountered

    attr_reader :column

    ##
    # The line where the error was encountered

    attr_reader :line

    ##
    # The location of the lock file

    attr_reader :path

    ##
    # Raises a ParseError with the given +message+ which was encountered at a
    # +line+ and +column+ while parsing.

    def initialize(message, column, line, path)
      @line   = line
      @column = column
      @path   = path
      super "#{message} (at line #{line} column #{column})"
    end
  end

  ##
  # Creates a new Lockfile for the given Gem::RequestSet and +gem_deps_file+
  # location.

  def self.build(request_set, gem_deps_file, dependencies = nil)
    request_set.resolve
    dependencies ||= requests_to_deps request_set.sorted_requests
    new request_set, gem_deps_file, dependencies
  end

  ##
  # Creates a Lockfile from Gem::RequestSet::LockEntry objects, bypassing
  # resolution entirely.  Use this to record the gems a process actually
  # activated in an environment that is already installed, where asking an
  # index for versions would describe what could be installed instead of what
  # is running.
  #
  # +dependencies+ defaults to pinning every entry to the exact version it was
  # activated at, so replaying the lock cannot float to another installed
  # version.  Path and git entries keep the +!+ marker.

  def self.from_entries(entries, gem_deps_file, dependencies = nil)
    dependencies ||= entries_to_deps entries
    lockfile = new nil, gem_deps_file, dependencies
    lockfile.entries = entries
    lockfile.built_from_entries = true
    lockfile.platforms.replace entries.map(&:platform).uniq.sort_by(&:to_s)
    lockfile
  end

  ##
  # The dependencies map used when a lockfile is built from activated entries.
  # Every entry is pinned to the version it was activated at.

  def self.entries_to_deps(entries) # :nodoc:
    entries.each_with_object({}) do |entry, deps|
      deps[entry.name] =
        if entry.source_set?
          Gem::Requirement.source_set
        else
          Gem::Requirement.new "= #{entry.version}"
        end
    end
  end

  def self.requests_to_deps(requests) # :nodoc:
    deps = {}

    requests.each do |request|
      spec        = request.spec
      name        = request.name
      requirement = request.request.dependency.requirement

      deps[name] = if [Gem::Resolver::VendorSpecification,
                       Gem::Resolver::GitSpecification].include? spec.class
        Gem::Requirement.source_set
      else
        requirement
      end
    end

    deps
  end

  ##
  # The platforms for this Lockfile

  attr_reader :platforms

  ##
  # The Gem::RequestSet::LockEntry objects this lockfile was built from.  Empty
  # for a lockfile built by resolving a RequestSet.

  attr_reader :entries

  attr_writer :entries # :nodoc:

  ##
  # True when this lockfile was built from entries rather than by resolving a
  # RequestSet.

  attr_writer :built_from_entries # :nodoc:

  def built_from_entries? # :nodoc:
    !!@built_from_entries
  end

  def initialize(request_set, gem_deps_file, dependencies)
    @set           = request_set
    @dependencies  = dependencies
    @gem_deps_file = File.expand_path(gem_deps_file)
    @gem_deps_dir  = File.dirname(@gem_deps_file)
    @built_from_entries = false
    @platforms = []
    @entries   = []
  end

  def add_DEPENDENCIES(out) # :nodoc:
    out << "DEPENDENCIES"

    out.concat @dependencies.sort.map {|name, requirement|
      "  #{name}#{requirement.for_lockfile}"
    }

    out << nil
  end

  def add_GEM(out, spec_groups) # :nodoc:
    return if spec_groups.empty?

    source_groups = spec_groups.values.flatten.group_by do |request|
      request.spec.source.uri
    end

    source_groups.sort_by {|group,| group.to_s }.map do |group, requests|
      out << "GEM"
      out << "  remote: #{group}"
      out << "  specs:"

      requests.sort_by(&:name).each do |request|
        next if request.spec.name == "bundler"
        platform = "-#{request.spec.platform}" unless
          request.spec.platform == Gem::Platform::RUBY

        out << "    #{request.name} (#{request.version}#{platform})"

        request.full_spec.dependencies.sort.each do |dependency|
          next if dependency.type == :development

          requirement = dependency.requirement
          out << "      #{dependency.name}#{requirement.for_lockfile}"
        end
      end
      out << nil
    end
  end

  def add_GIT(out, git_requests)
    return if git_requests.empty?

    by_repository_revision = git_requests.group_by do |request|
      source = request.spec.source
      [source.repository, source.rev_parse]
    end

    by_repository_revision.each do |(repository, revision), requests|
      out << "GIT"
      out << "  remote: #{repository}"
      out << "  revision: #{revision}"
      out << "  specs:"

      requests.sort_by(&:name).each do |request|
        out << "    #{request.name} (#{request.version})"

        dependencies = request.spec.dependencies.sort_by(&:name)
        dependencies.each do |dep|
          out << "      #{dep.name}#{dep.requirement.for_lockfile}"
        end
      end
      out << nil
    end
  end

  def relative_path_from(dest, base) # :nodoc:
    dest = File.expand_path(dest)
    base = File.expand_path(base)

    if dest.index(base) == 0
      offset = dest[base.size + 1..-1]

      return "." unless offset

      offset
    else
      dest
    end
  end

  def add_PATH(out, path_requests) # :nodoc:
    return if path_requests.empty?

    out << "PATH"
    path_requests.each do |request|
      directory = File.expand_path(request.spec.source.uri)

      out << "  remote: #{relative_path_from directory, @gem_deps_dir}"
      out << "  specs:"
      out << "    #{request.name} (#{request.version})"
    end

    out << nil
  end

  def add_PLATFORMS(out) # :nodoc:
    out << "PLATFORMS"

    platforms = requests.map {|request| request.spec.platform }.uniq

    platforms = platforms.sort_by(&:to_s)

    platforms.each do |platform|
      out << "  #{platform}"
    end

    out << nil
  end

  def spec_groups
    requests.group_by {|request| request.spec.class }
  end

  ##
  # Writes a GEM section for entries whose source is a remote, or whose origin
  # is unknown.  Entries with a known remote are grouped by it.
  #
  # An activated gem that is installed locally but whose origin was never
  # recorded gets a GEM section with no +remote:+ line and a comment saying
  # the remote is unknown.  That keeps the lock loadable while showing the gap
  # instead of inventing a source; #missing_information reports the same fact
  # in a structured way.

  def add_GEM_entries(out, entries) # :nodoc:
    return if entries.empty?

    remote_groups = entries.group_by(&:remote)
    unknown       = remote_groups.delete(nil) { [] }

    remote_groups.sort_by {|remote, _| remote.to_s }.each do |remote, grouped|
      out << "GEM"
      out << "  remote: #{remote}"
      add_entry_specs out, grouped
    end

    unless unknown.empty?
      out << "GEM"
      out << "  # remote: unknown"
      add_entry_specs out, unknown
    end
  end

  def add_entry_specs(out, entries) # :nodoc:
    out << "  specs:"

    entries.sort_by {|entry| [entry.name, entry.version, entry.platform.to_s] }.each do |entry|
      out << "    #{entry.name} (#{entry.version}#{entry.platform_suffix})"

      entry.dependencies.each do |dependency|
        out << "      #{dependency.name}#{dependency.requirement.for_lockfile}"
      end
    end

    out << nil
  end

  ##
  # Writes a PATH section per directory for entries that came from a path
  # source.  Entries without a usable directory are reported under an unknown
  # remote rather than silently dropped.

  def add_PATH_entries(out, entries) # :nodoc:
    return if entries.empty?

    by_directory = entries.group_by(&:path)

    by_directory.sort_by {|directory, _| directory.to_s }.each do |directory, grouped|
      out << "PATH"

      if directory.to_s.empty?
        out << "  # remote: unknown"
      else
        out << "  remote: #{relative_path_from directory, @gem_deps_dir}"
      end

      add_entry_specs out, grouped
    end
  end

  ##
  # Writes a GIT section per repository and revision for entries that came
  # from a git source.  A missing revision is reported instead of being
  # guessed from a moving branch.

  def add_GIT_entries(out, entries) # :nodoc:
    return if entries.empty?

    by_repository_revision = entries.group_by {|entry| [entry.repository, entry.revision] }
    sorted = by_repository_revision.sort_by do |(repository, revision), _|
      [repository.to_s, revision.to_s]
    end

    sorted.each do |(repository, revision), grouped|
      out << "GIT"
      out << (repository.to_s.empty? ? "  # remote: unknown" : "  remote: #{repository}")
      out << (revision.to_s.empty? ? "  # revision: unknown" : "  revision: #{revision}")
      add_entry_specs out, grouped
    end
  end

  ##
  # The PLATFORMS section for a lockfile built from entries.  The real
  # platform of each activated spec is written, so a native gem is not
  # disguised as the ruby platform.

  def add_ENTRY_PLATFORMS(out) # :nodoc:
    out << "PLATFORMS"

    @entries.map(&:platform).uniq.sort_by(&:to_s).each do |platform|
      out << "  #{platform}"
    end

    out << nil
  end

  ##
  # Per-gem report of versions that are installed but were not the activated
  # one.  Empty when every activated version is the only installed version.

  def divergences
    @entries.filter_map(&:divergence)
  end

  ##
  # The names and versions of entries whose source could not be fully
  # determined.

  def missing_information
    @entries.select(&:missing?).map do |entry|
      { name: entry.name, version: entry.version, missing: entry.missing }
    end
  end

  ##
  # The contents of the lock file.

  def to_s
    out = []

    if built_from_entries?
      add_PATH_entries out, @entries.select {|entry| entry.source_type == :path }

      add_GIT_entries out, @entries.select {|entry| entry.source_type == :git }

      add_GEM_entries out, @entries.select {|entry| entry.source_type != :path && entry.source_type != :git }

      add_ENTRY_PLATFORMS out
    else
      groups = spec_groups

      add_PATH out, groups.delete(Gem::Resolver::VendorSpecification) { [] }

      add_GIT out, groups.delete(Gem::Resolver::GitSpecification) { [] }

      add_GEM out, groups

      add_PLATFORMS out
    end

    add_DEPENDENCIES out

    out.join "\n"
  end

  ##
  # Writes the lock file alongside the gem dependencies file

  def write
    content = to_s

    File.open "#{@gem_deps_file}.lock", "w" do |io|
      io.write content
    end
  end

  private

  def requests
    @set.sorted_requests
  end
end
