# frozen_string_literal: true
require 'logger'
require 'fileutils'
require 'tmpdir'
require 'zlib'

class TestArchiveCompressor < Test::Unit::TestCase
  CompressionError = Logger::LogDevice::CompressionError
  ArchiveCompressor = Logger::LogDevice::ArchiveCompressor

  def with_tmpdir
    dir = Dir.mktmpdir('logger-compress')
    yield dir
  ensure
    # Best effort: on Windows a just-released file handle can briefly reject
    # removal, and that must not turn a passing test into a teardown error.
    FileUtils.rm_rf(dir) rescue nil
  end

  def capture_stderr
    previous = $stderr
    stderr = +''
    class << stderr
      alias write concat
    end
    $stderr = stderr
    begin
      yield
    ensure
      $stderr = previous
    end
    stderr
  end

  def gunzip(path)
    Zlib::GzipReader.open(path, &:read)
  end

  def test_compress_creates_gzip_and_removes_source
    with_tmpdir do |dir|
      source = File.join(dir, 'log.0')
      File.binwrite(source, 'hello' * 100)
      compressor = ArchiveCompressor.new

      result = compressor.compress(source)

      assert_equal(:compressed, result.status)
      assert_equal(File.expand_path(source), result.source)
      assert_equal(File.expand_path(source) + '.gz', result.destination)
      assert_predicate(result, :compressed?)
      assert_not_predicate(result, :duplicated?)
      assert_file.not_exist?(source)
      assert_file.exist?(source + '.gz')
      assert_equal('hello' * 100, gunzip(source + '.gz'))
    end
  end

  def test_compress_round_trips_binary_content
    with_tmpdir do |dir|
      source = File.join(dir, 'log.0')
      content = (0..255).map(&:chr).join * 64
      File.binwrite(source, content)

      ArchiveCompressor.new.compress(source)

      assert_equal(content.b, gunzip(source + '.gz').b)
    end
  end

  def test_missing_source_raises_compression_error
    with_tmpdir do |dir|
      error = assert_raise(CompressionError) do
        ArchiveCompressor.new.compress(File.join(dir, 'absent.0'))
      end
      assert_match(/does not exist/, error.message)
    end
  end

  def test_existing_destination_is_not_overwritten
    with_tmpdir do |dir|
      source = File.join(dir, 'log.0')
      destination = source + '.gz'
      File.binwrite(source, 'payload')
      File.binwrite(destination, 'preexisting')

      assert_raise(CompressionError) do
        ArchiveCompressor.new.compress(source)
      end

      assert_equal('preexisting', File.binread(destination))
      assert_file.exist?(source)
    end
  end

  def test_truncated_gzip_fails_verification_and_preserves_source
    with_tmpdir do |dir|
      source = File.join(dir, 'log.0')
      File.binwrite(source, 'payload' * 100)
      bad = Class.new(ArchiveCompressor) do
        private
        def write_gzip(input, tmp_path, stat)
          File.binwrite(tmp_path, "not a gzip stream")
        end
      end.new

      error = assert_raise(CompressionError) { bad.compress(source) }
      assert_match(/verification failed/, error.message)

      # No correctly-named but truncated archive, and the source is intact.
      assert_file.not_exist?(source + '.gz')
      assert_file.exist?(source)
      assert_equal('payload' * 100, File.binread(source))
      assert_no_temp_artifacts(dir)
    end
  end

  def test_interrupted_compression_leaves_no_final_archive
    with_tmpdir do |dir|
      source = File.join(dir, 'log.0')
      File.binwrite(source, 'payload' * 100)
      interrupted = Class.new(ArchiveCompressor) do
        private
        def write_gzip(input, tmp_path, stat)
          super
          raise RuntimeError, 'simulated interruption'
        end
      end.new

      assert_raise(RuntimeError) { interrupted.compress(source) }

      assert_file.not_exist?(source + '.gz')
      assert_file.exist?(source)
      assert_no_temp_artifacts(dir)
    end
  end

  def test_replaced_source_is_never_deleted
    with_tmpdir do |dir|
      source = File.join(dir, 'log.0')
      File.binwrite(source, 'original body')
      replacement = 'brand new object at the same path'
      swapping = Class.new(ArchiveCompressor) do
        def initialize(replacement)
          super()
          @replacement = replacement
        end
        private
        define_method(:remove_source) do |path, stat|
          File.unlink(path)
          File.binwrite(path, @replacement)
          super(path, stat)
        end
      end.new(replacement)

      assert_raise(CompressionError) { swapping.compress(source) }

      assert_equal(replacement, File.binread(source))
      assert_file.exist?(source + '.gz')
    end
  end

  def test_concurrent_submissions_compress_only_once
    with_tmpdir do |dir|
      source = File.join(dir, 'log.0')
      File.binwrite(source, 'payload' * 50)
      lock = Mutex.new
      compressor = Class.new(ArchiveCompressor) do
        attr_reader :runs
        def initialize(lock)
          super()
          @lock = lock
          @runs = 0
        end
        private
        define_method(:write_gzip) do |input, tmp_path, stat|
          @lock.synchronize { @runs += 1 }
          sleep 0.2
          super(input, tmp_path, stat)
        end
      end.new(lock)

      results = 5.times.map { Thread.new { compressor.compress(source) } }.map { |t| t.join.value }
      statuses = results.map(&:status)

      assert_equal(1, statuses.count(:compressed))
      assert_equal(4, statuses.count(:duplicated))
      assert_equal(1, compressor.runs)
      refute(compressor.in_flight?(File.expand_path(source)))
      assert_file.not_exist?(source)
      assert_file.exist?(source + '.gz')
    end
  end

  def test_repeated_submission_after_completion
    with_tmpdir do |dir|
      source = File.join(dir, 'log.0')
      File.binwrite(source, 'payload')
      compressor = ArchiveCompressor.new

      first = compressor.compress(source)
      second = compressor.compress(source)

      assert_predicate(first, :compressed?)
      assert_predicate(second, :already_compressed?)
      assert_equal(File.expand_path(source) + '.gz', second.destination)
    end
  end

  def test_in_flight_query
    with_tmpdir do |dir|
      source = File.join(dir, 'log.0')
      File.binwrite(source, 'payload')
      started = Queue.new
      gate = Queue.new
      worker = Class.new(ArchiveCompressor) do
        def initialize(started, gate)
          super()
          @started = started
          @gate = gate
        end
        private
        define_method(:write_gzip) do |input, tmp_path, stat|
          @started << true
          @gate.pop
          super(input, tmp_path, stat)
        end
      end.new(started, gate)

      thread = Thread.new { worker.compress(source) }
      started.pop
      assert(worker.in_flight?(File.expand_path(source)))
      refute(ArchiveCompressor.new.in_flight?(File.expand_path(source)))
      gate << true
      thread.join
      refute(worker.in_flight?(File.expand_path(source)))
    end
  end

  def test_logdevice_queues_detached_archives_on_size_rotation
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      compressor = ArchiveCompressor.new
      device = Logger::LogDevice.new(log, shift_age: 1, shift_size: 0,
                                          skip_header: true, archive_compressor: compressor)
      device.write("one\n")
      assert_empty(device.archived_logs)
      device.write("two\n")

      assert_equal([File.expand_path(log) + '.0'], device.archived_logs)
      assert_file.exist?(log)
      assert_file.exist?(log + '.0')
      assert_file.not_exist?(log + '.0.gz')
    ensure
      device&.close
    end
  end

  def test_logdevice_compress_archived_logs
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      device = Logger::LogDevice.new(log, shift_age: 'now', shift_size: 1048576,
                                          skip_header: true,
                                          archive_compressor: ArchiveCompressor.new)
      device.write("one\n")
      device.write("two\n")
      queued = device.archived_logs
      assert_equal(2, queued.size)

      results = device.compress_archived_logs

      assert_equal(2, results.size)
      assert(results.all?(&:compressed?))
      assert_empty(device.archived_logs)
      queued.each do |path|
        assert_file.not_exist?(path)
        assert_file.exist?(path + '.gz')
      end
    ensure
      device&.close
    end
  end

  def test_archived_logs_returns_a_copy
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      device = Logger::LogDevice.new(log, shift_age: 1, shift_size: 0,
                                          skip_header: true,
                                          archive_compressor: ArchiveCompressor.new)
      device.write("one\n")
      device.write("two\n")
      snapshot = device.archived_logs
      snapshot << 'mutated'

      assert_not_include(device.archived_logs, 'mutated')
    ensure
      device&.close
    end
  end

  def test_rotation_without_compressor_keeps_plain_archives
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      device = Logger::LogDevice.new(log, shift_age: 1, shift_size: 0, skip_header: true)
      device.write("one\n")
      device.write("two\n")

      assert_empty(device.archived_logs)
      assert_nil(device.archive_compressor)
      assert_file.exist?(log + '.0')
      assert_file.not_exist?(log + '.0.gz')
    ensure
      device&.close
    end
  end

  def test_compress_archived_logs_without_compressor_raises
    with_tmpdir do |dir|
      device = Logger::LogDevice.new(File.join(dir, 't.log'), skip_header: true)
      assert_raise(ArgumentError) { device.compress_archived_logs }
      assert_raise(ArgumentError) { device.compress_archive(File.join(dir, 'x.0')) }
    ensure
      device&.close
    end
  end

  def test_compress_archive_refuses_the_active_file
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      device = Logger::LogDevice.new(log, skip_header: true,
                                          archive_compressor: ArchiveCompressor.new)
      error = assert_raise(ArgumentError) { device.compress_archive(log) }
      assert_match(/active log file/, error.message)
      assert_file.exist?(log)
      assert_file.not_exist?(log + '.gz')
    ensure
      device&.close
    end
  end

  def test_compress_archive_skips_missing_source
    with_tmpdir do |dir|
      device = Logger::LogDevice.new(File.join(dir, 't.log'), skip_header: true,
                                          archive_compressor: ArchiveCompressor.new)
      assert_nil(device.compress_archive(File.join(dir, 'gone.0')))
    ensure
      device&.close
    end
  end

  def test_compression_failure_is_observable_separately_from_write_failure
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      bad = Class.new(ArchiveCompressor) do
        private
        def write_gzip(input, tmp_path, stat)
          File.binwrite(tmp_path, 'broken')
        end
      end.new
      device = Logger::LogDevice.new(log, shift_age: 1, shift_size: 0,
                                          skip_header: true, archive_compressor: bad)
      device.write("one\n")
      device.write("two\n")

      compression_stderr = capture_stderr { device.compress_archived_logs }
      assert_match(/log archive compression failed/, compression_stderr)
      assert_no_match(/log writing failed/, compression_stderr)
      assert_file.exist?(log + '.0')

      raiser = Object.new
      def raiser.write(*)
        raise 'disk is full'
      end
      def raiser.close; end
      def raiser.stat
        Object.new
      end
      write_device = Logger::LogDevice.new(raiser)
      write_stderr = capture_stderr { write_device.write('x') }
      assert_match(/log writing failed/, write_stderr)
      assert_no_match(/compression/, write_stderr)
    ensure
      device&.close
      write_device&.close
    end
  end

  def test_reraise_compression_errors
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      bad = Class.new(ArchiveCompressor) do
        private
        def write_gzip(input, tmp_path, stat)
          File.binwrite(tmp_path, 'broken')
        end
      end.new
      device = Logger::LogDevice.new(log, shift_age: 1, shift_size: 0, skip_header: true,
                                          archive_compressor: bad,
                                          reraise_compression_errors: [CompressionError])
      device.write("one\n")
      device.write("two\n")
      assert_raise(CompressionError) { device.compress_archived_logs }
      assert_file.exist?(log + '.0')
    ensure
      device&.close
    end
  end

  def test_compression_starts_no_background_threads
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      device = Logger::LogDevice.new(log, shift_age: 1, shift_size: 0,
                                          skip_header: true,
                                          archive_compressor: ArchiveCompressor.new)
      before = Thread.list.size
      device.write("one\n")
      device.write("two\n")
      device.compress_archived_logs
      assert_equal(before, Thread.list.size)
    ensure
      device&.close
    end
  end

  def test_logging_continues_after_compression
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      device = Logger::LogDevice.new(log, shift_age: 1, shift_size: 0,
                                          skip_header: true,
                                          archive_compressor: ArchiveCompressor.new)
      device.write("first\n")
      device.write("second\n") # triggers rotation, queuing t.log.0
      device.compress_archived_logs
      assert_empty(device.archived_logs)

      # The active device is untouched and still writable; this write rotates
      # again and queues a fresh archive, independently of the earlier gzip.
      device.write("third\n")
      assert_file.exist?(log)
      assert_equal("third\n", File.read(log))
      assert_file.exist?(log + '.0.gz')
      assert_equal([File.expand_path(log) + '.0'], device.archived_logs)
      assert_equal("first\n", gunzip(log + '.0.gz').b.gsub("\r\n", "\n"))
    ensure
      device&.close
    end
  end

  def test_compress_archive_accepts_standalone_detached_file
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      external = File.join(dir, 'external.log.7')
      File.binwrite(external, 'detached body')
      device = Logger::LogDevice.new(log, skip_header: true,
                                          archive_compressor: ArchiveCompressor.new)

      result = device.compress_archive(external)

      assert_predicate(result, :compressed?)
      assert_file.not_exist?(external)
      assert_file.exist?(external + '.gz')
      assert_equal('detached body', gunzip(external + '.gz'))
      # A manually submitted archive never enters the rotation queue.
      assert_empty(device.archived_logs)
    ensure
      device&.close
    end
  end

  def test_reused_archive_path_never_overwrites_existing_gzip
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      device = Logger::LogDevice.new(log, shift_age: 1, shift_size: 0,
                                          skip_header: true,
                                          archive_compressor: ArchiveCompressor.new)
      device.write("a\n")
      device.write("b\n") # first rotation -> t.log.0
      device.compress_archived_logs
      assert_file.exist?(log + '.0.gz')

      device.write("c\n")
      device.write("d\n") # rotates again, reusing the t.log.0 name
      stderr = capture_stderr { device.compress_archived_logs }

      assert_match(/log archive compression failed/, stderr)
      assert_match(/destination already exists/, stderr)
      # The earlier archive is intact and the new source is preserved.
      assert_file.exist?(log + '.0.gz')
      assert_file.exist?(log + '.0')
      assert_equal("c\n", File.read(log + '.0').gsub("\r\n", "\n"))
      # Ordinary logging remains unaffected.
      device.write("e\n")
      assert_file.exist?(log)
    ensure
      device&.close
    end
  end

  def test_logger_delegates_archive_api
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      logger = Logger.new(log, 1, 0, skip_header: true,
                                archive_compressor: ArchiveCompressor.new)
      logger.info('one')
      logger.info('two')
      assert_equal(1, logger.archived_logs.size)

      results = logger.compress_archived_logs
      assert_equal(1, results.size)
      assert(results.first.compressed?)
      assert_empty(logger.archived_logs)
      logger.close
    end
  end

  def test_logger_reopen_attaches_compressor
    with_tmpdir do |dir|
      log = File.join(dir, 't.log')
      first = File.join(dir, 'first.log')
      logger = Logger.new(first, skip_header: true)
      # Reopen onto a rotating log and attach a compressor at the same time.
      logger.reopen(log, 1, 0, archive_compressor: ArchiveCompressor.new)
      logger.info('one')
      logger.info('two')
      assert_equal(1, logger.archived_logs.size)
      results = logger.compress_archived_logs
      assert(results.first.compressed?)
      logger.close
    end
  end

  private

  def assert_no_temp_artifacts(dir)
    temp_dirs = Dir.glob(File.join(dir, '.logger-compress-*'))
    temp_files = Dir.glob(File.join(dir, '**', '.logger-compress-*', '*'))
    assert_empty(temp_dirs + temp_files)
  end
end


