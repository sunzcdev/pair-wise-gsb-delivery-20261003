# frozen_string_literal: true

# rubocop:disable Style/AsciiComments

# Copyright (C) 2004 Mauricio Julio Fernández Pradier
# See LICENSE.txt for additional licensing information.

# rubocop:enable Style/AsciiComments

require_relative "win_platform"
require_relative "security"
require_relative "user_interaction"

##
# Example using a Gem::Package
#
# Builds a .gem file given a Gem::Specification. A .gem file is a tarball
# which contains a data.tar.gz, metadata.gz, checksums.yaml.gz and possibly
# signatures.
#
#   require 'rubygems'
#   require 'rubygems/package'
#
#   spec = Gem::Specification.new do |s|
#     s.summary = "Ruby based make-like utility."
#     s.name = 'rake'
#     s.version = PKG_VERSION
#     s.requirements << 'none'
#     s.files = PKG_FILES
#     s.description = <<-EOF
#   Rake is a Make-like program implemented in Ruby. Tasks
#   and dependencies are specified in standard Ruby syntax.
#     EOF
#   end
#
#   Gem::Package.build spec
#
# Reads a .gem file.
#
#   require 'rubygems'
#   require 'rubygems/package'
#
#   the_gem = Gem::Package.new(path_to_dot_gem)
#   the_gem.contents # get the files in the gem
#   the_gem.extract_files destination_directory # extract the gem into a directory
#   the_gem.spec # get the spec out of the gem
#   the_gem.verify # check the gem is OK (contains valid gem specification, contains a not corrupt contents archive)
#   the_gem.integrity_report # what the read actually verified, without raising
#
# #files are the files in the .gem tar file, not the Ruby files in the gem
# #extract_files and #contents automatically call #verify

