# frozen_string_literal: true

require 'fileutils'
require 'monitor'
require 'tmpdir'
require 'zlib'

class Logger
  class LogDevice
    # Raised when an archived log file cannot be compressed safely.
    # It is never raised for ordinary log writing failures, so the two
    # kinds of failure remain independently observable.
    class CompressionError < StandardError
    end

    # Synchronously compresses archived (already rotated, no longer open for
    # writing) log files into gzip archives.
    #
    # This class never starts a thread on its own: callers decide when (and on
    # which thread or scheduler) compression happens. All methods are safe to
    # call concurrently.
    #
    # Safety guarantees:
    #
    # - The gzip output is written to a temporary file in the destination
    #   directory and fully verified (decompressed and compared against the
    #   source) before it is atomically renamed to its final name. An
    #   interrupted or failed compression therefore leaves no file with the
    #   final name and a truncated body; the original source is kept untouched.
    # - The source file is removed only after the gzip archive has been
    #   published, and only when the file at the source path still has the same
    #   device and inode as the file that was compressed. If the source path was
    #   replaced in the meantime, the new object is never deleted.
    # - Submitting the same archive more than once never compresses it twice
    #   concurrently; repeated submissions are reported without extra work.
    #
    # An existing destination file is never overwritten: when the +.gz+ path
    # already exists, CompressionError is raised instead, so a previously
    # published archive cannot be silently replaced and the new source stays in
    # place for the caller to resolve.
    class ArchiveCompressor
      include MonitorMixin

      # Outcome of a #compress call.
      #
      # +status+ is one of:
      #
      # - +:compressed+         - a new gzip archive was created and published.
      # - +:duplicated+         - the source was already being compressed by
      #                           another in-flight call.
      # - +:already_compressed+ - the source was gone and its gzip archive was
      #                           already present, or a prior call had finished.
      Result = Struct.new(:status, :source, :destination, keyword_init: true) do
        def compressed?
          status == :compressed
        end

        def duplicated?
          status == :duplicated
        end

        def already_compressed?
          status == :already_compressed
        end
      end

      # The filename suffix appended to compressed archives.
      GZIP_SUFFIX = '.gz'

      def initialize
        @in_flight = {}
        mon_initialize
      end

      # Returns true if +path+ is currently being compressed.
      def in_flight?(path)
        synchronize { @in_flight.key?(File.expand_path(path)) }
      end

      # Compresses +source+ into +source+ with a +.gz+ suffix.
      #
      # The call runs synchronously on the caller's thread and returns a
      # Result describing what happened.
      #
      # Raises CompressionError if the source cannot be read, the archive
      # cannot be written or verified, or the destination cannot be published.
      # In every failure case the source file is preserved and no file with
      # the destination name is left behind.
      def compress(source)
        source = File.expand_path(source)
        destination = destination_for(source)

        ticket = acquire(source, destination)
        return ticket unless ticket.nil?

        begin
          compress_file(source, destination)
          Result.new(status: :compressed, source: source, destination: destination)
        ensure
          release(source)
        end
      end

      private

      # :stopdoc:

      def destination_for(source)
        source + GZIP_SUFFIX
      end

      def acquire(source, destination)
        synchronize do
          if @in_flight.key?(source)
            return Result.new(status: :duplicated, source: source, destination: destination)
          end
          unless File.exist?(source)
            if File.exist?(destination)
              return Result.new(status: :already_compressed,
                                source: source, destination: destination)
            end
            raise CompressionError, "archive to compress does not exist: #{source}"
          end
          @in_flight[source] = destination
          nil
        end
      end

      def release(source)
        synchronize { @in_flight.delete(source) }
      end

      def compress_file(source, destination)
        stat = File.stat(source)
        tmpdir = Dir.mktmpdir('.logger-compress-', File.dirname(destination))
        tmp_path = File.join(tmpdir, File.basename(destination))
        File.open(source, 'rb') do |input|
          write_gzip(input, tmp_path, stat)
          verify_gzip(tmp_path, stat.size)
          publish(tmp_path, destination)
        end
        # The source must be closed first: Windows refuses to remove a file
        # that is still open. Re-checking identity at this last moment also
        # guarantees a replaced source path is never deleted.
        remove_source(source, stat)
      rescue CompressionError
        raise
      rescue SystemCallError, IOError, Zlib::Error => e
        raise CompressionError, "failed to compress #{source}: #{e.message}"
      ensure
        # rm_rf tolerates the directory (or its contents) already being gone,
        # which matters because #publish moves the verified file out of it.
        # Cleanup must never mask the real outcome of the compression, so any
        # leftover removal problem is swallowed here.
        begin
          FileUtils.rm_rf(tmpdir) if tmpdir
        rescue StandardError
          nil
        end
      end

      def write_gzip(input, tmp_path, stat)
        File.open(tmp_path, 'wb') do |file|
          Zlib::GzipWriter.wrap(file) do |gz|
            gz.mtime = stat.mtime
            gz.orig_name = File.basename(input.path)
            while (chunk = input.read(65536))
              gz.write(chunk)
            end
          end
        end
      end

      # Reads the whole gzip stream back so that a truncated stream, a bad CRC,
      # or a length mismatch is detected before the archive is published.
      def verify_gzip(tmp_path, expected_size)
        actual_size = 0
        Zlib::GzipReader.open(tmp_path) do |gz|
          gz.each do |chunk|
            actual_size += chunk.bytesize
            raise CompressionError, 'compressed archive exceeds source size' if
              actual_size > expected_size
          end
        end
        unless actual_size == expected_size
          raise CompressionError,
                "compressed size mismatch: #{actual_size} bytes instead of #{expected_size}"
        end
      rescue Zlib::Error => e
        raise CompressionError, "compressed archive verification failed: #{e.message}"
      end

      # Publishes the verified archive under its final name. The destination
      # must not already exist, otherwise an externally created object could be
      # silently overwritten.
      def publish(tmp_path, destination)
        if File.exist?(destination)
          raise CompressionError, "compression destination already exists: #{destination}"
        end
        File.rename(tmp_path, destination)
      end

      # Removes the source only when the path still points at the exact file
      # that was compressed. If rotation (or anything else) replaced it, the
      # new object is left alone.
      def remove_source(source, stat)
        current = File.stat(source)
        unless current.dev == stat.dev && current.ino == stat.ino
          raise CompressionError,
                "refusing to remove #{source}: it was replaced while being compressed"
        end
        File.unlink(source)
      end
    end
  end
end
