# frozen_string_literal: true

require_relative "helper"
require "rubygems/source"

class TestGemSourceGit < Gem::TestCase
  def setup
    super

    @name, @version, @repository, @head = git_gem

    @hash = OpenSSL::Digest::SHA1.hexdigest @repository

    @source = Gem::Source::Git.new @name, @repository, nil, false
  end

  def test_base_dir
    assert_equal File.join(Gem.dir, "bundler"), @source.base_dir

    @source.root_dir = "#{@gemhome}2"

    assert_equal File.join("#{@gemhome}2", "bundler"), @source.base_dir
  end

  def test_checkout
    @source.checkout

    assert_path_exist File.join @source.install_dir, "a.gemspec"
  end

  def test_checkout_default
    Dir.chdir @repository do
      default_branch = Gem::Util.popen(@git, "branch", "--show-current").strip
      system @git, "checkout", "-q", "-b", "other"
      system @git, "mv",             "a.gemspec", "b.gemspec"
      system @git, "commit",   "-q", "-a", "-m", "rename gemspec"
      system @git, "checkout", "-q", default_branch
    end

    @source = Gem::Source::Git.new @name, @repository, "other", false

    @source.checkout

    assert_path_exist File.join @source.install_dir, "b.gemspec"
  end

  def test_checkout_local
    @source.remote = false

    @source.checkout

    install_dir = File.join Gem.dir, "bundler", "gems", "a-#{@head[0..11]}"

    assert_path_not_exist File.join install_dir, "a.gemspec"
  end

  def test_checkout_local_cached
    @source.cache

    @source.remote = false

    @source.checkout

    assert_path_exist File.join @source.install_dir, "a.gemspec"
  end

  def test_checkout_submodules
    omit "JRuby on Windows hits git submodule path differences" if Gem.win_platform? && Gem.java_platform?

    # We need to allow to checkout submodules with file:// protocol
    # CVE-2022-39253
    # https://lore.kernel.org/lkml/xmqq4jw1uku5.fsf@gitster.g/
    system(@git, *%W[config --global protocol.file.allow always])

    source = Gem::Source::Git.new @name, @repository, nil, true

    git_gem "b"

    Dir.chdir "git/a" do
      require "open3"
      output, status = Open3.capture2e(@git, "submodule", "--quiet", "add", File.expand_path("../b"), "b")
      assert status.success?, output

      system @git, "commit", "--quiet", "-m", "add submodule b"
    end

    source.checkout

    assert_path_exist File.join source.install_dir, "a.gemspec"
    assert_path_exist File.join source.install_dir, "b/b.gemspec"
  end

  def test_cache
    assert @source.cache

    assert_path_exist @source.repo_cache_dir

    Dir.chdir @source.repo_cache_dir do
      assert_equal @head, Gem::Util.popen(@git, "rev-parse", "HEAD").strip
    end
  end

  def test_cache_local
    @source.remote = false

    @source.cache

    assert_path_not_exist @source.repo_cache_dir
  end

  def test_dir_shortref
    @source.cache

    assert_equal @head[0..11], @source.dir_shortref
  end

  def test_download
    refute @source.download nil, nil
  end

  def test_equals2
    assert_equal @source, @source

    assert_equal @source, @source.dup

    source =
      Gem::Source::Git.new @source.name, @source.repository, "other", false

    refute_equal @source, source

    source =
      Gem::Source::Git.new @source.name, "repo/other", @source.reference, false

    refute_equal @source, source

    source =
      Gem::Source::Git.new "b", @source.repository, @source.reference, false

    refute_equal @source, source

    source =
      Gem::Source::Git.new @source.name, @source.repository, @source.reference,
                           true

    refute_equal @source, source
  end

  def test_install_dir
    @source.cache

    expected = File.join Gem.dir, "bundler", "gems", "a-#{@head[0..11]}"

    assert_equal expected, @source.install_dir
  end

  def test_install_dir_local
    @source.remote = false

    assert_nil @source.install_dir
  end

  def test_repo_cache_dir
    expected =
      File.join Gem.dir, "cache", "bundler", "git", "a-#{@hash}"

    assert_equal expected, @source.repo_cache_dir

    @source.root_dir = "#{@gemhome}2"

    expected =
      File.join "#{@gemhome}2", "cache", "bundler", "git", "a-#{@hash}"

    assert_equal expected, @source.repo_cache_dir
  end

  def test_rev_parse
    @source.cache

    assert_equal @head, @source.rev_parse

    Dir.chdir @repository do
      system @git, "checkout", "--quiet", "-b", "other"
    end

    default_head = @head

    git_gem "a", 2

    source = Gem::Source::Git.new @name, @repository, "other", false

    source.cache

    refute_equal default_head, source.rev_parse

    source = Gem::Source::Git.new @name, @repository, "nonexistent", false

    source.cache

    e = assert_raise Gem::Exception do
      capture_subprocess_io { source.rev_parse }
    end

    assert_equal "unable to find reference nonexistent in #{@repository}",
                   e.message
  end

  def test_root_dir
    assert_equal Gem.dir, @source.root_dir

    @source.root_dir = "#{@gemhome}2"

    assert_equal "#{@gemhome}2", @source.root_dir
  end

  def test_spaceship
    git       = Gem::Source::Git.new "a", "git/a", nil, false
    remote    = Gem::Source.new @gem_repo
    installed = Gem::Source::Installed.new
    vendor    = Gem::Source::Vendor.new "vendor/foo"

    assert_equal(0, git.<=>(git),       "git <=> git") # rubocop:disable Lint/BinaryOperatorWithIdenticalOperands

    assert_equal(1, git.<=>(remote),    "git <=> remote")
    assert_equal(-1, remote.<=>(git), "remote <=> git")

    assert_equal(1, git.<=>(installed), "git <=> installed")
    assert_equal(-1, installed.<=>(git), "installed <=> git")

    assert_equal(-1, git.<=>(vendor), "git <=> vendor")
    assert_equal(1, vendor.<=>(git), "vendor <=> git")
  end

  def test_specs
    source = Gem::Source::Git.new @name, @repository, nil, true

    Dir.chdir "git/a" do
      FileUtils.mkdir "b"

      Dir.chdir "b" do
        b = Gem::Specification.new "b", 1

        File.open "b.gemspec", "w" do |io|
          io.write b.to_ruby
        end

        system @git, "add", "b.gemspec"
        system @git, "commit", "--quiet", "-m", "add b/b.gemspec"
      end
    end

    specs = nil

    capture_output do
      specs = source.specs
    end

    assert_equal %w[a-1 b-1], specs.map(&:full_name)

    a_spec = specs.shift

    base_dir = File.dirname File.dirname source.install_dir

    assert_equal source.install_dir, a_spec.full_gem_path
    assert_equal File.join(source.install_dir, "a.gemspec"), a_spec.loaded_from
    assert_equal base_dir, a_spec.base_dir

    extension_dir =
      File.join Gem.dir, "bundler", "extensions",
        Gem::Platform.local.to_s, Gem.extension_api_version,
        "a-#{source.dir_shortref}"

    assert_equal extension_dir, a_spec.extension_dir

    b_spec = specs.shift

    assert_equal File.join(source.install_dir, "b"), b_spec.full_gem_path
    assert_equal File.join(source.install_dir, "b", "b.gemspec"),
                 b_spec.loaded_from
    assert_equal base_dir, b_spec.base_dir

    assert_equal extension_dir, b_spec.extension_dir
  end

  def test_specs_local
    source = Gem::Source::Git.new @name, @repository, nil, true
    source.remote = false

    capture_output do
      assert_empty source.specs
    end
  end

  def test_specs_local_locked_objects
    # Lock a branch and a tag with the same name, both pointing at
    # version 1 of the gem.
    branch =
      Dir.chdir @repository do
        branch = Gem::Util.popen(@git, "branch", "--show-current").strip

        system @git, "tag", branch

        branch
      end

    @source.cache

    # Move the branch to version 2, the tag keeps pointing at version 1.
    _, _, _, branch_head = git_gem "a", 2

    @source.cache

    Dir.chdir @source.repo_cache_dir do
      assert_equal branch_head, Gem::Util.popen(@git, "rev-parse", "refs/heads/#{branch}").strip
      assert_equal @head,       Gem::Util.popen(@git, "rev-parse", "refs/tags/#{branch}").strip
    end

    refs_before =
      Dir.chdir @source.repo_cache_dir do
        Gem::Util.popen @git, "for-each-ref"
      end

    # The repository is no longer reachable, proving the reads below only
    # use the local cache.
    FileUtils.mv @repository, "#{@repository}.moved"

    # Two lock entries: the moved branch and the commit the same-named tag
    # points at.
    branch_source = Gem::Source::Git.new @name, @repository, branch, false
    branch_source.remote = false

    tag_source = Gem::Source::Git.new @name, @repository, @head, false
    tag_source.remote = false

    capture_output do
      assert_equal %w[a-2], branch_source.specs.map(&:full_name)

      assert_equal %w[a-1], tag_source.specs.map(&:full_name)

      # The previously fetched locked object is still usable.
      assert_equal %w[a-2], branch_source.specs.map(&:full_name)
    end

    assert_equal branch_head, branch_source.rev_parse
    assert_equal @head,       tag_source.rev_parse

    refute_equal branch_source.install_dir, tag_source.install_dir

    assert_path_exist File.join(branch_source.install_dir, "a.gemspec")
    assert_path_exist File.join(tag_source.install_dir, "a.gemspec")

    # Reading the specs did not update the cached refs.
    refs_after =
      Dir.chdir @source.repo_cache_dir do
        Gem::Util.popen @git, "for-each-ref"
      end

    assert_equal refs_before, refs_after
  end

  def test_checkout_local_rebinds_checkout_from_other_resolution
    _, _, _, head = git_gem "a", 2

    @source.cache

    source = Gem::Source::Git.new @name, @repository, @head, false
    source.remote = false

    # A checkout left behind by another resolution, bound to a different
    # commit than the locked object.
    system @git, "clone", "--quiet", @source.repo_cache_dir, source.install_dir

    Dir.chdir source.install_dir do
      assert_equal head, Gem::Util.popen(@git, "rev-parse", "HEAD").strip
    end

    capture_output do
      assert_equal %w[a-1], source.specs.map(&:full_name)
    end

    Dir.chdir source.install_dir do
      assert_equal @head, Gem::Util.popen(@git, "rev-parse", "HEAD").strip
    end
  end

  def test_checkout_local_missing_object
    @source.cache

    missing = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

    source = Gem::Source::Git.new @name, @repository, missing, false
    source.remote = false

    refs = lambda do
      Dir.chdir @source.repo_cache_dir do
        Gem::Util.popen @git, "for-each-ref"
      end
    end

    refs_before = refs.call

    # The repository is no longer reachable, proving no remote is consulted.
    FileUtils.mv @repository, "#{@repository}.moved"

    e = assert_raise Gem::Exception do
      capture_subprocess_io { source.checkout }
    end

    assert_equal "unable to find reference #{missing} in #{@repository}",
                 e.message

    assert_equal refs_before, refs.call
  end

  def test_uri
    assert_equal Gem::URI(@repository), @source.uri
  end

  def test_pretty_print
    assert_equal "#<Gem::Source::Git[Git: #{@repository} HEAD]>", @source.pretty_inspect.gsub(/\s+/, " ").strip
  end

  def test_uri_hash
    assert_equal @hash, @source.uri_hash

    source =
      Gem::Source::Git.new "a", "http://git@example/repo.git", nil, false

    assert_equal "291c4caac7feba8bb64c297987028acb3dde6cfe",
                 source.uri_hash

    source =
      Gem::Source::Git.new "a", "HTTP://git@EXAMPLE/repo.git", nil, false

    assert_equal "291c4caac7feba8bb64c297987028acb3dde6cfe",
                 source.uri_hash
  end
end