class Gem::Package
  include Gem::UserInteraction

  class Error < Gem::Exception; end

  class FormatError < Error
    attr_reader :path

    def initialize(message, source = nil)
      if source
        @path = source.is_a?(String) ? source : source.path

        message += " in #{path}" if path
      end

      super message
    end
  end

  class PathError < Error
    def initialize(destination, destination_dir)
      super format("installing into parent path %s of %s is not allowed", destination, destination_dir)
    end
  end

  class SymlinkError < Error
    def initialize(name, destination, destination_dir)
      super format("installing symlink '%s' pointing to parent path %s of %s is not allowed", name, destination, destination_dir)
    end
  end

  class NonSeekableIO < Error; end

  class TooLongFileName < Error; end

  ##
  # Raised when a tar file is corrupt

  class TarInvalidError < Error; end

  ##
  # Raised when a filename contains characters that are invalid on Windows

  class InvalidWindowsFileNameError < Gem::InstallError
    def initialize(filename, gem_name = nil)
      message = "The gem contains a file '#{filename}' with characters in its name that are not allowed on Windows (e.g., colons)."
      message += " This is a problem with the '#{gem_name}' gem, not Rubygems." if gem_name
      message += " Please report this issue to the gem author."
      super message
    end
  end

  ##
  # One integrity check performed while reading a package, as reported by
  # Gem::Package#integrity_report.
  #
  # The +status+ separates the three things that can happen to a check:
  #
  # [VERIFIED] the package supplied the evidence for this check and it
  #            matched what was read
  # [FAILED] the package supplied evidence that did not match, or the data
  #          needed to finish the check could not be read
  # [NOT_PROVIDED] the package does not carry this evidence at all, so
  #                there was nothing to verify. This is not a failure: gems
  #                built before checksums.yaml.gz existed have no checksums
  #                to check.
  #
  # The +kind+ names the stage that produced the check:
  #
  # [:package] the read could not start or ended unexpectedly
  # [:read] a package member was read from start to end
  # [:structure] the package carries the members a gem must have
  # [:specification] the metadata was parsed into a Gem::Specification
  # [:manifest] checksums.yaml.gz was present and parsed
  # [:checksum] a member digest was compared against checksums.yaml.gz
  # [:signature] signatures were verified by the security policy

  class IntegrityCheck
    VERIFIED = :verified
    FAILED = :failed
    NOT_PROVIDED = :not_provided

    STATUSES = [VERIFIED, FAILED, NOT_PROVIDED].freeze
    KINDS = [:package, :read, :structure, :specification, :manifest, :checksum, :signature].freeze
    private_constant :STATUSES, :KINDS

    ##
    # The stage that produced this check, one of the kinds listed above.

    attr_reader :kind

    ##
    # What was checked, for example "data.tar.gz", "specification", or
    # "signatures".

    attr_reader :subject

    ##
    # One of VERIFIED, FAILED, or NOT_PROVIDED.

    attr_reader :status

    ##
    # Human readable explanation of the status, or nil.

    attr_reader :detail

    def initialize(kind, subject, status, detail = nil)
      unless KINDS.include? kind
        raise ArgumentError, "unknown integrity check kind #{kind.inspect}"
      end
      unless STATUSES.include? status
        raise ArgumentError, "unknown integrity check status #{status.inspect}"
      end

      @kind = kind
      @subject = subject
      @status = status
      @detail = detail
    end

    def verified?
      status == VERIFIED
    end

    def failed?
      status == FAILED
    end

    def not_provided?
      status == NOT_PROVIDED
    end

    def to_h
      { kind: kind, subject: subject, status: status, detail: detail }
    end

    def to_s
      detail ? "#{kind} #{subject}: #{status} (#{detail})" : "#{kind} #{subject}: #{status}"
    end
  end

  ##
  # The result of reading a package, as returned by
  # Gem::Package#integrity_report.
  #
  # Unlike Gem::Package#verify it does not raise. A failure #verify would
  # have raised is available from #error, and every check the read reached
  # is listed in #checks with its own IntegrityCheck status, so callers can
  # tell evidence that did not match from evidence the package never
  # supplied.
  #
  # A package that failed its read is not installable, even if its metadata
  # was readable before the failure: #installable? is the judgement callers
  # should use before delivering the package as a normal installation.

  class IntegrityReport
    ##
    # Accumulates checks while a package is read. Internal to
    # Gem::Package#integrity_report.

    class Recorder # :nodoc:
      attr_reader :checks

      def initialize
        @checks = []
      end

      def record(kind, subject, status, detail = nil)
        @checks << IntegrityCheck.new(kind, subject, status, detail)
        self
      end

      def failure(kind, subject, error)
        record kind, subject, IntegrityCheck::FAILED, error.message
      end

      def failed?
        @checks.any?(&:failed?)
      end

      def recorded?(kind, subject)
        @checks.any? {|check| check.kind == kind && check.subject == subject }
      end
    end

    ##
    # Overall status of a package whose read completed and whose supplied
    # evidence all matched, but which did not supply every kind of evidence
    # (for example, a gem with no checksums.yaml.gz).

    UNVERIFIED = :unverified

    ##
    # The IntegrityCheck objects recorded by the read, in the order the
    # read reached them.

    attr_reader :checks

    ##
    # The exception Gem::Package#verify would have raised, or nil when the
    # read completed.

    attr_reader :error

    def initialize(checks, error = nil)
      @checks = checks.freeze
      @error = error
    end

    ##
    # The overall outcome: :failed if any check failed or the read raised,
    # :verified if every check was verified, and :unverified when the read
    # completed and every piece of supplied evidence matched but some
    # evidence was not supplied. A read that recorded no checks at all, such
    # as the old format which has no evidence to offer, is :unverified.

    def status
      return IntegrityCheck::FAILED if failed?
      return UNVERIFIED if checks.empty?
      return IntegrityCheck::VERIFIED if checks.all?(&:verified?)

      UNVERIFIED
    end

    def verified?
      status == IntegrityCheck::VERIFIED
    end

    def failed?
      !error.nil? || checks.any?(&:failed?)
    end

    ##
    # Whether the package may be delivered as a normally installable gem.
    #
    # A package whose read failed is not installable even when its metadata
    # was readable, because the rest of it could not be checked. A package
    # that merely omitted evidence, such as a gem built before
    # checksums.yaml.gz existed, is still installable, as it always has
    # been; it is just not fully verified.

    def installable?
      !failed?
    end

    def checks_for(kind, subject = nil)
      checks.select do |check|
        check.kind == kind && (subject.nil? || check.subject == subject)
      end
    end

    def verified_subjects(kind = nil)
      subjects_with_status IntegrityCheck::VERIFIED, kind
    end

    def failed_subjects(kind = nil)
      subjects_with_status IntegrityCheck::FAILED, kind
    end

    def not_provided_subjects(kind = nil)
      subjects_with_status IntegrityCheck::NOT_PROVIDED, kind
    end

    def to_h
      {
        status: status,
        installable: installable?,
        error: error ? { class: error.class.name, message: error.message } : nil,
        checks: checks.map(&:to_h),
      }
    end

    def to_s
      lines = ["integrity: #{status}", "installable: #{installable?}"]
      checks.each {|check| lines << "  #{check}" }
      lines << "  error: #{error.class}: #{error.message}" if error
      lines.join "\n"
    end

    private

    def subjects_with_status(status, kind)
      selected = checks.select {|check| check.status == status }
      selected = selected.select {|check| check.kind == kind } if kind
      selected.map(&:subject).uniq
    end
  end

  attr_accessor :build_time # :nodoc:

  ##
  # Checksums for the contents of the package

  attr_reader :checksums

  ##
  # The files in this package.  This is not the contents of the gem, just the
  # files in the top-level container.

  attr_reader :files

  ##
  # Reference to the gem being packaged.

  attr_reader :gem

  ##
  # The security policy used for verifying the contents of this package.

  attr_accessor :security_policy

  ##
  # Sets the Gem::Specification to use to build this package.

  attr_writer :spec

  ##
  # Permission for directories
  attr_accessor :dir_mode

  ##
  # Permission for program files
  attr_accessor :prog_mode

  ##
  # Permission for other files
  attr_accessor :data_mode

  ##
  # The minimum RubyGems version that can install content-addressable gems.
  # Built into +required_rubygems_version+ so older clients reject skinny
  # gems through both the local and remote install paths.

  MINIMUM_RUBYGEMS_VERSION = ">= 4.1.0.a"

  ##
  # Builds the gem described by +spec+ and returns the built file name.
  #
  # When +content_addressable+ is true, builds a gem named by the SHA-256
  # digest of its contents. The spec must declare a non-Ruby platform and a
  # +required_ruby_version+ identifying a single Ruby ABI. Its
  # +required_rubygems_version+ is constrained to at least
  # MINIMUM_RUBYGEMS_VERSION. Content-addressable builds are incompatible
  # with +file_name+.

  def self.build(spec, skip_validation = false, strict_validation = false, file_name = nil, content_addressable: false)
    if content_addressable && file_name
      raise ArgumentError, "Cannot specify an output file name for a content-addressable gem as these gems must use the generated file name."
    end
    if content_addressable
      require "digest"
      require "stringio"

      io = StringIO.new
      io.set_encoding(Encoding::BINARY)

      package = new io
      package.spec = spec.dup
      gem_file = package.build_content_addressable_file skip_validation, strict_validation

      spec.required_rubygems_version = package.spec.required_rubygems_version
    else
      gem_file = file_name || spec.file_name

      package = new gem_file
      package.spec = spec
      package.build skip_validation, strict_validation
    end
    gem_file
  end

  ##
  # Creates a new Gem::Package for the file at +gem+. +gem+ can also be
  # provided as an IO object.
  #
  # If +gem+ is an existing file in the old format a Gem::Package::Old will be
  # returned.

  def self.new(gem, security_policy = nil)
    gem = if gem.is_a?(Gem::Package::Source)
      gem
    elsif gem.respond_to? :read
      Gem::Package::IOSource.new gem
    else
      Gem::Package::FileSource.new gem
    end

    return super unless self == Gem::Package
    return super unless gem.present?

    return super unless gem.start
    return super unless gem.start.include? "MD5SUM ="

    Gem::Package::Old.new gem, security_policy
  end

  ##
  # Extracts the Gem::Specification and raw metadata from the .gem file at
  # +path+.
  #--

  def self.raw_spec(path, security_policy = nil)
    format = new(path, security_policy)
    spec = format.spec

    metadata = nil

    File.open path, Gem.binary_mode do |io|
      tar = Gem::Package::TarReader.new io
      tar.each_entry do |entry|
        case entry.full_name
        when "metadata" then
          metadata = entry.read
        when "metadata.gz" then
          metadata = Gem::Util.gunzip entry.read
        end
      end
    end

    [spec, metadata]
  end

  ##
  # Creates a new package that will read or write to the file +gem+.

  def initialize(gem, security_policy) # :notnew:
    require "zlib"

    @gem = gem

    @build_time      = Gem.source_date_epoch
    @checksums       = {}
    @contents        = nil
    @digests         = Hash.new {|h, algorithm| h[algorithm] = {} }
    @files           = nil
    @security_policy = security_policy
    @signatures      = {}
    @signer          = nil
    @spec            = nil

    @integrity_recorder = nil
  end

  ##
  # Copies this package to +path+ (if possible)

  def copy_to(path)
    FileUtils.cp @gem.path, path unless File.exist? path
  end

  ##
  # Derives the content address from the gem's file name and verifies it
  # against the SHA256 digest of the file contents.

  def content_address
    path = @gem&.path
    return unless path

    address = Gem::ContentAddress.verified_file_name_claim(path, spec)
    return nil unless address
    return nil unless Gem::ContentAddress.eligible?(spec)

    address
  end

  ##
  # Adds a checksum for each entry in the gem to checksums.yaml.gz.

  def add_checksums(tar)
    Gem.load_yaml

    checksums_by_algorithm = Hash.new {|h, algorithm| h[algorithm] = {} }

    @checksums.each do |name, digests|
      digests.each do |algorithm, digest|
        checksums_by_algorithm[algorithm][name] = digest.hexdigest
      end
    end

    tar.add_file_signed "checksums.yaml.gz", 0o444, @signer do |io|
      gzip_to io do |gz_io|
        if Gem.use_psych?
          Psych.dump checksums_by_algorithm, gz_io
        else
          gz_io.write Gem::YAMLSerializer.dump(checksums_by_algorithm)
        end
      end
    end
  end

  ##
  # Adds the files listed in the packages's Gem::Specification to data.tar.gz
  # and adds this file to the +tar+.

  def add_contents(tar) # :nodoc:
    digests = tar.add_file_signed "data.tar.gz", 0o444, @signer do |io|
      gzip_to io do |gz_io|
        Gem::Package::TarWriter.new gz_io do |data_tar|
          add_files data_tar
        end
      end
    end

    @checksums["data.tar.gz"] = digests
  end

  ##
  # Adds files included the package's Gem::Specification to the +tar+ file

  def add_files(tar) # :nodoc:
    @spec.files.each do |file|
      if invalid_windows_filename?(file)
        alert_warning "filename '#{file}' contains characters that are invalid on Windows (e.g., colons). This gem may fail to install on Windows."
      end

      stat = File.lstat file

      if stat.symlink?
        tar.add_symlink file, File.readlink(file), stat.mode
      end

      next unless stat.file?

      tar.add_file_simple file, stat.mode, stat.size do |dst_io|
        File.open file, "rb" do |src_io|
          copy_stream(src_io, dst_io, stat.size)
        end
      end
    end
  end

  ##
  # Adds the package's Gem::Specification to the +tar+ file

  def add_metadata(tar) # :nodoc:
    digests = tar.add_file_signed "metadata.gz", 0o444, @signer do |io|
      gzip_to io do |gz_io|
        gz_io.write @spec.to_yaml
      end
    end

    @checksums["metadata.gz"] = digests
  end

  ##
  # Builds this package based on the specification set by #spec=

  def build(skip_validation = false, strict_validation = false)
    raise ArgumentError, "skip_validation = true and strict_validation = true are incompatible" if skip_validation && strict_validation

    Gem.load_yaml

    @spec.validate true, strict_validation unless skip_validation

    setup_signer(
      signer_options: {
        expiration_length_days: Gem.configuration.cert_expiration_length_days,
      }
    )

    @gem.with_write_io do |gem_io|
      Gem::Package::TarWriter.new gem_io do |gem|
        add_metadata gem
        add_contents gem
        add_checksums gem
      end
    end

    message = <<-EOM
  Successfully built RubyGem
  Name: #{@spec.name}
  Version: #{@spec.version}
