# frozen_string_literal: true

require "digest"

class PStore
  # A write-ahead journal used by recoverable stores.
  #
  # A journal belongs to exactly one store (the one whose path it derives from);
  # it never coordinates changes across stores or with any external service.
  #
  # On-disk layout (all integers big-endian, all strings binary):
  #
  #   header   ::= MAGIC (7 bytes) + VERSION (1 byte) +
  #                payload_size (8 bytes, unsigned 64-bit)
  #   payload  ::= payload_size bytes of marshalled table image
  #   trailer  ::= digest of the header followed by the payload
  #                (DIGEST_LENGTH bytes)
  #
  # The trailer is the commit marker: a journal is only considered committed
  # once the full trailer, with a valid digest, is on disk.  Any shorter file
  # is an uncommitted tail left by an interrupted process and must be
  # discarded, never interpreted as a table.
  class Journal
    # :stopdoc:
    MAGIC = "PStJrnl"
    VERSION = 1
    DIGEST = %w[SHA512 SHA384 SHA256 SHA1 RMD160 MD5].each do |algo|
      begin
        break Digest(algo)
      rescue LoadError
      end
    end
    DIGEST_LENGTH = DIGEST.new.digest_length
    HEADER_SIZE = MAGIC.bytesize + 1 + 8
    ACCESS = IO::WRONLY | IO::CREAT | IO::TRUNC | IO::BINARY

    private_constant :MAGIC, :VERSION, :DIGEST, :DIGEST_LENGTH, :HEADER_SIZE, :ACCESS
    # :startdoc:

    # Path of the journal file backing +store_path+.
    def self.path_for(store_path)
      "#{store_path}.journal"
    end

    def initialize(store_path)
      @path = self.class.path_for(store_path)
    end

    # The path of this journal file.
    attr_reader :path

    # Digest (binary) of +payload+, shared with the recovery code.
    def self.digest(payload)
      DIGEST.digest(payload)
    end

    # Rewrite the whole journal from scratch with +payload+ and publish the
    # commit marker.  On any I/O failure the exception propagates and whatever
    # was written stays on disk as evidence; the store file itself is never
    # touched here.
    def write!(payload)
      header = MAGIC.dup << VERSION.chr << [payload.bytesize].pack("Q>")
      marker = DIGEST.digest(header + payload)
      File.open(@path, mode: ACCESS, encoding: Encoding::ASCII_8BIT) do |file|
        file.write(header)
        file.write(payload)
        file.write(marker)
        file.flush
        file.fsync
      end
      fsync_parent_directory
      nil
    end

    # Classify the journal without touching the store.
    #
    # Returns one of:
    #
    # - +nil+: no journal exists.
    # - +[:uncommitted]+: only an uncommitted tail is present; it must not be
    #   treated as valid new table data.
    # - +[:committed, payload]+: a complete, verified transaction record with
    #   its commit marker.
    # - +[:corrupt, reason]+: a record of committed length whose magic,
    #   version, size or digest check failed.
    def examine
      return nil unless File.exist?(@path)

      bytes = File.binread(@path)
      if bytes.bytesize < HEADER_SIZE
        return [:uncommitted]
      end

      magic, version, payload_size = bytes.unpack("a7 C Q>")
      if magic != MAGIC || version != VERSION ||
         payload_size > bytes.bytesize - HEADER_SIZE - DIGEST_LENGTH
        return [:uncommitted]
      end

      expected_size = HEADER_SIZE + payload_size + DIGEST_LENGTH
      if bytes.bytesize != expected_size
        # A record that reached (or passed) a committed length but is not the
        # exact, fully verified record: the committed frame itself is damaged.
        return [:corrupt, "malformed journal record"]
      end

      header = bytes.byteslice(0, HEADER_SIZE)
      payload = bytes.byteslice(HEADER_SIZE, payload_size)
      marker = bytes.byteslice(HEADER_SIZE + payload_size, DIGEST_LENGTH)
      unless marker.bytesize == DIGEST_LENGTH &&
             DIGEST.digest(header + payload) == marker
        return [:corrupt, "journal checksum mismatch"]
      end

      [:committed, payload]
    end

    # Remove the journal.  A missing journal is success.  Any other failure is
    # a terminal cleanup error that leaves the journal in place as evidence.
    def delete!
      File.unlink(@path)
      fsync_parent_directory
      nil
    rescue Errno::ENOENT
      nil
    rescue SystemCallError => error
      raise PStore::JournalCleanupError.new(
        "failed to remove journal #{@path}: #{error.message}", @path
      )
    end

    # Move a corrupt journal aside so the original bytes are preserved as
    # evidence.  Returns the path of the quarantined copy.  Failure is terminal
    # and leaves the original journal untouched.
    def quarantine!
      target = "#{@path}.corrupt.#{Time.now.to_i}.#{Process.pid}"
      suffix = 1
      while File.exist?(target)
        target = "#{@path}.corrupt.#{Time.now.to_i}.#{Process.pid}.#{suffix}"
        suffix += 1
      end
      File.rename(@path, target)
      fsync_parent_directory
      target
    rescue SystemCallError => error
      raise PStore::JournalCleanupError.new(
        "failed to quarantine corrupt journal #{@path}: #{error.message}", @path
      )
    end

    private

    # Best effort: make the journal's own create/unlink/rename durable too.
    # Platforms or filesystems without directory fsync simply skip this.
    def fsync_parent_directory
      dir = File.dirname(@path)
      File.open(dir, IO::RDONLY | IO::BINARY) do |directory|
        directory.fsync
      end
    rescue SystemCallError, NotImplementedError
      nil
    end
  end
end