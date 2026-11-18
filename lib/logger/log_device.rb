# frozen_string_literal: true

require_relative 'period'

class Logger
  # Device used for logging messages.
  #
  # == Cooperative rotation
  #
  # When a log file is written by more than one process, pass
  # <tt>cooperative: true</tt> to #initialize.  In that mode every write is
  # preceded by a check that this device still points at the file currently
  # found at #filename.  The comparison uses real file identity
  # (File.identical? over the File#stat device/inode pair), not the path:
  # if another process rotated the file away, or this process was forked
  # and inherited the parent's handle, the device is reopened and rebound
  # before the message is written, so an instance never keeps appending to
  # an archived history file.
  #
  # Actual rotation is still performed under an inter-process lock
  # (File#flock), and the file identity is rechecked while holding that
  # lock, so cooperating processes do not rename into the same archive
  # slot concurrently.  This coordination is intentionally limited to this
  # logger's own handles and archives on the local filesystem; it does not
  # claim that arbitrary network or exotic filesystems support atomic
  # cross-process rotation.
  #
  # A failed reopen/rebind never silently drops the working device: the
  # previous device is kept and the exception is available via
  # #reopen_error.  Process identity (#pid) and file identity
  # (#file_identity) are tracked separately.
  class LogDevice
    include Period

    attr_reader :dev
    attr_reader :filename
    # Whether cooperative rotation is enabled.  See the +cooperative+
    # keyword argument of #initialize.
    attr_reader :cooperative
    # The id of the process that owns the currently open device.
    # It is tracked separately from #file_identity so that a forked child
    # never writes through the parent's inherited file handle.
    attr_reader :pid
    # Identity of the file that the currently open device points at, or
    # +nil+ when the device is an IO object without a path.  The value is
    # taken from File#stat and is compared with File.identical?; it is not
    # a path, so it survives renames done during rotation.
    attr_reader :file_identity
    # The Exception saved by the most recent failed #reopen or cooperative
    # rebind, or +nil+ when the last (re)open succeeded.  A failed reopen
    # never silently closes the working device: the previous device is kept
    # and the failure is kept here so callers can inspect it.
    attr_reader :reopen_error
    include MonitorMixin

    def initialize(
      log = nil, shift_age: nil, shift_size: nil, shift_period_suffix: nil,
      binmode: false, reraise_write_errors: [], skip_header: false,
      cooperative: false
    )
      @dev = @filename = @shift_age = @shift_size = @shift_period_suffix = nil
      @binmode = binmode
      @reraise_write_errors = reraise_write_errors
      @skip_header = skip_header
      @cooperative = cooperative
      @pid = Process.pid
      @file_identity = nil
      @reopen_error = nil
      mon_initialize
      set_dev(log)
      remember_file_identity
      set_file(shift_age, shift_size, shift_period_suffix) if @filename
    end

    def write(message)
      handle_write_errors("writing") do
        synchronize do
          if @cooperative
            handle_write_errors("reopening") {check_cooperative_binding}
          elsif @shift_age and @dev.respond_to?(:stat)
            handle_write_errors("shifting") {check_shift_log}
          end
          handle_write_errors("writing") {@dev.write(message)}
        end
      end
    end

    def close
      begin
        synchronize do
          @dev.close rescue nil
        end
      rescue Exception
        @dev.close rescue nil
      end
    end

    def reopen(log = nil, shift_age: nil, shift_size: nil, shift_period_suffix: nil, binmode: nil)
      # reopen the same filename if no argument, do nothing for IO
      log ||= @filename if @filename
      @binmode = binmode unless binmode.nil?
      if log
        synchronize do
          old_dev = @dev
          old_filename = @filename
          same_target = old_filename && (log == old_filename)
          begin
            if @filename and @dev
              @dev.close rescue nil # close only file opened by Logger
              @filename = nil
            end
            set_dev(log)
            remember_file_identity
            set_file(shift_age, shift_size, shift_period_suffix) if @filename
            @pid = Process.pid
            @reopen_error = nil
          rescue Exception => error
            # Keep the instance inspectable after a failed reopen.  When the
            # target changed we restore the previously working device; when
            # we tried to reopen the same file its handle is already closed,
            # so we retain the path and let a later write/rebind recover.
            unless same_target
              @dev = old_dev
              @filename = old_filename
            end
            @reopen_error = error
            raise
          end
        end
      end
      self
    end

  private

    # :stopdoc:

    MODE = File::WRONLY | File::APPEND
    # TruffleRuby < 24.2 does not have File::SHARE_DELETE
    if File.const_defined? :SHARE_DELETE
      MODE_TO_OPEN = MODE | File::SHARE_DELETE | File::BINARY
    else
      MODE_TO_OPEN = MODE | File::BINARY
    end
    MODE_TO_CREATE = MODE_TO_OPEN | File::CREAT | File::EXCL

    # Capture the actual on-disk identity (device + inode pair) of the
    # open device.  Returns nil for an IO object that has no path or whose
    # file no longer exists.
    def remember_file_identity
      @file_identity = current_file_identity
    end

    def current_file_identity
      return nil unless @filename
      stat = @dev.respond_to?(:stat) ? @dev.stat : File.stat(@filename)
      [stat.dev, stat.rdev, stat.ino]
    rescue SystemCallError
      nil
    end

    # True when the device still points at the file found at +@filename+.
    # Uses File.identical? (real file identity) rather than the path, so a
    # rotated-away handle is detected even though the path still exists.
    def device_matches_file?
      return false unless @filename && @dev.respond_to?(:stat)
      return false if @dev.closed?
      return false unless File.exist?(@filename)
      File.identical?(@filename, @dev)
    rescue SystemCallError, IOError
      false
    end

    # Reopen +@filename+ and rebind the device to its current identity.
    # On failure the previous device is retained (when still usable) and the
    # exception is stored in #reopen_error so it stays inspectable.
    def rebind_to_current_file
      old_dev = @dev
      begin
        new_dev = open_logfile(@filename)
      rescue Exception => error
        # Expose the failure without disturbing the previous device; if that
        # device is itself unusable (e.g. closed after a failed rotation),
        # a later write will retry the rebind against the path.
        @reopen_error = error
        raise
      end
      @dev = new_dev
      remember_file_identity
      @pid = Process.pid
      @reopen_error = nil
      old_dev.close rescue nil unless old_dev.equal?(new_dev)
    end

    # Cooperative mode: before every write, make sure this instance still
    # owns a handle to the current log file.  A forked process or a rotation
    # performed by another process leaves the inherited/old handle pointing
    # at an archived file; rebind by actual file identity before writing.
    # When a rebind happens, size/period shift checks are skipped for this
    # write: the freshly bound device is already the current file and another
    # rebind inside the shift lock would only risk closing the handle again.
    # Rotation is reconsidered on the next write.
    def check_cooperative_binding
      return unless @filename
      rebound = false
      if @pid != Process.pid
        # Inherited handle after fork(2): the child must not share the
        # parent's offset/handle.  Rebind before the first write.
        rebind_to_current_file
        rebound = true
      elsif !device_matches_file?
        rebind_to_current_file
        rebound = true
      end
      check_shift_log if !rebound && @shift_age && @dev.respond_to?(:stat)
    end

    def set_dev(log)
      if log.respond_to?(:write) and log.respond_to?(:close)
        @dev = log
        if log.respond_to?(:path) and path = log.path
          if File.exist?(path)
            @filename = path
          end
        end
      else
        @dev = open_logfile(log)
        @filename = log
      end
    end

    def set_file(shift_age, shift_size, shift_period_suffix)
      @shift_age = shift_age || @shift_age || 7
      @shift_size = shift_size || @shift_size || 1048576
      @shift_period_suffix = shift_period_suffix || @shift_period_suffix || '%Y%m%d'

      unless @shift_age.is_a?(Integer)
        base_time = @dev.respond_to?(:stat) ? @dev.stat.mtime : Time.now
        @next_rotate_time = next_rotate_time(base_time, @shift_age)
      end
    end

    if MODE_TO_OPEN == MODE
      def fixup_mode(dev)
        dev
      end
    else
      def fixup_mode(dev)
        return dev if @binmode
        dev.autoclose = false
        old_dev = dev
        dev = File.new(dev.fileno, mode: MODE, path: dev.path)
        old_dev.close
        PathAttr.set_path(dev, filename) if defined?(PathAttr)
        dev
      end
    end

    def open_logfile(filename)
      begin
        dev = File.open(filename, MODE_TO_OPEN)
      rescue Errno::ENOENT
        create_logfile(filename)
      else
        dev = fixup_mode(dev)
        dev.sync = true
        dev.binmode if @binmode
        dev
      end
    end

    def create_logfile(filename)
      begin
        logdev = File.open(filename, MODE_TO_CREATE)
        logdev.flock(File::LOCK_EX)
        logdev = fixup_mode(logdev)
        logdev.sync = true
        logdev.binmode if @binmode
        add_log_header(logdev) unless @skip_header
        logdev.flock(File::LOCK_UN)
        logdev
      rescue Errno::EEXIST
        # file is created by another process
        open_logfile(filename)
      end
    end

    def handle_write_errors(mesg)
      yield
    rescue *@reraise_write_errors
      raise
    rescue
      warn("log #{mesg} failed. #{$!}")
    end

    def add_log_header(file)
      file.write(
        "# Logfile created on %s by %s\n" % [Time.now.to_s, Logger::ProgName]
      ) if file.size == 0
    end

    def check_shift_log
      if @shift_age.is_a?(Integer)
        # Note: always returns false if '0'.
        if @filename && (@shift_age > 0) && (@dev.stat.size > @shift_size)
          lock_shift_log { shift_log_age }
        end
      else
        now = Time.now
        if now >= @next_rotate_time
          @next_rotate_time = next_rotate_time(now, @shift_age)
          lock_shift_log { shift_log_period(previous_period_end(now, @shift_age)) }
        end
      end
    end

    def lock_shift_log
      retry_limit = 8
      retry_sleep = 0.1
      begin
        File.open(@filename, MODE_TO_OPEN) do |lock|
          lock.flock(File::LOCK_EX) # inter-process locking. will be unlocked at closing file
          if File.identical?(@filename, lock) and File.identical?(lock, @dev)
            yield # log shifting
          else
            # log shifted by another process (i-node before locking and i-node after locking are different)
            # Rebind by real file identity so two processes never keep
            # writing into the same archive slot through stale handles.
            rebind_to_current_file
          end
        end
        true
      rescue Errno::ENOENT
        # @filename file would not exist right after #rename and before #create_logfile
        if retry_limit <= 0
          warn("log rotation inter-process lock failed. #{$!}")
        else
          sleep retry_sleep
          retry_limit -= 1
          retry_sleep *= 2
          retry
        end
      end
    rescue
      warn("log rotation inter-process lock failed. #{$!}")
    end

    def shift_log_age
      (@shift_age-3).downto(0) do |i|
        if FileTest.exist?("#{@filename}.#{i}")
          File.rename("#{@filename}.#{i}", "#{@filename}.#{i+1}")
        end
      end
      shift_log_file("#{@filename}.0")
    end

    def shift_log_period(period_end)
      suffix = period_end.strftime(@shift_period_suffix)
      age_file = "#{@filename}.#{suffix}"
      if FileTest.exist?(age_file)
        # try to avoid filename crash caused by Timestamp change.
        idx = 0
        # .99 can be overridden; avoid too much file search with 'loop do'
        while idx < 100
          idx += 1
          age_file = "#{@filename}.#{suffix}.#{idx}"
          break unless FileTest.exist?(age_file)
        end
      end
      shift_log_file(age_file)
    end

    def shift_log_file(shifted)
      stat = @dev.stat
      @dev.close rescue nil
      begin
        File.rename(@filename, shifted)
      rescue SystemCallError
        # On filesystems that refuse to rename a file still open by another
        # process (e.g. Windows), the rename can fail after our handle was
        # closed.  Reopen the original path so this instance keeps a usable
        # device instead of a closed stream, then let the caller surface the
        # rotation failure; the next write will retry coordination.
        @dev = open_logfile(@filename)
        remember_file_identity
        raise
      end
      @dev = create_logfile(@filename)
      remember_file_identity
      mode, uid, gid = stat.mode, stat.uid, stat.gid
      begin
        @dev.chmod(mode) if mode
        mode = nil
        @dev.chown(uid, gid)
      rescue Errno::EPERM
        if mode
          # failed to chmod, probably nothing can do more.
        elsif uid
          uid = nil
          retry # to change gid only
        end
      end
      return true
    end
  end
end

File.open(__FILE__) do |f|
  File.new(f.fileno, autoclose: false, path: "").path
rescue IOError
  module PathAttr               # :nodoc:
    attr_reader :path

    def self.set_path(file, path)
      file.extend(self).instance_variable_set(:@path, path)
    end
  end
end
