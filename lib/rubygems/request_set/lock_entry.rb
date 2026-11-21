# frozen_string_literal: true

##
# A single row of a lockfile built from the gems a process actually activates,
# together with the local specifications that back them.
#
# A Gem::Resolver::LockSpecification comes out of resolving against an index;
# a LockEntry instead records what an activation really decided:
#
# * the platform carried by the activated Gem::Specification, so a native gem
#   is not reported as the ruby platform
# * the source the gem came from: a remote-backed install, a plain installed
#   gem whose remote is unknown, a path gem, a git checkout, or nothing usable
#   at all
#
# Versions of the same gem that merely happen to be installed are kept in
# #other_installed_versions so a report can name them, but they never add a
# row of their own: being installed does not make a version active.

class Gem::RequestSet::LockEntry
  ##
  # Source types.  +:installed+ means "installed locally, remote unknown",
  # +:unknown+ means no source information at all could be found.

  SOURCE_TYPES = [:gem, :installed, :path, :git, :unknown].freeze

  ##
  # The name of the gem.

  attr_reader :name

  ##
  # The Gem::Version that was activated.

  attr_reader :version

  ##
  # The Gem::Platform the activated specification carries.

  attr_reader :platform

  ##
  # The runtime dependencies of the activated gem, as Gem::Dependency
  # objects.  Development dependencies are not part of a lock.

  attr_reader :dependencies

  ##
  # Where this entry came from: one of SOURCE_TYPES.

  attr_reader :source_type

  ##
  # The source object behind this entry, when there is one.

  attr_reader :source

  ##
  # The remote for a gem entry, when it is known.

  attr_reader :remote

  ##
  # The repository URL for a git entry.

  attr_reader :repository

  ##
  # The resolved commit for a git entry.

  attr_reader :revision

  ##
  # The directory for a path entry.

  attr_reader :path

  ##
  # The base directory of the local install that backs this entry, when one
  # was found.

  attr_reader :installed_dir

  ##
  # Every version of this gem the local spec record knows about.

  attr_reader :installed_versions

  ##
  # The local specifications with the same name as the activated gem.

  attr_reader :local_specs

  ##
  # The pieces of information this entry could not determine.  +:remote+ means
  # the gem is installed but its origin is unknown; +:source+ means no source
  # at all could be named.

  attr_reader :missing

  ##
  # Builds an entry for +spec+, which may be a Gem::Specification, an
  # ActivationRequest or a resolver specification.  +local_specs+ are the
  # local specifications with the same name, used to locate the install and to
  # list the versions that were not activated.  +source_spec+ is consulted for
  # source information when +spec+ does not carry any, for example when a gem
  # activated from the loaded specs is paired with the resolver request that
  # selected it.

  def self.from_spec(spec, local_specs: [], source_spec: nil)
    new spec, local_specs: local_specs, source_spec: source_spec
  end

  def initialize(spec, local_specs: [], source_spec: nil)
    spec = spec.spec if spec.is_a?(Gem::Resolver::ActivationRequest)

    @name               = spec.name
    @version            = spec.version
    @platform           = spec.platform || Gem::Platform::RUBY
    @local_specs        = local_specs
    @installed_versions = local_specs.map(&:version).uniq.sort
    @source             = nil
    @source_type        = :unknown
    @remote             = nil
    @repository         = nil
    @revision           = nil
    @path               = nil
    @installed_dir      = nil
    @missing            = []

    @dependencies = runtime_dependencies(spec)

    local = matching_local_spec(spec)
    @installed_dir = full_spec_base_dir(local) if local
    @installed_dir ||= full_spec_base_dir(spec)

    classify_source spec, local, source_spec
    @missing.uniq!
  end

  ##
  # True when at least one piece of information could not be determined.

  def missing?
    !@missing.empty?
  end

  ##
  # True when this entry came from a path or git source, which a lockfile
  # marks with a trailing +!+ in DEPENDENCIES.

  def source_set?
    @source_type == :path || @source_type == :git
  end

  ##
  # Every installed version of this gem other than the activated one.

  def other_installed_versions
    @installed_versions.reject {|installed| installed == @version }
  end

  ##
  # True when another version of the same gem is installed but is not the one
  # this entry activates.

  def divergent?
    !other_installed_versions.empty?
  end

  ##
  # A report row naming the activated version and the installed versions that
  # were not activated.  Nil when the installed versions agree.

  def divergence
    return unless divergent?

    { name: @name, activated: @version, installed: other_installed_versions }
  end

  ##
  # The name, version and platform of the activated gem, in the same shape as
  # a lockfile spec line.

  def full_name
    "#{@name}-#{@version}#{platform_suffix}"
  end

  ##
  # The platform suffix used on a spec line, empty for the ruby platform.

  def platform_suffix
    return if @platform == Gem::Platform::RUBY

    "-#{@platform}"
  end

  ##
  # A structured view for reports and callers that should not depend on
  # internal ivars.

  def to_h
    {
      name: @name,
      version: @version,
      platform: @platform,
      source_type: @source_type,
      remote: @remote,
      repository: @repository,
      revision: @revision,
      path: @path,
      installed_dir: @installed_dir,
      installed_versions: @installed_versions,
      missing: @missing.dup,
    }
  end

  def to_s
    "#{@name} (#{@version}#{platform_suffix})"
  end

  def inspect # :nodoc:
    format("#<%s %s source %s%s>", self.class, to_s, @source_type,
           missing? ? " missing #{@missing.join(",")}" : "")
  end

  private

  def runtime_dependencies(spec)
    deps = if spec.respond_to?(:runtime_dependencies)
      spec.runtime_dependencies
    elsif spec.respond_to?(:dependencies)
      spec.dependencies.reject {|dep| dep.type == :development }
    else
      []
    end

    deps.sort_by(&:name)
  rescue StandardError
    # A resolver specification may need to fetch its gemspec to answer this.
    # The version and platform are already known, so record the gap rather
    # than failing the whole export.
    @missing << :dependencies
    []
  end

  ##
  # The base directory of the local install backing +spec+.  Only a spec that
  # was actually loaded from disk has one: an in-memory Gem::Specification and
  # an index specification both fall back to the default gem dir, which would
  # wrongly look like an install location.

  def full_spec_base_dir(spec)
    candidate = spec
    candidate = candidate.spec if candidate.is_a?(Gem::Resolver::Specification)
    return unless candidate.is_a?(Gem::Specification)
    return if candidate.loaded_from.nil?

    candidate.base_dir
  rescue StandardError
    nil
  end

  def matching_local_spec(spec)
    exact = @local_specs.find do |local|
      local.name == spec.name && local.version == spec.version && local.platform == spec.platform
    end
    return exact if exact

    # An installed spec may keep a platform the resolver spec normalised to
    # ruby, so a name and version match is still the same local install.
    @local_specs.find do |local|
      local.name == spec.name && local.version == spec.version
    end
  end

  def classify_source(spec, local, source_spec = nil)
    source = spec.source if spec.respond_to?(:source)
    source = nil if placeholder_source?(source)

    if source.nil? && source_spec && source_spec != spec
      alt = source_spec.source if source_spec.respond_to?(:source)
      source = alt unless placeholder_source?(alt)
    end

    if source.nil?
      installed_or_unknown local
    elsif git_source?(source)
      git_entry source
    elsif path_source?(source)
      path_entry path_of(source)
    elsif (uri = source_uri(source))
      @source      = source
      @source_type = :gem
      @remote      = uri
    else
      installed_or_unknown local, source
    end
  end

  ##
  # True for a source that only says "this gem is installed" without naming
  # where it came from.  Gem::Source::Vendor subclasses it but does carry a
  # path, so the check is on the exact class.

  def placeholder_source?(source)
    source.class == Gem::Source::Installed
  end

  def git_source?(source)
    %w[Gem::Source::Git Bundler::Source::Git].include?(source.class.name)
  end

  def path_source?(source)
    %w[
      Gem::Source::Vendor
      Bundler::Source::Path
      Bundler::Source::Gemspec
    ].include?(source.class.name)
  end

  ##
  # The remote a source advertises.  A Gem::Source exposes +uri+, while a
  # Bundler source exposes a list of +remotes+; the first is the one a
  # lockfile would write.

  def source_uri(source)
    uri =
      if source.respond_to?(:uri)
        source.uri
      elsif source.respond_to?(:remotes)
        Array(source.remotes).first
      end

    return if uri.nil?

    text = uri.to_s
    text.empty? ? nil : text
  rescue StandardError
    nil
  end

  def git_entry(source)
    @source      = source
    @source_type = :git
    @repository  = (source.respond_to?(:repository) ? source.repository : source.uri).to_s
    @revision    = git_revision(source).to_s
    @remote      = @repository

    @missing << :revision if @revision.to_s.strip.empty?
  end

  def git_revision(source)
    if source.respond_to?(:rev_parse)
      source.rev_parse
    elsif source.respond_to?(:revision)
      source.revision
    end
  rescue StandardError
    # A reference such as a branch name is not a commit, so it is not
    # substituted here: the revision is reported as missing instead.
    nil
  end

  def path_entry(directory)
    @source_type = :path
    @path        = directory.to_s
    @remote      = @path

    @missing << :path if @path.empty?
  end

  def path_of(source)
    if source.respond_to?(:path)
      source.path
    elsif source.respond_to?(:uri)
      source.uri
    end
  end

  def installed_or_unknown(local, source = nil)
    if local
      @source      = source
      @source_type = :installed
      @missing << :remote
    else
      @source      = source
      @source_type = :unknown
      @missing << :source
    end
  end
end
