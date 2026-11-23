# frozen_string_literal: true

require "digest"
require "fileutils"
require "rubygems/package"

class Gem::CompactIndexClient
  # write cache files in a way that is robust to concurrent modifications
  # if digests are given, the checksums will be verified
  class CacheFile
    DEFAULT_FILE_MODE = 0o644
    private_constant :DEFAULT_FILE_MODE

    class Error < RuntimeError; end
    class ClosedError < Error; end

    class DigestMismatchError < Error
      def initialize(digests, expected_digests)
        super "Calculated checksums #{digests.inspect} did not match expected #{expected_digests.inspect}."
      end
    end

    # A signature of the file at +path+ used to detect concurrent
    # modifications between reading a baseline and committing an update.
    # Returns nil when the file does not exist.
    def self.baseline(path)
      stat = path.stat
      [stat.ino, stat.size, stat.mtime]
    rescue Errno::ENOENT
      nil
    end

    # Initialize with a copy of the original file, then yield the instance.
    def self.copy(path, &block)
      new(path) do |file|
        file.initialize_digests

        Gem::CompactIndexClient.filesystem_access(path, :read) do
          path.open("rb") do |s|
            file.open {|f| IO.copy_stream(s, f) }
          end
        end

        yield file
      end
    end

    # Write data to a temp file, then replace the original file with it verifying the digests if given.
    # When +baseline+ is given, the replacement only happens if the file on
    # disk still matches that baseline (see .baseline), and the result is
    # false without replacing anything otherwise. Returns whether the
    # original file was replaced.
    def self.write(path, data, digests = nil, baseline: :capture)
      return unless data
      committed = nil
      new(path) do |file|
        file.baseline = baseline unless baseline == :capture
        file.digests = digests
        committed = file.write(data)
      end
      committed
    end

    attr_reader :original_path, :path

    # The baseline signature the commit is checked against. Defaults to the
    # state of the original file when this CacheFile was created.
    attr_writer :baseline

    def initialize(original_path, &block)
      @original_path = original_path
      @perm = original_path.file? ? original_path.stat.mode : DEFAULT_FILE_MODE
      @baseline = self.class.baseline(original_path)
      @path = original_path.sub(/$/, ".#{$$}.tmp")
      return unless block_given?
      begin
        yield self
      ensure
        close
      end
    end

    def size
      path.size
    end

    # The last byte of the temp file as an Integer, or nil when it is empty.
    # Compared with the first byte of a ranged response to tell whether the
    # response continues the same generation of the file and can be resumed.
    def last_byte
      return unless path.file? && path.size.nonzero?
      Gem::CompactIndexClient.filesystem_access(path, :read) do
        path.open("rb") {|f| f.seek(-1, IO::SEEK_END); f.getbyte }
      end
    end

    # initialize the digests using CompactIndexClient::SUPPORTED_DIGESTS, or a subset based on keys.
    def initialize_digests(keys = nil)
      @digests = keys ? SUPPORTED_DIGESTS.slice(*keys) : SUPPORTED_DIGESTS.dup
      @digests.transform_values! {|algo_class| Digest(algo_class).new }
    end

    # reset the digests so they don't contain any previously read data
    def reset_digests
      @digests&.each_value(&:reset)
    end

    # set the digests that will be verified at the end
    def digests=(expected_digests)
      @expected_digests = expected_digests

      if @expected_digests.nil?
        @digests = nil
      elsif @digests
        @digests = @digests.slice(*@expected_digests.keys)
      else
        initialize_digests(@expected_digests.keys)
      end
    end

    def digests?
      @digests&.any?
    end

    # Open the temp file for writing, reusing original permissions, yielding the IO object.
    def open(write_mode = "wb", perm = @perm, &block)
      raise ClosedError, "Cannot reopen closed file" if @closed
      Gem::CompactIndexClient.filesystem_access(path, :write) do
        path.open(write_mode, perm) do |f|
          yield digests? ? Gem::Package::DigestIO.new(f, @digests) : f
        end
      end
    end

    # Returns false without appending when no digests since appending is too error prone to do without digests.
    def append(data)
      return false unless digests?
      open("ab") {|f| f.write data }
      verify && commit
    end

    def write(data)
      reset_digests
      open {|f| f.write data }
      commit!
    end

    def commit!
      verify || raise(DigestMismatchError.new(@base64digests, @expected_digests))
      commit
    end

    # Verify the digests, returning true on match, false on mismatch.
    def verify
      return true unless @expected_digests && digests?
      @base64digests = @digests.transform_values!(&:base64digest)
      @digests = nil
      @base64digests.all? {|algo, digest| @expected_digests[algo] == digest }
    end

    # Replace the original file with the temp file without verifying digests.
    # The file is permanently closed. Returns false without replacing
    # anything when the original file no longer matches the baseline,
    # meaning another process updated the cache concurrently and its
    # confirmed contents must win.
    def commit
      raise ClosedError, "Cannot commit closed file" if @closed
      return false if stale?
      Gem::CompactIndexClient.filesystem_access(original_path, :write) do
        FileUtils.mv(path, original_path)
      end
      @closed = true
      true
    end

    # Remove the temp file without replacing the original file.
    # The file is permanently closed.
    def close
      return if @closed
      FileUtils.remove_file(path) if @path&.file?
      @closed = true
    end

    private

    # Whether the original file was changed since the baseline was captured.
    def stale?
      self.class.baseline(original_path) != @baseline
    end
  end
end
