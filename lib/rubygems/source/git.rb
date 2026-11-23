# frozen_string_literal: true

##
# A git gem for use in a gem dependencies file.
#
# Example:
#
#   source =
#     Gem::Source::Git.new 'rake', 'git@example:rake.git', 'rake-10.1.0', false
#
#   source.specs

class Gem::Source::Git < Gem::Source
  ##
  # The name of the gem created by this git gem.

  attr_reader :name

  ##
  # The commit reference used for checking out this git gem.

  attr_reader :reference

  ##
  # When false the cache for this repository will not be updated.

  attr_accessor :remote

  ##
  # The git repository this gem is sourced from.

  attr_reader :repository

  ##
  # The directory for cache and git gem installation

  attr_accessor :root_dir

  ##
  # Does this repository need submodules checked out too?

  attr_reader :need_submodules

  ##
  # Creates a new git gem source for a gems from loaded from +repository+ at
  # the given +reference+.  The +name+ is only used to track the repository
  # back to a gem dependencies file, it has no real significance as a git
  # repository may contain multiple gems.  If +submodules+ is true, submodules
  # will be checked out when the gem is installed.

  def initialize(name, repository, reference, submodules = false)
    require_relative "../uri"
    @uri = Gem::Uri.parse(repository)
    @name            = name
    @repository      = repository
    @reference       = reference || "HEAD"
    @need_submodules = submodules

    @remote   = true
    @root_dir = Gem.dir
  end

  def <=>(other)
    case other
    when Gem::Source::Git then
      0
    when Gem::Source::Vendor,
         Gem::Source::Lock then
      -1
    when Gem::Source then
      1
    end
  end

  def ==(other) # :nodoc:
    super &&
      @name            == other.name &&
      @repository      == other.repository &&
      @reference       == other.reference &&
      @need_submodules == other.need_submodules
  end

  def git_command
    ENV.fetch("git", "git")
  end

  ##
  # Checks out the files for the repository into the install_dir.
  #
  # The checkout is always bound to the locked commit selected by rev_parse.
  # When the cache must not be updated (@remote is false) no refs are
  # fetched and an existing checkout is only reused as-is when it is already
  # bound to that commit; otherwise it is re-bound from the local cache.

  def checkout # :nodoc:
    cache

    return false unless File.exist? repo_cache_dir

    revision = rev_parse

    previously_cloned = File.exist? install_dir

    unless previously_cloned
      system git_command, "clone", "--quiet", "--no-checkout",
             repo_cache_dir, install_dir
    end

    Dir.chdir install_dir do
      if @remote
        system git_command, "fetch", "--quiet", "--force", "--tags",
               repo_cache_dir
      elsif previously_cloned &&
            revision == Gem::Util.popen(git_command, "rev-parse", "HEAD").strip
        # The checkout is already bound to the locked object, so it can be
        # reused without updating any refs.
        return true
      end

      success = system git_command, "reset", "--quiet", "--hard", revision

      if @need_submodules
        require "open3"
        _, status = Open3.capture2e(git_command, "submodule", "update", "--quiet", "--init", "--recursive")

        success &&= status.success?
      end

      success
    end
  end

  ##
  # Creates a local cache repository for the git gem.

  def cache # :nodoc:
    return unless @remote

    if File.exist? repo_cache_dir
      Dir.chdir repo_cache_dir do
        system git_command, "fetch", "--quiet", "--force", "--tags",
               @repository, "refs/heads/*:refs/heads/*"
      end
    else
      system git_command, "clone", "--quiet", "--bare", "--no-hardlinks",
             @repository, repo_cache_dir
    end
  end

  ##
  # Directory where git gems get unpacked and so-forth.

  def base_dir # :nodoc:
    File.join @root_dir, "bundler"
  end

  ##
  # A short reference for use in git gem directories

  def dir_shortref # :nodoc:
    rev_parse[0..11]
  end

  ##
  # Nothing to download for git gems

  def download(full_spec, path) # :nodoc:
  end

  ##
  # The directory where the git gem will be installed.

  def install_dir # :nodoc:
    return unless File.exist? repo_cache_dir

    File.join base_dir, "gems", "#{@name}-#{dir_shortref}"
  end

  def pretty_print(q) # :nodoc:
    q.object_group(self) do
      q.group 2, "[Git: ", "]" do
        q.breakable
        q.text @repository

        q.breakable
        q.text @reference
      end
    end
  end

  ##
  # The directory where the git gem's repository will be cached.

  def repo_cache_dir # :nodoc:
    File.join @root_dir, "cache", "bundler", "git", "#{@name}-#{uri_hash}"
  end

  ##
  # Converts the git reference for the repository into a commit hash.
  #
  # A full commit SHA selects that locked object directly, even when no ref
  # in the cached repository points at it.  Branch and tag names are
  # resolved through their explicit refs so that a branch and a tag sharing
  # the same name stay distinct.
  #
  # Raises Gem::Exception when the object cannot be found in the cached
  # repository.

  def rev_parse # :nodoc:
    hash = nil

    Dir.chdir repo_cache_dir do
      hash = ref_to_commit @reference
    end

    raise Gem::Exception,
          "unable to find reference #{@reference} in #{@repository}" unless hash

    hash
  end

  ##
  # Resolves +reference+ to a commit hash in the repository in the current
  # directory.  Returns nil when the object is not present locally; nothing
  # is fetched and no refs are updated.

  def ref_to_commit(reference) # :nodoc:
    candidates =
      if /\A[0-9a-f]{40}\z/i.match? reference
        # A full commit SHA names the locked object itself.
        ["#{reference}^{commit}"]
      else
        # Resolve branches and tags through their explicit refs so a branch
        # and a tag with the same name do not shadow each other.
        ["refs/heads/#{reference}", "refs/tags/#{reference}", reference]
      end

    candidates.each do |candidate|
      hash = Gem::Util.popen git_command, "rev-parse", "--verify", "--quiet",
                             "#{candidate}^{commit}"

      hash = hash.strip if hash

      return hash unless hash.nil? || hash.empty?
    end

    nil
  end

  ##
  # Loads all gemspecs in the repository
  #
  # The gemspecs (and thus the gem name, version and dependencies) are read
  # from the checkout of the locked commit selected by rev_parse, so each
  # locked object yields the metadata recorded in that commit.

  def specs
    checkout

    return [] unless install_dir

    Dir.chdir install_dir do
      Dir["{,*,*/*}.gemspec"].filter_map do |spec_file|
        directory = File.dirname spec_file
        file      = File.basename spec_file

        Dir.chdir directory do
          # Expand the path so Gem::Specification.load caches the gemspec
          # under the checkout of this locked object, not under a relative
          # name another checkout may reuse.
          spec = Gem::Specification.load File.expand_path(file)
          if spec
            spec.base_dir = base_dir

            spec.extension_dir =
              File.join base_dir, "extensions", Gem::Platform.local.to_s,
                Gem.extension_api_version, "#{name}-#{dir_shortref}"

            spec.full_gem_path = File.dirname spec.loaded_from if spec
          end
          spec
        end
      end
    end
  end

  ##
  # A hash for the git gem based on the git repository Gem::URI.

  def uri_hash # :nodoc:
    require_relative "../openssl"

    normalized =
      if @repository.match?(%r{^\w+://(\w+@)?})
        uri = Gem::URI(@repository).normalize.to_s.sub %r{/$},""
        uri.sub(/\A(\w+)/) { $1.downcase }
      else
        @repository
      end

    OpenSSL::Digest::SHA1.hexdigest normalized
  end
end
