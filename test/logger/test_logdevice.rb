# coding: US-ASCII
# frozen_string_literal: false
require 'logger'
require 'tempfile'
require 'tmpdir'
require 'pathname'
require 'stringio'

class TestLogDevice < Test::Unit::TestCase
  # Runs the block with one of the file operations used by rotation replaced by
  # a failure, without touching anything else.  +logdev+ is the device to
  # affect, +method_name+ the private LogDevice method to make fail and +times+
  # how many calls fail (default: the first one).
  #
  # The fault is installed on the single device, so no other test and no other
  # log device is affected, and it disappears with the device.
  def with_io_failure(logdev, method_name, error = Errno::EIO.new("injected failure"), times: 1)
    original = logdev.method(method_name)
    calls = 0
    logdev.define_singleton_method(method_name) do |*args, **kwargs|
      if (calls += 1) <= times
        raise error
      end
      original.call(*args, **kwargs)
    end
    yield
    logdev.singleton_class.send(:remove_method, method_name)
  end

  # Captures what the log device writes to $stderr while the block runs.
  def capture_shift_warnings
    original = $stderr
    $stderr = StringIO.new
    yield
    $stderr.string
  ensure
    $stderr = original
  end

  class LogExcnRaiser
    def write(*arg)
      raise 'disk is full'
    end

    def close
    end

    def stat
      Object.new
    end
  end

  def setup
    @top_dir = File.expand_path('../../lib', __dir__)
    @tempfile = Tempfile.new("logger")
    @tempfile.close
    @filename = @tempfile.path
    File.unlink(@filename)
  end

  def teardown
    @tempfile.close(true)
  end

  def d(log, opt = {})
    Logger::LogDevice.new(log, **opt)
  end

  def test_initialize
    logdev = d(STDERR)
    assert_equal(STDERR, logdev.dev)
    assert_nil(logdev.filename)
    assert_raise(TypeError) do
      d(nil)
    end
    #
    logdev = d(@filename)
    begin
      assert_file.exist?(@filename)
      assert_predicate(logdev.dev, :sync)
      refute_predicate(logdev.dev, :binmode?)
      assert_equal(@filename, logdev.filename)
      logdev.write('hello')
    ensure
      logdev.close
    end
    # create logfile which is already exist.
    logdev = d(@filename)
    begin
      assert_predicate(logdev.dev, :sync)
      refute_predicate(logdev.dev, :binmode?)
      logdev.write('world')
      logfile = File.read(@filename)
      assert_equal(2, logfile.split(/\n/).size)
      assert_match(/^helloworld$/, logfile)
    ensure
      logdev.close
    end
    # logfile object with path
    tempfile = Tempfile.new("logger")
    tempfile.sync = true
    logdev = d(tempfile)
    begin
      logdev.write('world')
      logfile = File.read(tempfile.path)
      assert_equal(1, logfile.split(/\n/).size)
      assert_match(/^world$/, logfile)
      assert_equal(tempfile.path, logdev.filename)
    ensure
      logdev.close
      File.unlink(tempfile)
      tempfile.close(true)
    end
    # logfile object with Pathname object
    tempfile = Tempfile.new("logger")
    pathname = Pathname.new(tempfile.path)
    logdev = d(pathname)
    begin
      logdev.write('world')
      logfile = File.read(pathname)
      assert_equal(1, logfile.split(/\n/).size)
      assert_match(/^world$/, logfile)
      assert_equal(pathname, logdev.filename)
    ensure
      logdev.close
      tempfile.close(true)
    end
  end

  def test_write
    r, w = IO.pipe
    logdev = d(w)
    logdev.write("msg2\n\n")
    IO.select([r], nil, nil, 0.1)
    w.close
    msg = r.read
    r.close
    assert_equal("msg2\n\n", msg)
    #
    logdev = d(LogExcnRaiser.new)
    class << (stderr = '')
      alias write concat
    end
    $stderr, stderr = stderr, $stderr
    begin
      assert_nothing_raised do
        logdev.write('hello')
      end
    ensure
      logdev.close
      $stderr, stderr = stderr, $stderr
    end
    assert_equal "log writing failed. disk is full\n", stderr
  end

  def test_close
    r, w = IO.pipe
    logdev = d(w)
    logdev.write("msg2\n\n")
    IO.select([r], nil, nil, 0.1)
    assert_not_predicate(w, :closed?)
    logdev.close
    assert_predicate(w, :closed?)
    r.close
  end

  def test_reopen_io
    logdev  = d(STDERR)
    old_dev = logdev.dev
    logdev.reopen
    assert_equal(STDERR, logdev.dev)
    assert_not_predicate(old_dev, :closed?)
  end

  def test_reopen_io_by_io
    logdev  = d(STDERR)
    old_dev = logdev.dev
    logdev.reopen(STDOUT)
    assert_equal(STDOUT, logdev.dev)
    assert_not_predicate(old_dev, :closed?)
  end

  def test_reopen_io_by_file
    logdev  = d(STDERR)
    old_dev = logdev.dev
    logdev.reopen(@filename)
    begin
      assert_file.exist?(@filename)
      assert_equal(@filename, logdev.filename)
      assert_not_predicate(old_dev, :closed?)
    ensure
      logdev.close
    end
  end

  def test_reopen_file
    logdev = d(@filename)
    old_dev = logdev.dev

    logdev.reopen
    begin
      assert_file.exist?(@filename)
      assert_equal(@filename, logdev.filename)
      assert_predicate(old_dev, :closed?)
    ensure
      logdev.close
    end
  end

  def test_reopen_file_by_io
    logdev = d(@filename)
    old_dev = logdev.dev
    logdev.reopen(STDOUT)
    assert_equal(STDOUT, logdev.dev)
    assert_nil(logdev.filename)
    assert_predicate(old_dev, :closed?)
  end

  def test_reopen_file_by_file
    logdev = d(@filename)
    old_dev = logdev.dev

    tempfile2 = Tempfile.new("logger")
    tempfile2.close
    filename2 = tempfile2.path
    File.unlink(filename2)

    logdev.reopen(filename2)
    begin
      assert_file.exist?(filename2)
      assert_equal(filename2, logdev.filename)
      assert_predicate(old_dev, :closed?)
    ensure
      logdev.close
      tempfile2.close(true)
    end
  end

  def test_perm_after_shift
    mode = 0o611
    File.open(@filename, "w") {|f| f.chmod mode}
    logfile = @filename
    logfile0 = logfile + '.0'
    logdev = d(@filename, shift_age: 1, shift_size: 0)
    logdev.write('hello')
    logdev.write('hello')
    logdev.close

    assert_equal File.stat(logfile0).mode, File.stat(logfile).mode
  ensure
    if logfile0
      File.unlink(logfile0) rescue nil
    end
  end

  def test_shifting_size_with_reopen
    tmpfile = Tempfile.new([File.basename(__FILE__, '.*'), '_1.log'])
    logfile = tmpfile.path
    logfile0 = logfile + '.0'
    logfile1 = logfile + '.1'
    logfile2 = logfile + '.2'
    logfile3 = logfile + '.3'
    tmpfile.close(true)
    File.unlink(logfile) if File.exist?(logfile)
    File.unlink(logfile0) if File.exist?(logfile0)
    File.unlink(logfile1) if File.exist?(logfile1)
    File.unlink(logfile2) if File.exist?(logfile2)

    logger = Logger.new(STDERR)
    logger.reopen(logfile, 4, 100)

    logger.error("0" * 15)
    assert_file.exist?(logfile)
    assert_file.not_exist?(logfile0)
    logger.error("0" * 15)
    assert_file.exist?(logfile0)
    assert_file.not_exist?(logfile1)
    logger.error("0" * 15)
    assert_file.exist?(logfile1)
    assert_file.not_exist?(logfile2)
    logger.error("0" * 15)
    assert_file.exist?(logfile2)
    assert_file.not_exist?(logfile3)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile3)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile3)
    logger.close
    File.unlink(logfile)
    File.unlink(logfile0)
    File.unlink(logfile1)
    File.unlink(logfile2)

    tmpfile = Tempfile.new([File.basename(__FILE__, '.*'), '_2.log'])
    logfile = tmpfile.path
    logfile0 = logfile + '.0'
    logfile1 = logfile + '.1'
    logfile2 = logfile + '.2'
    logfile3 = logfile + '.3'
    tmpfile.close(true)
    logger = Logger.new(logfile, 4, 150)
    logger.error("0" * 15)
    assert_file.exist?(logfile)
    assert_file.not_exist?(logfile0)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile0)
    logger.error("0" * 15)
    assert_file.exist?(logfile0)
    assert_file.not_exist?(logfile1)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile1)
    logger.error("0" * 15)
    assert_file.exist?(logfile1)
    assert_file.not_exist?(logfile2)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile2)
    logger.error("0" * 15)
    assert_file.exist?(logfile2)
    assert_file.not_exist?(logfile3)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile3)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile3)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile3)
    logger.close
    File.unlink(logfile)
    File.unlink(logfile0)
    File.unlink(logfile1)
    File.unlink(logfile2)
  end

  def test_shifting_size
    tmpfile = Tempfile.new([File.basename(__FILE__, '.*'), '_1.log'])
    logfile = tmpfile.path
    logfile0 = logfile + '.0'
    logfile1 = logfile + '.1'
    logfile2 = logfile + '.2'
    logfile3 = logfile + '.3'
    tmpfile.close(true)
    File.unlink(logfile) if File.exist?(logfile)
    File.unlink(logfile0) if File.exist?(logfile0)
    File.unlink(logfile1) if File.exist?(logfile1)
    File.unlink(logfile2) if File.exist?(logfile2)
    logger = Logger.new(logfile, 4, 100)
    logger.error("0" * 15)
    assert_file.exist?(logfile)
    assert_file.not_exist?(logfile0)
    logger.error("0" * 15)
    assert_file.exist?(logfile0)
    assert_file.not_exist?(logfile1)
    logger.error("0" * 15)
    assert_file.exist?(logfile1)
    assert_file.not_exist?(logfile2)
    logger.error("0" * 15)
    assert_file.exist?(logfile2)
    assert_file.not_exist?(logfile3)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile3)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile3)
    logger.close
    File.unlink(logfile)
    File.unlink(logfile0)
    File.unlink(logfile1)
    File.unlink(logfile2)

    tmpfile = Tempfile.new([File.basename(__FILE__, '.*'), '_2.log'])
    logfile = tmpfile.path
    logfile0 = logfile + '.0'
    logfile1 = logfile + '.1'
    logfile2 = logfile + '.2'
    logfile3 = logfile + '.3'
    tmpfile.close(true)
    logger = Logger.new(logfile, 4, 150)
    logger.error("0" * 15)
    assert_file.exist?(logfile)
    assert_file.not_exist?(logfile0)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile0)
    logger.error("0" * 15)
    assert_file.exist?(logfile0)
    assert_file.not_exist?(logfile1)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile1)
    logger.error("0" * 15)
    assert_file.exist?(logfile1)
    assert_file.not_exist?(logfile2)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile2)
    logger.error("0" * 15)
    assert_file.exist?(logfile2)
    assert_file.not_exist?(logfile3)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile3)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile3)
    logger.error("0" * 15)
    assert_file.not_exist?(logfile3)
    logger.close
    File.unlink(logfile)
    File.unlink(logfile0)
    File.unlink(logfile1)
    File.unlink(logfile2)
  end

  def test_shifting_age_variants
    logger = Logger.new(@filename, 'daily')
    logger.info('daily')
    logger.close
    logger = Logger.new(@filename, 'weekly')
    logger.info('weekly')
    logger.close
    logger = Logger.new(@filename, 'monthly')
    logger.info('monthly')
    logger.close
  end

  def test_invalid_shifting_age
    assert_raise(ArgumentError) { Logger::Period.next_rotate_time(Time.now, 'invalid') }
    assert_raise(ArgumentError) { Logger::Period.previous_period_end(Time.now, 'invalid') }
  end

  def test_shifting_age
    yyyymmdd = Time.now.strftime("%Y%m%d")
    filename1 = @filename + ".#{yyyymmdd}"
    filename2 = @filename + ".#{yyyymmdd}.1"
    filename3 = @filename + ".#{yyyymmdd}.2"
    begin
      logger = Logger.new(@filename, 'now')
      assert_file.exist?(@filename)
      assert_file.not_exist?(filename1)
      assert_file.not_exist?(filename2)
      assert_file.not_exist?(filename3)
      logger.info("0" * 15)
      assert_file.exist?(@filename)
      assert_file.exist?(filename1)
      assert_file.not_exist?(filename2)
      assert_file.not_exist?(filename3)
      logger.warn("0" * 15)
      assert_file.exist?(@filename)
      assert_file.exist?(filename1)
      assert_file.exist?(filename2)
      assert_file.not_exist?(filename3)
      logger.error("0" * 15)
      assert_file.exist?(@filename)
      assert_file.exist?(filename1)
      assert_file.exist?(filename2)
      assert_file.exist?(filename3)
    ensure
      logger.close if logger
      [filename1, filename2, filename3].each do |filename|
        File.unlink(filename) if File.exist?(filename)
      end
    end
  end

  def test_shifting_period_suffix
    ['%Y%m%d', '%Y-%m-%d', '%Y'].each do |format|
      if format == '%Y%m%d' # default
        logger = Logger.new(@filename, 'now', 1048576)
      else # config
        logger = Logger.new(@filename, 'now', 1048576, shift_period_suffix: format)
      end
      begin
        yyyymmdd = Time.now.strftime(format)
        filename1 = @filename + ".#{yyyymmdd}"
        filename2 = @filename + ".#{yyyymmdd}.1"
        filename3 = @filename + ".#{yyyymmdd}.2"
        logger.info("0" * 15)
        logger.info("0" * 15)
        logger.info("0" * 15)
        assert_file.exist?(@filename)
        assert_file.exist?(filename1)
        assert_file.exist?(filename2)
        assert_file.exist?(filename3)
      ensure
        logger.close if logger
        [filename1, filename2, filename3].each do |filename|
          File.unlink(filename) if File.exist?(filename)
        end
      end
    end
  end

  def test_shifting_size_in_multiprocess
    tmpfile = Tempfile.new([File.basename(__FILE__, '.*'), '_1.log'])
    logfile = tmpfile.path
    logfile0 = logfile + '.0'
    logfile1 = logfile + '.1'
    logfile2 = logfile + '.2'
    tmpfile.close(true)
    File.unlink(logfile) if File.exist?(logfile)
    File.unlink(logfile0) if File.exist?(logfile0)
    File.unlink(logfile1) if File.exist?(logfile1)
    File.unlink(logfile2) if File.exist?(logfile2)
    begin
      stderr = run_children(2, [logfile], "#{<<-"begin;"}\n#{<<-'end;'}")
      begin;
        logger = Logger.new(ARGV[0], 4, 10)
        10.times do
          logger.info '0' * 15
        end
      end;
      assert_no_match(/log shifting failed/, stderr)
      assert_no_match(/log writing failed/, stderr)
      assert_no_match(/log rotation inter-process lock failed/, stderr)
    ensure
      File.unlink(logfile) if File.exist?(logfile)
      File.unlink(logfile0) if File.exist?(logfile0)
      File.unlink(logfile1) if File.exist?(logfile1)
      File.unlink(logfile2) if File.exist?(logfile2)
    end
  end

  def test_shifting_age_in_multiprocess
    yyyymmdd = Time.now.strftime("%Y%m%d")
    begin
      stderr = run_children(2, [@filename], "#{<<-"begin;"}\n#{<<-'end;'}")
      begin;
        logger = Logger.new(ARGV[0], 'now')
        10.times do
          logger.info '0' * 15
        end
      end;
      assert_no_match(/log shifting failed/, stderr)
      assert_no_match(/log writing failed/, stderr)
      assert_no_match(/log rotation inter-process lock failed/, stderr)
    ensure
      Dir.glob("#{@filename}.#{yyyymmdd}{,.[1-9]*}") do |filename|
        File.unlink(filename) if File.exist?(filename)
      end
    end
  end

  def test_open_without_header
    d(@filename, skip_header: true)

    assert_equal("", File.read(@filename))
  end

  def test_open_logfile_in_multiprocess
    tmpfile = Tempfile.new([File.basename(__FILE__, '.*'), '_1.log'])
    logfile = tmpfile.path
    tmpfile.close(true)
    begin
      20.times do
        run_children(2, [logfile], "#{<<-"begin;"}\n#{<<-'end;'}")
        begin;
          logfile = ARGV[0]
          logdev = Logger::LogDevice.new(logfile)
          logdev.send(:open_logfile, logfile)
        end;
        assert_equal(1, File.readlines(logfile).grep(/# Logfile created on/).size)
        File.unlink(logfile)
      end
    ensure
      File.unlink(logfile) if File.exist?(logfile)
    end
  end

  def test_shifting_size_not_rotate_too_much
    logdev0 = d(@filename)
    logdev0.__send__(:add_log_header, @tempfile)
    header_size = @tempfile.size
    message = "*" * 99 + "\n"
    shift_size = header_size + message.size * 3 - 1
    opt = {shift_age: 1, shift_size: shift_size}

    Dir.mktmpdir do |tmpdir|
      begin
        log = File.join(tmpdir, "log")
        logdev1 = d(log, opt)
        logdev2 = d(log, opt)

        assert_file.identical?(log, logdev1.dev)
        assert_file.identical?(log, logdev2.dev)

        3.times{logdev1.write(message)}
        assert_file.identical?(log, logdev1.dev)
        assert_file.identical?(log, logdev2.dev)

        logdev1.write(message)
        assert_file.identical?(log, logdev1.dev)
        # NOTE: below assertion fails in JRuby 9.3 and TruffleRuby
        unless %w[jruby truffleruby].include? RUBY_ENGINE
          assert_file.identical?(log + ".0", logdev2.dev)
        end

        logdev2.write(message)
        assert_file.identical?(log, logdev1.dev)
        assert_file.identical?(log, logdev2.dev)

        logdev1.write(message)
        assert_file.identical?(log, logdev1.dev)
        assert_file.identical?(log, logdev2.dev)
      ensure
        logdev1.close if logdev1
        logdev2.close if logdev2
      end
    end
  ensure
    logdev0.close
  end

  def test_shifting_midnight
    Dir.mktmpdir do |tmpdir|
      assert_in_out_err([*%W"--disable=gems -I#{@top_dir} -rlogger -C#{tmpdir} -"], "#{<<-"begin;"}\n#{<<-'end;'}")
      begin;
        begin
          module FakeTime
            attr_accessor :now
          end

          class << Time
            prepend FakeTime
          end

          log = "log"
          File.open(log, "w") {}
          File.utime(*[Time.mktime(2014, 1, 2, 0, 0, 0)]*2, log)

          Time.now = Time.mktime(2014, 1, 2, 23, 59, 59, 999000)
          dev = Logger::LogDevice.new(log, shift_age: 'daily')
          dev.write("#{Time.now} hello-1\n")
          File.utime(Time.now, Time.now, log)

          Time.now = Time.mktime(2014, 1, 3, 1, 1, 1)
          dev.write("#{Time.now} hello-2\n")
          File.utime(Time.now, Time.now, log)
        ensure
          dev.close
        end
      end;

      bug = '[GH-539]'
      log = File.join(tmpdir, "log")
      cont = File.read(log)
      assert_match(/hello-2/, cont)
      assert_not_match(/hello-1/, cont)
      assert_file.for(bug).exist?(log+".20140102")
      assert_match(/hello-1/, File.read(log+".20140102"), bug)
    end
  end

  env_tz_works = /linux|darwin|freebsd|openbsd/ =~ RUBY_PLATFORM # borrow from test/ruby/test_time_tz.rb

  def test_shifting_weekly
    Dir.mktmpdir do |tmpdir|
      assert_in_out_err([{"TZ"=>"UTC"}, *%W"-I#{@top_dir} -rlogger -C#{tmpdir} -"], "#{<<-"begin;"}\n#{<<-'end;'}")
      begin;
        begin
          module FakeTime
            attr_accessor :now
          end

          class << Time
            prepend FakeTime
          end

          log = "log"
          File.open(log, "w") {}
          Time.now = Time.utc(2015, 12, 14, 0, 1, 1)
          File.utime(Time.now, Time.now, log)

          dev = Logger::LogDevice.new("log", shift_age: 'weekly')

          Time.now = Time.utc(2015, 12, 19, 12, 34, 56)
          dev.write("#{Time.now} hello-1\n")
          File.utime(Time.now, Time.now, log)

          Time.now = Time.utc(2015, 12, 20, 0, 1, 1)
          dev.write("#{Time.now} hello-2\n")
          File.utime(Time.now, Time.now, log)
        ensure
          dev.close if dev
        end
      end;
      log = File.join(tmpdir, "log")
      cont = File.read(log)
      assert_match(/hello-2/, cont)
      assert_not_match(/hello-1/, cont)
      log = Dir.glob(log+".*")
      assert_equal(1, log.size)
      log, = *log
      cont = File.read(log)
      assert_match(/hello-1/, cont)
      assert_equal("2015-12-19", cont[/^[-\d]+/])
      assert_equal("20151219", log[/\d+\z/])
    end
  end if env_tz_works

  def test_shifting_monthly
    Dir.mktmpdir do |tmpdir|
      assert_in_out_err([{"TZ"=>"UTC"}, *%W"-I#{@top_dir} -rlogger -C#{tmpdir} -"], "#{<<-"begin;"}\n#{<<-'end;'}")
      begin;
        begin
          module FakeTime
            attr_accessor :now
          end

          class << Time
            prepend FakeTime
          end

          log = "log"
          File.open(log, "w") {}
          Time.now = Time.utc(2015, 12, 14, 0, 1, 1)
          File.utime(Time.now, Time.now, log)

          dev = Logger::LogDevice.new("log", shift_age: 'monthly')

          Time.now = Time.utc(2015, 12, 31, 12, 34, 56)
          dev.write("#{Time.now} hello-1\n")
          File.utime(Time.now, Time.now, log)

          Time.now = Time.utc(2016, 1, 1, 0, 1, 1)
          dev.write("#{Time.now} hello-2\n")
          File.utime(Time.now, Time.now, log)
        ensure
          dev.close if dev
        end
      end;
      log = File.join(tmpdir, "log")
      cont = File.read(log)
      assert_match(/hello-2/, cont)
      assert_not_match(/hello-1/, cont)
      log = Dir.glob(log+".*")
      assert_equal(1, log.size)
      log, = *log
      cont = File.read(log)
      assert_match(/hello-1/, cont)
      assert_equal("2015-12-31", cont[/^[-\d]+/])
      assert_equal("20151231", log[/\d+\z/])
    end
  end if env_tz_works

  def test_shifting_dst_change
    Dir.mktmpdir do |tmpdir|
      assert_in_out_err([{"TZ"=>"Europe/London"}, *%W"--disable=gems -I#{@top_dir} -rlogger -C#{tmpdir} -"], "#{<<-"begin;"}\n#{<<-'end;'}")
      begin;
        begin
          module FakeTime
            attr_accessor :now
          end

          class << Time
            prepend FakeTime
          end

          log = "log"
          File.open(log, "w") {}

          Time.now = Time.mktime(2014, 3, 30, 0, 1, 1)
          File.utime(Time.now, Time.now, log)

          dev = Logger::LogDevice.new(log, shift_age: 'daily')
          dev.write("#{Time.now} hello-1\n")
          File.utime(*[Time.mktime(2014, 3, 30, 0, 2, 3)]*2, log)

          Time.now = Time.mktime(2014, 3, 31, 0, 1, 1)
          dev.write("#{Time.now} hello-2\n")
          File.utime(Time.now, Time.now, log)
        ensure
          dev.close
        end
      end;

      log = File.join(tmpdir, "log")
      cont = File.read(log)
      assert_match(/hello-2/, cont)
      assert_not_match(/hello-1/, cont)
      assert_file.exist?(log+".20140330")
    end
  end if env_tz_works

  def test_shifting_weekly_dst_change
    Dir.mktmpdir do |tmpdir|
      assert_separately([{"TZ"=>"Europe/London"}, *%W"-I#{@top_dir} -W0 -rlogger -C#{tmpdir} -"], "#{<<-"begin;"}\n#{<<-'end;'}")
      begin;
        begin
          module FakeTime
            attr_accessor :now
          end

          class << Time
            prepend FakeTime
          end

          log = "log"
          File.open(log, "w") {}
          Time.now = Time.mktime(2015, 10, 25, 0, 1, 1)
          File.utime(Time.now, Time.now, log)

          dev = Logger::LogDevice.new("log", shift_age: 'weekly')
          dev.write("#{Time.now} hello-1\n")
          File.utime(Time.now, Time.now, log)
        ensure
          dev.close if dev
        end
      end;
      log = File.join(tmpdir, "log")
      cont = File.read(log)
      assert_match(/hello-1/, cont)
    end
  end if env_tz_works

  def test_shifting_monthly_dst_change
    Dir.mktmpdir do |tmpdir|
      assert_separately([{"TZ"=>"Europe/London"}, *%W"-I#{@top_dir} -W0 -rlogger -C#{tmpdir} -"], "#{<<-"begin;"}\n#{<<-'end;'}")
      begin;
        begin
          module FakeTime
            attr_accessor :now
          end

          class << Time
            prepend FakeTime
          end

          log = "log"
          File.open(log, "w") {}
          Time.now = Time.utc(2016, 9, 1, 0, 1, 1)
          File.utime(Time.now, Time.now, log)

          dev = Logger::LogDevice.new("log", shift_age: 'monthly')

          Time.now = Time.utc(2016, 9, 8, 7, 6, 5)
          dev.write("#{Time.now} hello-1\n")
          File.utime(Time.now, Time.now, log)

          Time.now = Time.utc(2016, 10, 9, 8, 7, 6)
          dev.write("#{Time.now} hello-2\n")
          File.utime(Time.now, Time.now, log)

          Time.now = Time.utc(2016, 10, 9, 8, 7, 7)
          dev.write("#{Time.now} hello-3\n")
          File.utime(Time.now, Time.now, log)
        ensure
          dev.close if dev
        end
      end;
      log = File.join(tmpdir, "log")
      cont = File.read(log)
      assert_match(/hello-2/, cont)
      assert_not_match(/hello-1/, cont)
      log = Dir.glob(log+".*")
      assert_equal(1, log.size)
      log, = *log
      cont = File.read(log)
      assert_match(/hello-1/, cont)
      assert_equal("2016-09-08", cont[/^[-\d]+/])
      assert_equal("20160930", log[/\d+\z/])
    end
  end if env_tz_works

  def test_shifting_midnight_exist_file
    Dir.mktmpdir do |tmpdir|
      assert_in_out_err([*%W"--disable=gems -I#{@top_dir} -rlogger -C#{tmpdir} -"], "#{<<-"begin;"}\n#{<<-'end;'}")
      begin;
        begin
          module FakeTime
            attr_accessor :now
          end

          class << Time
            prepend FakeTime
          end

          log = "log"
          File.open(log, "w") {}
          File.utime(*[Time.mktime(2014, 1, 2, 0, 0, 0)]*2, log)

          Time.now = Time.mktime(2014, 1, 2, 23, 59, 59, 999000)
          dev = Logger::LogDevice.new(log, shift_age: 'daily')
          dev.write("#{Time.now} hello-1\n")
          dev.close
          File.utime(Time.now, Time.now, log)

          Time.now = Time.mktime(2014, 1, 3, 1, 1, 1)
          dev = Logger::LogDevice.new(log, shift_age: 'daily')
          dev.write("#{Time.now} hello-2\n")
          File.utime(Time.now, Time.now, log)
        ensure
          dev.close
        end
      end;

      bug = '[GH-539]'
      log = File.join(tmpdir, "log")
      cont = File.read(log)
      assert_match(/hello-2/, cont)
      assert_not_match(/hello-1/, cont)
      assert_file.for(bug).exist?(log+".20140102")
      assert_match(/hello-1/, File.read(log+".20140102"), bug)
    end
  end

  def test_shifting_weekly_exist_file
    Dir.mktmpdir do |tmpdir|
      assert_in_out_err([{"TZ"=>"UTC"}, *%W"-I#{@top_dir} -rlogger -C#{tmpdir} -"], "#{<<-"begin;"}\n#{<<-'end;'}")
      begin;
        begin
          module FakeTime
            attr_accessor :now
          end

          class << Time
            prepend FakeTime
          end

          log = "log"
          File.open(log, "w") {}
          Time.now = Time.utc(2015, 12, 14, 0, 1, 1)
          File.utime(Time.now, Time.now, log)

          dev = Logger::LogDevice.new("log", shift_age: 'weekly')

          Time.now = Time.utc(2015, 12, 19, 12, 34, 56)
          dev.write("#{Time.now} hello-1\n")
          dev.close
          File.utime(Time.now, Time.now, log)

          Time.now = Time.utc(2015, 12, 20, 0, 1, 1)
          dev = Logger::LogDevice.new("log", shift_age: 'weekly')
          dev.write("#{Time.now} hello-2\n")
          File.utime(Time.now, Time.now, log)
        ensure
          dev.close if dev
        end
      end;
      log = File.join(tmpdir, "log")
      cont = File.read(log)
      assert_match(/hello-2/, cont)
      assert_not_match(/hello-1/, cont)
      log = Dir.glob(log+".*")
      assert_equal(1, log.size)
      log, = *log
      cont = File.read(log)
      assert_match(/hello-1/, cont)
      assert_equal("2015-12-19", cont[/^[-\d]+/])
      assert_equal("20151219", log[/\d+\z/])
    end
  end if env_tz_works

  def test_shifting_monthly_exist_file
    Dir.mktmpdir do |tmpdir|
      assert_in_out_err([{"TZ"=>"UTC"}, *%W"-I#{@top_dir} -rlogger -C#{tmpdir} -"], "#{<<-"begin;"}\n#{<<-'end;'}")
      begin;
        begin
          module FakeTime
            attr_accessor :now
          end

          class << Time
            prepend FakeTime
          end

          log = "log"
          File.open(log, "w") {}
          Time.now = Time.utc(2015, 12, 14, 0, 1, 1)
          File.utime(Time.now, Time.now, log)

          dev = Logger::LogDevice.new("log", shift_age: 'monthly')

          Time.now = Time.utc(2015, 12, 31, 12, 34, 56)
          dev.write("#{Time.now} hello-1\n")
          dev.close
          File.utime(Time.now, Time.now, log)

          Time.now = Time.utc(2016, 1, 1, 0, 1, 1)
          dev = Logger::LogDevice.new("log", shift_age: 'monthly')
          dev.write("#{Time.now} hello-2\n")
          File.utime(Time.now, Time.now, log)
        ensure
          dev.close if dev
        end
      end;
      log = File.join(tmpdir, "log")
      cont = File.read(log)
      assert_match(/hello-2/, cont)
      assert_not_match(/hello-1/, cont)
      log = Dir.glob(log+".*")
      assert_equal(1, log.size)
      log, = *log
      cont = File.read(log)
      assert_match(/hello-1/, cont)
      assert_equal("2015-12-31", cont[/^[-\d]+/])
      assert_equal("20151231", log[/\d+\z/])
    end
  end if env_tz_works

  # --- rotation failure handling -------------------------------------------

  def test_shift_failure_keeps_device_writable
    Dir.mktmpdir do |tmpdir|
      log = File.join(tmpdir, "log")
      logdev = d(log, shift_age: 2, shift_size: 0, skip_header: true)
      begin
        logdev.write("before\n")
        # Make the slot the live file has to move onto unusable.
        Dir.mkdir(log + ".0")
        warning = capture_shift_warnings { logdev.write("during\n") }

        assert_not_predicate(logdev.dev, :closed?,
                             "the device must stay open after a failed rotation")
        assert_match(/log shifting failed/, warning)
        # The pending message was still written, to the original device.
        assert_equal("before\nduring\n", File.binread(log).gsub("\r\n", "\n"))
      ensure
        logdev.close
      end
    end
  end

  def test_shift_failure_reports_stage
    Dir.mktmpdir do |tmpdir|
      log = File.join(tmpdir, "log")
      logdev = d(log, shift_age: 2, shift_size: 0, skip_header: true)
      begin
        logdev.write("before\n")
        Dir.mkdir(log + ".0")

        warning = capture_shift_warnings { logdev.write("during\n") }
        assert_match(/log shifting failed\. .*moving the current log file aside/,
                     warning)
      ensure
        logdev.close
      end
    end
  end

  def test_shift_failure_stage_is_reported_on_the_exception
    Dir.mktmpdir do |tmpdir|
      log = File.join(tmpdir, "log")
      logdev = d(log, shift_age: 2, shift_size: 0, skip_header: true)
      begin
        logdev.write("before\n")
        Dir.mkdir(log + ".0")

        # #shift_log_age is called directly here, so the failure escapes;
        # through #write it is reported and the message is still written.
        failure = nil
        capture_shift_warnings do
          failure = assert_raise(Logger::LogDevice::ShiftFailure) do
            logdev.send(:shift_log_age)
          end
        end
        assert_match(/moving the current log file aside/, failure.message)
        assert_equal(:rotate, failure.stage)
      ensure
        logdev.close
      end
    end
  end

  def test_shift_create_failure_keeps_previous_device
    Dir.mktmpdir do |tmpdir|
      log = File.join(tmpdir, "log")
      logdev = d(log, shift_age: 2, shift_size: 0, skip_header: true)
      begin
        logdev.write("before\n")
        old_dev = logdev.dev

        warning = capture_shift_warnings do
          with_io_failure(logdev, :create_replacement, Errno::ENOSPC.new("disk full")) do
            logdev.write("during\n")
          end
        end

        assert_match(/log shifting failed/, warning)
        assert_match(/creating the replacement log file/, warning)
        assert_same(old_dev, logdev.dev)
        assert_not_predicate(logdev.dev, :closed?)
        assert_equal("before\nduring\n", File.binread(log).gsub("\r\n", "\n"))
        # No half-created replacement is left behind.
        assert_empty(Dir.glob(log + ".*tmp"))
      ensure
        logdev.close
      end
    end
  end

  def test_shift_install_failure_restores_writable_log
    Dir.mktmpdir do |tmpdir|
      log = File.join(tmpdir, "log")
      logdev = d(log, shift_age: 2, shift_size: 0, skip_header: true)
      begin
        logdev.write("before\n")

        warning = capture_shift_warnings do
          with_io_failure(logdev, :install_replacement, Errno::ENOSPC.new("disk full")) do
            logdev.write("during\n")
          end
        end

        assert_match(/log shifting failed/, warning)
        assert_match(/installing the replacement log file/, warning)
        assert_not_predicate(logdev.dev, :closed?)
        # The failed rotation left the previous content recoverable and the
        # live file still at its own name; the message is written there.
        assert_file.exist?(log)
        assert_match(/during/, File.binread(log))

        logdev.write("after\n")
        assert_match(/after/, File.binread(log))
      ensure
        logdev.close
      end
    end
  end

  def test_shift_happens_once_when_failure_is_recovered
    Dir.mktmpdir do |tmpdir|
      log = File.join(tmpdir, "log")
      logdev = d(log, shift_age: 2, shift_size: 0, skip_header: true)
      begin
        logdev.write("one\n")
        with_io_failure(logdev, :install_replacement, Errno::ENOSPC.new("disk full")) do
          capture_shift_warnings { logdev.write("two\n") }
        end
        logdev.write("three\n")
      ensure
        logdev.close
      end

      rotated = File.binread(log + ".0").gsub("\r\n", "\n")
      live = File.binread(log).gsub("\r\n", "\n")
      # Each message is written exactly once across both files.
      assert_equal(["one", "two", "three"].sort, (rotated + live).lines.map(&:chomp).sort)
      assert_equal(1, (rotated + live).scan(/^one$/).size)
      assert_equal(1, (rotated + live).scan(/^two$/).size)
      assert_equal(1, (rotated + live).scan(/^three$/).size)
    end
  end

  def test_shift_does_not_replace_foreign_history_file
    Dir.mktmpdir do |tmpdir|
      log = File.join(tmpdir, "log")
      suffix = Time.now.strftime("%Y%m%d")
      foreign = "#{log}.#{suffix}"
      File.write(foreign, "FOREIGN\n")

      logdev = d(log, shift_age: 'now', skip_header: true)
      begin
        # 'now' rotates before each write, so the second write puts the first
        # message into the history.
        logdev.write("mine\n")
        logdev.write("latest\n")
      ensure
        logdev.close
      end

      # 'now' rotates before every write, so each write leaves the previous
      # content in a history file.  The foreign file keeps its name and its
      # content; the rotation moves on to the next free name instead.
      assert_equal("FOREIGN\n", File.binread(foreign).gsub("\r\n", "\n"))
      assert_equal("latest\n", File.binread(log).gsub("\r\n", "\n"))
      history = Dir.glob("#{foreign}.*").sort.map do |file|
        File.binread(file).gsub("\r\n", "\n")
      end
      assert_includes(history, "mine\n")
    end
  end

  def test_shift_aborts_before_touching_live_file_when_name_is_taken
    Dir.mktmpdir do |tmpdir|
      log = File.join(tmpdir, "log")
      suffix = Time.now.strftime("%Y%m%d")
      # Exhaust every candidate name so the reservation stage has to fail.
      File.write("#{log}.#{suffix}", "taken\n")
      1.upto(Logger::LogDevice::SHIFT_NAME_LIMIT) do |index|
        File.write("#{log}.#{suffix}.#{index}", "taken\n")
      end

      logdev = d(log, shift_age: 'now', skip_header: true)
      begin
        warning = capture_shift_warnings { logdev.write("survives\n") }
        assert_match(/log shifting failed/, warning)
        assert_match(/reserving a name/, warning)
        assert_not_predicate(logdev.dev, :closed?)
        assert_equal("survives\n", File.binread(log).gsub("\r\n", "\n"))
      ensure
        logdev.close
      end
    end
  end

  def test_shift_keeps_bounded_number_of_files
    Dir.mktmpdir do |tmpdir|
      log = File.join(tmpdir, "log")
      logdev = d(log, shift_age: 3, shift_size: 0, skip_header: true)
      begin
        %w[a b c d e].each {|message| logdev.write(message + "\n")}
      ensure
        logdev.close
      end

      files = Dir.glob(log + "*")
      assert_equal(3, files.size)
      assert_file.exist?(log)
      assert_file.exist?(log + ".0")
      assert_file.exist?(log + ".1")
      assert_file.not_exist?(log + ".2")
    end
  end

  # The close-and-rename strategy is used where an open file cannot be
  # unlinked or renamed (for example Windows without SHARE_DELETE).  The same
  # guarantees must hold there, so the platform answer is stubbed off on a
  # single device, leaving the class and other devices untouched.
  def without_open_file_operations(logdev)
    logdev.define_singleton_method(:unlink_open_file?) { false }
    logdev.define_singleton_method(:rename_open_file?) { false }
    yield
  end

  def test_close_and_rename_strategy_rotates_without_duplicates
    Dir.mktmpdir do |tmpdir|
      log = File.join(tmpdir, "log")
      logdev = d(log, shift_age: 3, shift_size: 0, skip_header: true)
      begin
        without_open_file_operations(logdev) do
          logdev.write("one\n")
          with_io_failure(logdev, :install_replacement, Errno::ENOSPC.new("disk full")) do
            capture_shift_warnings { logdev.write("two\n") }
          end
          logdev.write("three\n")
          assert_not_predicate(logdev.dev, :closed?)
        end
      ensure
        logdev.close
      end

      body = Dir.glob(log + "*").sort.map do |file|
        File.binread(file).gsub("\r\n", "\n")
      end.join
      assert_equal(["one", "three", "two"], body.lines.map(&:chomp).sort)
    end
  end

  def test_close_and_rename_strategy_keeps_foreign_history_file
    Dir.mktmpdir do |tmpdir|
      log = File.join(tmpdir, "log")
      suffix = Time.now.strftime("%Y%m%d")
      foreign = "#{log}.#{suffix}"
      File.write(foreign, "FOREIGN\n")

      logdev = d(log, shift_age: 'now', skip_header: true)
      begin
        without_open_file_operations(logdev) do
          logdev.write("mine\n")
          logdev.write("latest\n")
        end
      ensure
        logdev.close
      end

      assert_equal("FOREIGN\n", File.binread(foreign).gsub("\r\n", "\n"))
      assert_equal("latest\n", File.binread(log).gsub("\r\n", "\n"))
      history = Dir.glob("#{foreign}.*").map do |file|
        File.binread(file).gsub("\r\n", "\n")
      end
      assert_includes(history, "mine\n")
    end
  end

  private

  def run_children(n, args, src)
    r, w = IO.pipe
    [w, *(1..n).map do
       f = IO.popen([EnvUtil.rubybin, *%w[--disable=gems -], *args], "w", err: w)
       src = "$LOAD_PATH.unshift('#{@top_dir}'); require 'logger';#{src}"
       f.puts(src)
       f
     end].each(&:close)
    stderr = r.read
    r.close
    stderr
  end
end