EOM

    message += "  File: #{File.basename(@gem.path)}\n" if @gem.path
    say message
  ensure
    @signer = nil
  end

  ##
  # Builds this package and writes it to a content-addressable file name
  # derived from the SHA-256 digest of the gem contents, for example
  # "example-1.0-01234567.gem". Returns the written file name.
  #
  # The spec is validated for content addressing. Its
  # +required_ruby_version+ determines the Ruby ABI, while its
  # +required_rubygems_version+ is constrained before building.

  def build_content_addressable_file(skip_validation = false, strict_validation = false)
    Gem::ContentAddress.eligible!(@spec)
    ruby_abi = Gem::ContentAddress.ruby_abi_for(@spec.required_ruby_version)
    @spec.required_rubygems_version = normalized_required_rubygems_version(ruby_abi)

    build skip_validation, strict_validation

    bytes = @gem.with_read_io(&:read)
    gem_file = "#{@spec.name}-#{@spec.version}-#{Gem::ContentAddress.address_for(bytes)}.gem"
    File.binwrite(gem_file, bytes)

    say "  File: #{gem_file}"
    say "  Platform: #{@spec.platform}"
    say "  Ruby ABI: #{ruby_abi}"

    gem_file
  end

  ##
  # A list of file names contained in this gem

  def contents
    return @contents if @contents

    verify unless @spec

    @contents = []

    @gem.with_read_io do |io|
      gem_tar = Gem::Package::TarReader.new io

      gem_tar.each do |entry|
        next unless entry.full_name == "data.tar.gz"

        open_tar_gz entry do |pkg_tar|
          pkg_tar.each do |contents_entry|
            @contents << contents_entry.full_name
          end
        end

        return @contents
      end
    end
  rescue Zlib::GzipFile::Error, EOFError, Gem::Package::TarInvalidError => e
    raise Gem::Package::FormatError.new e.message, @gem
  end

  ##
  # Creates a digest of the TarEntry +entry+ from the digest algorithm set by
  # the security policy.

  def digest(entry) # :nodoc:
    algorithms = digest_algorithms

    return @digests if algorithms.empty?

    buf = String.new(capacity: 16_384, encoding: Encoding::BINARY)
    until entry.eof?
      entry.readpartial(16_384, buf)
      algorithms.each_value {|digester| digester << buf }
    end
    entry.rewind

    algorithms.each do |algorithm, digester|
      @digests[algorithm][entry.full_name] = digester
    end

    @digests
  end

  ##
  # Extracts the files in this package into +destination_dir+
  #
  # If +pattern+ is specified, only entries matching that glob will be
  # extracted.

  def extract_files(destination_dir, pattern = "*")
    verify unless @spec

    FileUtils.mkdir_p destination_dir, mode: dir_mode && 0o755

    @gem.with_read_io do |io|
      reader = Gem::Package::TarReader.new io

      reader.each do |entry|
        next unless entry.full_name == "data.tar.gz"

        extract_tar_gz entry, destination_dir, pattern

        break # ignore further entries
      end
    end
  rescue Zlib::GzipFile::Error, EOFError, Gem::Package::TarInvalidError => e
    raise Gem::Package::FormatError.new e.message, @gem
  end

  ##
  # Extracts all the files in the gzipped tar archive +io+ into
  # +destination_dir+.
  #
  # If an entry in the archive contains a relative path above
  # +destination_dir+ or an absolute path is encountered an exception is
  # raised.
  #
  # If +pattern+ is specified, only entries matching that glob will be
  # extracted.

  def extract_tar_gz(io, destination_dir, pattern = "*") # :nodoc:
    destination_dir = File.realpath(destination_dir)

    directories = []
    symlinks = []

    open_tar_gz io do |tar|
      tar.each do |entry|
        full_name = entry.full_name
        next unless File.fnmatch pattern, full_name, File::FNM_DOTMATCH

        if Gem.win_platform? && invalid_windows_filename?(full_name)
          gem_name = @spec ? @spec.full_name : "unknown"
          raise Gem::Package::InvalidWindowsFileNameError.new(full_name, gem_name)
        end

        destination = install_location full_name, destination_dir

        if entry.symlink?
          link_target = entry.header.linkname
          real_destination = File.expand_path(link_target, File.dirname(destination))

          raise Gem::Package::SymlinkError.new(full_name, real_destination, destination_dir) unless
            normalize_path(real_destination).start_with? normalize_path(destination_dir + "/")

          symlinks << [full_name, link_target, destination, real_destination]
        end

        mkdir =
          if entry.directory?
            destination
          else
            File.dirname destination
          end

        unless directories.include?(mkdir)
          FileUtils.mkdir_p mkdir, mode: dir_mode ? 0o755 : (entry.header.mode & 0o777 if entry.directory?)
          directories << mkdir
        end

        verify_extraction_dir mkdir, destination_dir

        if entry.file?
          raise Gem::Package::PathError.new(destination, destination_dir) if
            File.symlink?(destination)

          File.open(destination, "wb") do |out|
            copy_stream(tar.io, out, entry.size)
            # Flush needs to happen before chmod because there could be data
            # in the IO buffer that needs to be written, and that could be
            # written after the chmod (on close) which would mess up the perms
            out.flush
            out.chmod file_mode(entry.header.mode) & ~File.umask
          end
        end

        verbose destination
      end
    end

    symlinks.each do |name, target, destination, real_destination|
      if File.exist?(real_destination)
        create_symlink(target, destination)
      else
        alert_warning "#{@spec.full_name} ships with a dangling symlink named #{name} pointing to missing #{target} file. Ignoring"
      end
    end

    if dir_mode && !directories.empty?
      File.chmod(dir_mode, *directories)
    end
  end

  def file_mode(mode) # :nodoc:
    ((mode & 0o111).zero? ? data_mode : prog_mode) ||
      # If we're not using one of the default modes, then we're going to fall
      # back to the permission bits from the tarball. Masking drops setuid,
      # setgid and sticky bits along with anything that does not fit into a
      # mode in CRuby (an unsigned short).
      (mode & 0o777)
  end

  ##
  # Gzips content written to +gz_io+ to +io+.
  #--
  # Also sets the gzip modification time to the package build time to ease
  # testing.

  def gzip_to(io) # :yields: gz_io
    gz_io = Zlib::GzipWriter.new io, Zlib::BEST_COMPRESSION
    gz_io.mtime = @build_time

    yield gz_io
  ensure
    gz_io.close
  end

  if Gem.win_platform?
    def normalize_path(pathname) # :nodoc:
      pathname.downcase
    end
  else
    def normalize_path(pathname) # :nodoc:
      pathname
    end
  end

  ##
  # Checks if a filename contains characters that are invalid on Windows.
  # Windows doesn't allow: < > : " | ? * \ and control characters (0x00-0x1F).
  # Colons are the most common issue since they're allowed on Unix.
  # Note: Colons are only valid as drive letter separators (e.g., C:), not in filenames.

  def invalid_windows_filename?(filename) # :nodoc:
    filename.to_s.split("/").any? {|part| part.match?(/[:<>"|?*\\\x00-\x1f]/) }
  end

  ##
  # Loads a Gem::Specification from the TarEntry +entry+

  def load_spec_from_metadata(entry) # :nodoc:
    limit = 10 * 1024 * 1024
    case entry.full_name
    when "metadata" then
      @spec = Gem::Specification.from_yaml limit_read(entry, "metadata", limit)
    when "metadata.gz" then
      Zlib::GzipReader.wrap(entry, external_encoding: Encoding::UTF_8) do |gzio|
        @spec = Gem::Specification.from_yaml limit_read(gzio, "metadata.gz", limit)
      end
    end
  end

  ##
  # Opens +io+ as a gzipped tar archive

  def open_tar_gz(io) # :nodoc:
    Zlib::GzipReader.wrap io do |gzio|
      tar = Gem::Package::TarReader.new gzio

      yield tar
    ensure
      # Consume remaining gzip data to prevent the
      # "attempt to close unfinished zstream; reset forced" warning
      # when the GzipReader is closed with unconsumed compressed data.
      begin
        IO.copy_stream(gzio, IO::NULL)
      rescue Zlib::GzipFile::Error, IOError
        nil
      end
    end
  end

  ##
  # Reads and loads checksums.yaml.gz from the tar file +gem+
  #
  # The checksums file is the package's own evidence that the parts named in
  # it are intact. A package built before checksums.yaml.gz existed has none,
  # which #integrity_report reports as not provided rather than as a failure.

  def read_checksums(gem)
    Gem.load_yaml

    present = false

    @checksums = gem.seek "checksums.yaml.gz" do |entry|
      present = true
      Zlib::GzipReader.wrap entry do |gz_io|
        Gem::SafeYAML.safe_load limit_read(gz_io, "checksums.yaml.gz", 10 * 1024 * 1024)
      end
    end

    if present
      integrity_record :manifest, "checksums.yaml.gz", IntegrityCheck::VERIFIED
    else
      integrity_record :manifest, "checksums.yaml.gz", IntegrityCheck::NOT_PROVIDED,
                        "package has no checksum manifest"
    end
  rescue StandardError => e
    integrity_record :manifest, "checksums.yaml.gz", IntegrityCheck::FAILED, e.message
    raise
  end

  ##
  # Prepares the gem for signing and checksum generation.  If a signing
  # certificate and key are not present only checksum generation is set up.

  def setup_signer(signer_options: {})
    passphrase = ENV["GEM_PRIVATE_KEY_PASSPHRASE"]
    if @spec.signing_key
      @signer =
        Gem::Security::Signer.new(
          @spec.signing_key,
          @spec.cert_chain,
          passphrase,
          signer_options
        )

      @spec.signing_key = nil
      @spec.cert_chain = @signer.cert_chain.map(&:to_s)
    else
      @signer = Gem::Security::Signer.new nil, nil, passphrase
      @spec.cert_chain = @signer.cert_chain.map(&:to_pem) if
        @signer.cert_chain
    end
  end

  ##
  # The spec for this gem.
  #
  # If this is a package for a built gem the spec is loaded from the
  # gem and returned.  If this is a package for a gem being built the provided
  # spec is returned.

  def spec
    verify unless @spec

    @spec
  end

  ##
  # Verifies that this gem:
  #
  # * Contains a valid gem specification
  # * Contains a contents archive
  # * The contents archive is not corrupt
  #
  # After verification the gem specification from the gem is available from
  # #spec
  #
  # Raises Gem::Package::FormatError or Gem::Security::Exception when the
  # package is not intact. Use #integrity_report for the same checks without
  # exceptions.

  def verify
    @files     = []
    @spec      = nil

    read_package

    verify_checksums @digests, @checksums

    record_unchecked_members

    verify_package_signatures

    true
  rescue Gem::Security::Exception
    reset_verification
    raise
  rescue Errno::ENOENT => e
    reset_verification
    raise Gem::Package::FormatError.new e.message
  rescue Zlib::GzipFile::Error, EOFError, Gem::Package::TarInvalidError => e
    reset_verification
    raise Gem::Package::FormatError.new e.message, @gem
  rescue StandardError
    # Any other integrity failure, including a checksum mismatch or missing
    # member found while reading. The metadata may already be loaded by now,
    # but the package as a whole was not verified, so drop it rather than
    # leaving #spec able to return metadata from a package that failed.
    reset_verification
    raise
  end

  ##
  # Reads the package members, the checksum manifest, and the metadata into
  # the package state used by #verify.

  def read_package # :nodoc:
    @gem.with_read_io do |io|
      Gem::Package::TarReader.new io do |reader|
        read_checksums reader

        verify_files reader
      end
    end
  end

  ##
  # Discards the partially loaded state of a failed verification so that
  # #spec does not hand back metadata from a package whose integrity check
  # failed.

  def reset_verification # :nodoc:
    @spec  = nil
    @files = []
  end

  ##
  # The digest algorithms to compute for each member, from the algorithms
  # recorded in checksums.yaml.gz, or the security default when there is no
  # manifest.

  def digest_algorithms # :nodoc:
    if @checksums
      @checksums.to_h {|algorithm, _| [algorithm, Gem::Security.create_digest(algorithm)] }
    elsif Gem::Security::DIGEST_NAME
      { Gem::Security::DIGEST_NAME => Gem::Security.create_digest(Gem::Security::DIGEST_NAME) }
    else
      {}
    end
  end

  ##
  # Reports what reading this package actually checked, without raising for
  # an integrity problem.
  #
  # The report is produced by the same read #verify performs, so it reflects
  # the stages the read reached and the checks it ran. Each check has one of
  # three statuses: verified when the package supplied matching evidence,
  # failed when evidence did not match or the data needed for the check could
  # not be read, and not_provided when the package carries no such evidence
  # at all (gems built before checksums.yaml.gz existed have no checksums to
  # check, which is not the same as corrupt data).
  #
  # A package whose metadata is readable but whose read fails afterwards is
  # reported as failed, and #installable? is false for it. The metadata it
  # managed to read is not left behind for #spec either, so a caller cannot
  # mistake that package for a normal installable result. Packages that
  # merely omit evidence remain installable, as they are for #verify, but
  # are not fully verified.
  #
  # Any exception #verify would have raised is available from
  # Gem::Package::IntegrityReport#error.

  def integrity_report
    recorder = IntegrityReport::Recorder.new
    @integrity_recorder = recorder

    error = nil
    begin
      verify
    rescue StandardError => e
      error = e
      # The check that failed usually recorded itself with the stage that
      # produced it, which is more specific than the exception. Only add a
      # package-level failure when nothing else did.
      recorder.failure(:package, "package", e) unless recorder.failed?
    ensure
      @integrity_recorder = nil
    end

    IntegrityReport.new recorder.checks, error
  end

  private

  ##
  # Records a check in the report being built by #integrity_report. Does
  # nothing when no report is being built, so the read path is unchanged for
  # #verify and its callers.

  def integrity_record(kind, subject, status, detail = nil)
    @integrity_recorder&.record kind, subject, status, detail
  end

  ##
  # Whether the report being built already has a check for +subject+ in
  # +kind+. Used to record each member once even when multiple readers reach
  # it.

  def integrity_recorded?(kind, subject)
    @integrity_recorder&.recorded? kind, subject
  end

  ##
  # The +required_rubygems_version+ for a content-addressable build: the
  # spec's requirement raised to at least MINIMUM_RUBYGEMS_VERSION, warning
  # if it had to be changed. Raises if the requirement excludes every version
  # satisfying that floor, since no RubyGems could install the built gem.

  def normalized_required_rubygems_version(ruby_abi)
    minimum = Gem::Requirement.new(MINIMUM_RUBYGEMS_VERSION)
    existing = @spec.required_rubygems_version

    return minimum if existing.nil? || existing == Gem::Requirement.default

    floor = minimum.requirements.first.last

    if excludes_rubygems_floor?(existing, floor)
      raise ArgumentError,
        "Cannot build gem for Ruby ABI #{ruby_abi} because required_rubygems_version is set to #{existing}, " \
        "which excludes RubyGems #{MINIMUM_RUBYGEMS_VERSION} required to install content addressable gems. " \
        "Please remove or loosen the conflicting constraint."
    end

    return existing if satisfies_rubygems_floor?(existing, floor)

    preserved = existing.requirements.filter_map do |op, version|
      "#{op} #{version}" if ["~>", "<", "<=", "!="].include?(op)
    end

    normalized = Gem::Requirement.new([MINIMUM_RUBYGEMS_VERSION, *preserved])

    alert_warning \
      "required_rubygems_version was changed from \"#{existing}\" to \"#{normalized}\" for this build " \
      "because content addressable gems can only be installed by RubyGems #{MINIMUM_RUBYGEMS_VERSION}."

    normalized
  end

  ##
  # Whether +requirement+ excludes every RubyGems version satisfying the
  # +floor+, so that no RubyGems could install the built gem.

  def excludes_rubygems_floor?(requirement, floor)
    capped_below_floor = requirement.requirements.any? do |op, version|
      case op
      when "<" then version <= floor
      when "<=", "=" then version < floor
      when "~>" then version.bump <= floor.release
      else false
      end
    end

    return true if capped_below_floor

    !requirement.satisfied_by?(floor) && requirement.requirements.any? do |op, version|
      ["<=", "="].include?(op) && version == floor
    end
  end

  ##
  # Whether one of the lower bounds of +requirement+ already guarantees the
  # +floor+.

  def satisfies_rubygems_floor?(requirement, floor)
    requirement.requirements.any? do |op, version|
      case op
      when ">=", "~>", "=", ">" then version >= floor
      else false
      end
    end
  end

  ##
  # Returns the full path for installing +filename+ into +destination_dir+,
  # which must already be resolved with File.realpath by the caller.
  #
  # If +filename+ is not inside +destination_dir+ an exception is raised.

  def install_location(filename, destination_dir) # :nodoc:
    raise Gem::Package::PathError.new(filename, destination_dir) if
      filename.start_with? "/"

    destination = File.expand_path(filename, destination_dir)

    raise Gem::Package::PathError.new(destination, destination_dir) unless
      normalize_path(destination).start_with? normalize_path(destination_dir + "/")

    destination
  end

  ##
  # Raises an exception unless +dir+, with symlinks resolved, is
  # +destination_dir+ or a directory inside it.  +destination_dir+ must
  # already be resolved with File.realpath by the caller.

  def verify_extraction_dir(dir, destination_dir) # :nodoc:
    real_dir = File.realpath(dir)

    return if real_dir == destination_dir ||
              normalize_path(real_dir).start_with?(normalize_path(destination_dir + "/"))

    raise Gem::Package::PathError.new(real_dir, destination_dir)
  end

  ##
  # Verifies signatures with the security policy, if any.
  #
  # The policy always runs when one is configured, so policies that refuse
  # unsigned packages keep doing so. An unsigned package is recorded as
  # not_provided when the policy allows it through, since there was no
  # signature evidence to check.

  def verify_package_signatures # :nodoc:
    return unless @security_policy

    unsigned = @signatures.empty?

    @security_policy.verify_signatures @spec, @digests, @signatures

    if unsigned
      integrity_record :signature, "signatures", IntegrityCheck::NOT_PROVIDED,
                       "package has no signatures"
    else
      integrity_record :signature, "signatures", IntegrityCheck::VERIFIED
    end
  rescue Gem::Security::Exception => e
    integrity_record :signature, "signatures", IntegrityCheck::FAILED, e.message
    raise
  end

  ##
  # Verifies the +checksums+ against the +digests+.  This check is not
  # cryptographically secure.  Missing checksums are ignored.
  #
  # Each manifest entry is recorded as its own check, because a member may
  # be covered by several algorithms and they can disagree: one can match
  # while another does not. The subject names both, as
  # "data.tar.gz (SHA256)".

  def verify_checksums(digests, checksums) # :nodoc:
    return unless checksums

    checksums.sort.each do |algorithm, gem_digests|
      gem_digests.sort.each do |file_name, gem_hexdigest|
        computed_digest = digests[algorithm][file_name]

        if computed_digest.hexdigest == gem_hexdigest
          integrity_record :checksum, "#{file_name} (#{algorithm})", IntegrityCheck::VERIFIED
        else
          detail = "#{algorithm} checksum mismatch for #{file_name}"
          integrity_record :checksum, "#{file_name} (#{algorithm})", IntegrityCheck::FAILED, detail
          raise Gem::Package::FormatError.new detail, @gem
        end
      end
    end
  end

  ##
  # Records, for every member without checksum evidence, that the package
  # did not supply any. Gems built before checksums.yaml.gz existed have no
  # checksums for metadata.gz or data.tar.gz, which is different from having
  # checksums that do not match.

  def record_unchecked_members # :nodoc:
    return unless @integrity_recorder

    checked = @checksums ? @checksums.flat_map {|_algorithm, members| members.keys } : []

    @files.each do |file_name|
      next if file_name.end_with? ".sig"
      next if file_name == "checksums.yaml.gz"
      next if checked.include? file_name

      integrity_record :checksum, file_name, IntegrityCheck::NOT_PROVIDED,
                       "package has no checksum for this member"
    end
  end

  ##
  # Verifies +entry+ in a .gem file.
  #
  # Reading a member to its declared end and parsing its metadata are
  # recorded separately, so a report can say whether the bytes of a member
  # were actually read even when parsing the metadata that was read fails.

  def verify_entry(entry)
    file_name = entry.full_name
    @files << file_name

    if file_name.end_with? ".sig"
      read_signature_member entry, file_name
      return
    end

    read_member entry, file_name
    parse_metadata_member entry, file_name
  rescue StandardError
    warn "Exception while verifying #{@gem.path}"
    raise
  end

  ##
  # Reads a non-signature member to its declared end, so a truncated or
  # otherwise unreadable member is distinguished from a checksum mismatch.
  #
  # The report only claims the member was read when the read path actually
  # consumed it. #digest skips reading when the package offers no digest
  # algorithm to feed, so such a member is reported as not provided rather
  # than verified.

  def read_member(entry, file_name) # :nodoc:
    if digest_algorithms.empty?
      integrity_record :read, file_name, IntegrityCheck::NOT_PROVIDED,
                       "no digest algorithm to read this member with"
      return
    end

    digest entry

    integrity_record :read, file_name, IntegrityCheck::VERIFIED
  rescue StandardError => e
    integrity_record :read, file_name, IntegrityCheck::FAILED, e.message
    raise
  end

  ##
  # Reads a signature member. Signatures are only read when a security
  # policy will verify them, so without one the member is reported as not
  # provided rather than read.

  def read_signature_member(entry, file_name) # :nodoc:
    unless @security_policy
      integrity_record :read, file_name, IntegrityCheck::NOT_PROVIDED,
                       "no security policy to verify signatures"
      return
    end

    @signatures[file_name.delete_suffix(".sig")] = limit_read(entry, file_name, 1024 * 1024)

    integrity_record :read, file_name, IntegrityCheck::VERIFIED
  rescue StandardError => e
    integrity_record :read, file_name, IntegrityCheck::FAILED, e.message
    raise
  end

  ##
  # Parses the spec out of a metadata member, recording the malformed
  # metadata separately from a failure to read the member at all.

  def parse_metadata_member(entry, file_name) # :nodoc:
    load_spec_from_metadata entry

    return unless @spec && %w[metadata metadata.gz].include?(file_name)

    integrity_record :specification, "metadata", IntegrityCheck::VERIFIED
  rescue StandardError => e
    integrity_record :specification, "metadata", IntegrityCheck::FAILED, e.message
    raise
  end

  ##
  # Verifies the files of the +gem+

  def verify_files(gem)
    gem.each do |entry|
      verify_entry entry
    end

    unless @spec
      integrity_record :structure, "metadata", IntegrityCheck::FAILED, "package metadata is missing"
      raise Gem::Package::FormatError.new "package metadata is missing", @gem
    end

    unless @files.include? "data.tar.gz"
      integrity_record :structure, "data.tar.gz", IntegrityCheck::FAILED,
                       "package content (data.tar.gz) is missing"
      raise Gem::Package::FormatError.new \
        "package content (data.tar.gz) is missing", @gem
    end

    if (duplicates = @files.group_by {|f| f }.select {|_k,v| v.size > 1 }.map(&:first)) && duplicates.any?
      integrity_record :structure, "package", IntegrityCheck::FAILED,
                       "duplicate files in the package: (#{duplicates.map(&:inspect).join(", ")})"
      raise Gem::Security::Exception, "duplicate files in the package: (#{duplicates.map(&:inspect).join(", ")})"
    end

    integrity_record :structure, "package", IntegrityCheck::VERIFIED
  end

  if RUBY_ENGINE == "truffleruby"
    def copy_stream(src, dst, size) # :nodoc:
      dst.write src.read(size)
    end
  else
    def copy_stream(src, dst, size) # :nodoc:
      IO.copy_stream(src, dst, size)
    end
  end

  def limit_read(io, name, limit)
    bytes = io.read(limit + 1)
    raise Gem::Package::FormatError, "#{name} is too big (over #{limit} bytes)" if bytes.size > limit
    bytes
  end

  if Gem.win_platform?
    # Create a symlink and fallback to copy the file or directory on Windows,
    # where symlink creation needs special privileges in form of the Developer Mode.
    # JRuby on Windows raises TypeError from the wincode path-conversion helper
    # when it cannot create the symlink, so fall back to copy in that case too.
    def create_symlink(old_name, new_name)
      File.symlink(old_name, new_name)
    rescue Errno::EACCES, TypeError
      from = File.expand_path(old_name, File.dirname(new_name))
      FileUtils.cp_r(from, new_name)
    end
  else
    def create_symlink(old_name, new_name)
      File.symlink(old_name, new_name)
    end
  end
end

require_relative "package/digest_io"
require_relative "package/source"
require_relative "package/file_source"
require_relative "package/io_source"
require_relative "package/old"
require_relative "package/tar_header"
require_relative "package/tar_reader"
require_relative "package/tar_reader/entry"
require_relative "package/tar_writer"
