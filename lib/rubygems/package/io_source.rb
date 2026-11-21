# frozen_string_literal: true

##
# Supports reading and writing gems from/to a generic IO object.  This is
# useful for other applications built on top of rubygems, such as
# rubygems.org.
#
# Inputs that can seek are read in place, rewound after each stage.  Inputs
# that can only be read forwards (sockets, pipes, HTTP bodies) are copied
# exactly once into a bounded spool so that the metadata stage and the data
# stage both see the same bytes.  See Gem::Package::Source for the two-stage
# contract.
#
# This is a private class, do not depend on it directly. Instead, pass an IO
# object to `Gem::Package.new`.

class Gem::Package::IOSource < Gem::Package::Source # :nodoc: all
  ##
  # Number of bytes copied from a forward-only input at a time.

  SPOOL_CHUNK_SIZE = 64 * 1024

  ##
  # Upper bound on how many bytes are copied from a forward-only input into
  # its spool.  The spool lives in a temporary file, so this bounds the disk
  # usage an untrusted input can trigger.

  MAX_SPOOL_SIZE = 1024 * 1024 * 1024

  attr_reader :io

  def initialize(io)
    @io = io
    @start = nil
    @spool = nil
    @failure = nil
    @seekable = nil
  end

  def start
    @start ||= begin
      if seekable?
        if io.pos > 0
          raise Gem::Package::Error, "Cannot read start unless IO is at start"
        end

        value = io.read 20
        io.rewind
        value
      else
        spool_file {|file| file.read 20 }
      end
    rescue Gem::Package::FormatError => e
      # The package cannot be sniffed, but construction should not hide the
      # failure: record it and let the stage reads report it.
      @failure ||= e
      nil
    end
  end

  def present?
    true
  end

  ##
  # Whether the underlying IO can be rewound without consuming it.  Pipes and
  # other forward-only inputs answer false.

  def seekable?
    return @seekable unless @seekable.nil?

    @seekable =
      if io.respond_to?(:pos) && (io.respond_to?(:seek) || io.respond_to?(:rewind))
        begin
          io.pos
          true
        rescue SystemCallError, IOError, NotImplementedError
          false
        end
      else
        false
      end
  end

  ##
  # Metadata stage: an IO positioned at the start of the package, from which
  # only the specification is read.  Shares its bytes with #with_read_io.

  def with_metadata_io
    with_read_io {|read_io| yield read_io }
  end

  def with_read_io
    if seekable?
      begin
        yield io
      ensure
        io.rewind
      end
    else
      spool_file {|file| yield file }
    end
  end

  def with_write_io
    raise Gem::Package::NonSeekableIO, "Cannot write to a forward-only IO" unless seekable?

    begin
      yield io
    ensure
      io.rewind
    end
  end

  def path
  end

  ##
  # Drops any spooled bytes and their temporary file.  A forward-only input
  # has already been consumed, so later stages report the recorded failure
  # instead of silently reading a partial package.
  #
  # This is a no-op for a seekable input, whose bytes are still on the
  # original IO.

  def release(failure = nil)
    return nil if seekable?

    @failure ||= failure
    discard_spool
    nil
  end

  ##
  # The error that ended this source's usable life, if any.

  def failure
    @failure
  end

  private

  ##
  # The temporary spool holding the single copy of a forward-only input.
  #
  # A failure to create or fill the spool is reported through the package
  # error contract, so callers see a Gem::Package::FormatError and the next
  # package can be read normally.

  def spool
    raise @failure if @failure

    @spool ||= begin
      build_spool
    rescue Gem::Package::Error
      raise
    rescue StandardError => e
      raise spool_error e
    end
  end

  ##
  # Copies the whole input into a temporary file.  Overridable so callers and
  # tests can make spooling fail.

  def build_spool
    require "tempfile"

    tempfile = Tempfile.new(["gem-package", ".gem"], binmode: true)

    begin
      copy_to_spool tempfile
      tempfile.flush
      # Keep the path, release the write handle.  Each stage opens its own
      # read handle over the same bytes.
      tempfile.close
    rescue Exception # rubocop:disable Lint/RescueException
      close_spool tempfile
      raise
    end

    tempfile
  end

  ##
  # Reports a failed spool through the package error contract, so callers see
  # a Gem::Package::FormatError rather than a raw IO error.  Nothing is left
  # behind and the failure is local to this source.
  #
  # The failure is not attributable to the metadata or the body, so +stage+ is
  # left nil.

  def spool_error(cause)
    Gem::Package::FormatError.new \
      "could not spool package: #{cause.message}",
      nil,
      detail: cause.class.name
  end

  ##
  # Copies +io+ into +spool+ in bounded chunks, refusing inputs larger than
  # MAX_SPOOL_SIZE.

  def copy_to_spool(spool)
    copied = 0

    while (chunk = io.read(SPOOL_CHUNK_SIZE))
      break if chunk.empty?

      copied += chunk.bytesize
      if copied > MAX_SPOOL_SIZE
        raise Gem::Package::FormatError,
          "package is too large to spool from a forward-only IO"
      end

      spool.write chunk
    end

    copied
  end

  ##
  # Yields a fresh read-only IO over the spooled bytes, positioned at the
  # start of the package.  Only failures to open the spool are translated;
  # errors raised by the block belong to the caller.

  def spool_file
    file =
      begin
        File.open spool.path, "rb"
      rescue StandardError => e
        raise spool_error e
      end

    begin
      yield file
    ensure
      file.close
    end
  end

  ##
  # Drops the spooled bytes and its temporary file.

  def discard_spool
    return unless @spool

    tempfile = @spool
    @spool = nil
    close_spool tempfile
  end

  def close_spool(tempfile)
    tempfile.close!
  rescue StandardError
    tempfile.close unless tempfile.closed?
  end
end
