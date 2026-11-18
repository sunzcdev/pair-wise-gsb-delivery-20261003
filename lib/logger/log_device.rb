# frozen_string_literal: true

require_relative 'period'
require_relative 'errors'

class Logger
  # Device used for logging messages.
  class LogDevice
    include Period

    # Internal signal that a rotation attempt failed.
    #
    # Rotation has a preparation phase and a publication phase.  Preparing
    # creates the replacement log file, shifts the older rotated files and
    # reserves the name the current log file will take; none of that touches
    # the live log file, so a failure there leaves the logger writing to the
    # same, still-writable device.  Publishing moves the current log file onto
    # the reserved name and installs the replacement, and puts the previous
    # log file back if it cannot finish.
    #
    # +stage+ names the step that failed and +error+ is the underlying
    # exception, so a caller can tell a failed rename from a failed create.
    class ShiftFailure < ShiftingError # :nodoc:
      attr_reader :stage
      attr_reader :error

      def initialize(stage, error)
        @stage = stage
        @error = error
        description = SHIFT_STAGES[stage] || stage.to_s.tr("_", " ")
        super("#{description} failed (#{error.class}: #{error.message})")
      end
    end

    attr_reader :dev
    attr_reader :filename
    include MonitorMixin

    def initialize(
      log = nil, shift_age: nil, shift_size: nil, shift_period_suffix: nil,
      binmode: false, reraise_write_errors: [], skip_header: false
    )
      @dev = @filename = @shift_age = @shift_size = @shift_period_suffix = nil
      @binmode = binmode
      @reraise_write_errors = reraise_write_errors
      @skip_header = skip_header
      mon_initialize
      set_dev(log)
      set_file(shift_age, shift_size, shift_period_suffix) if @filename
    end

    def write(message)
      handle_write_errors("writing") do
        synchronize do
          if @shift_age and @dev.respond_to?(:stat)
            handle_shift_errors {check_shift_log}
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
          if @filename and @dev
            @dev.close rescue nil # close only file opened by Logger
            @filename = nil
          end
          set_dev(log)
          set_file(shift_age, shift_size, shift_period_suffix) if @filename
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

    # True when a file may be unlinked while a handle to it is still open.
    # Windows only allows that when the handle was opened with SHARE_DELETE;
    # POSIX always allows it.
    UNLINK_OPEN_FILE =
      if File.const_defined?(:SHARE_DELETE)
        true
      else
        File::ALT_SEPARATOR.nil?
      end

    # True when an open log file may be renamed.  On Windows that needs
    # SHARE_DELETE on the handle, which #MODE_TO_OPEN adds where available.
    RENAME_OPEN_FILE = UNLINK_OPEN_FILE

    # The questions are asked through methods so a device can be tested as if
    # the platform did not support one of the operations.
    def unlink_open_file?
      UNLINK_OPEN_FILE
    end

    def rename_open_file?
      RENAME_OPEN_FILE
    end

    # How many alternative names are tried before a rotation gives up.  A
    # rotation never takes a name that is already in use, so running out of
    # names is reported instead of overwriting a history file.
    SHIFT_NAME_LIMIT = 100

    # Descriptions of the stages of a rotation, used when one fails.
    SHIFT_STAGES = {
      stat: "inspecting the log file",
      create_replacement: "creating the replacement log file",
      shift_history: "shifting the older rotated log files",
      reserve: "reserving a name for the rotated log file",
      rotate: "moving the current log file aside",
      install: "installing the replacement log file",
    }.freeze

    # Errors that mean the file system does not support hard links.  Only for
    # these is close-and-rename tried as a fallback: any other error is a real
    # failure and is reported as such.
    LINK_UNSUPPORTED_ERRORS = [Errno::EPERM, Errno::EXDEV, Errno::EMLINK]
    LINK_UNSUPPORTED_ERRORS << Errno::ENOSYS if defined?(Errno::ENOSYS)
    LINK_UNSUPPORTED_ERRORS << Errno::EOPNOTSUPP if defined?(Errno::EOPNOTSUPP)
    LINK_UNSUPPORTED_ERRORS.freeze

    # A log file created ahead of being published as the live log file.
    Replacement = Struct.new(:dev, :path) # :nodoc:

    # A name reserved for the rotated log file, and how it is held.
    #
    # With +:link+ the name is a hard link to the live log file, so the open
    # handle keeps writing to the same content.  With +:placeholder+ the name
    # is an empty file that the live log file will be renamed onto.  With
    # +:overwrite+ the name belongs to the size rotation ring and the live log
    # file replaces its content by rename.
    Reservation = Struct.new(:name, :kind) # :nodoc:

    # Everything the publication phase needs; see #prepare_shift.
    ShiftPlan = Struct.new(:live_name, :reservation, :replacement) # :nodoc:

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

    # Reports a failed rotation without losing the message being written.
    #
    # The message is written afterwards by #write to whatever device is
    # current, so a rotation failure does not duplicate it either: the
    # rotation never writes a log message itself.
    def handle_shift_errors
      yield
    rescue ShiftFailure => failure
      if @reraise_write_errors.any? {|klass| failure.error.is_a?(klass)}
        raise failure.error
      end
      warn("log shifting failed. #{failure.message}")
    rescue *@reraise_write_errors
      raise
    rescue
      warn("log shifting failed. #{$!}")
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
          lock_shift_log { shift_log_period(previous_period_end(now, @shift_age)) }
          # Advance only once the rotation went through (or was found to be
          # already done by another process), so a failure is retried by the
          # next write instead of being skipped until the next period.
          @next_rotate_time = next_rotate_time(now, @shift_age)
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
            @dev.close rescue nil
            @dev = open_logfile(@filename)
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
      rescue ShiftingError
        # A stage specific failure; keep it for #handle_shift_errors.
        raise
      rescue
        warn("log rotation inter-process lock failed. #{$!}")
      end
    rescue ShiftingError
      raise
    rescue
      warn("log rotation inter-process lock failed. #{$!}")
    end

    # Rotates the log file when it has grown past @shift_size.
    def shift_log_age
      publish_shift(prepare_shift(age_history_chain, "#{@filename}.0", ring: true))
    end

    # Rotates the log file when the current period has ended.
    def shift_log_period(period_end)
      base = "#{@filename}.#{period_end.strftime(@shift_period_suffix)}"
      publish_shift(prepare_shift([], base))
    end

    # The rotated names used by size rotation, oldest last.
    #
    # Size rotation keeps a fixed number of files, so the live log file takes
    # the first slot and the content of the last slot is dropped.  With
    # +shift_age+ of two there is a single slot, exactly as before.
    def age_history_chain
      (0..(@shift_age - 2)).map {|i| "#{@filename}.#{i}"}
    end

    # Preparation phase: build everything publication will need, without
    # touching the live log file or the device that is writing to it.
    #
    # +ring+ marks the bounded size rotation, whose oldest slot is deliberately
    # recycled.  Period rotation leaves pre-existing names alone: a conflict
    # moves on to the next candidate instead of overwriting a history file.
    def prepare_shift(history_chain, rotated_base, ring: false)
      replacement = begin
        create_replacement
      rescue ShiftFailure
        raise
      rescue SystemCallError, IOError => error
        raise ShiftFailure.new(:create_replacement, error)
      end
      begin
        shift_history(history_chain)
      rescue SystemCallError, IOError => error
        discard_replacement(replacement)
        raise ShiftFailure.new(:shift_history, error)
      end
      begin
        reservation = reserve_rotated_name(rotated_base, ring: ring)
      rescue ShiftFailure
        discard_replacement(replacement)
        raise
      rescue SystemCallError, IOError => error
        discard_replacement(replacement)
        raise ShiftFailure.new(:reserve, error)
      end
      ShiftPlan.new(@filename, reservation, replacement)
    end

    # Creates the log file that will become the live log file.
    #
    # It is created under a name owned by this device, so a failure never
    # truncates or replaces a file that somebody else may be using.
    def create_replacement
      stat = begin
        @dev.stat
      rescue SystemCallError, IOError => error
        raise ShiftFailure.new(:stat, error)
      end
      path = replacement_path
      dev = open_replacement(path)
      begin
        dev = fixup_mode(dev)
        dev.sync = true
        dev.binmode if @binmode
        add_log_header(dev) unless @skip_header
        apply_metadata(dev, stat)
      rescue SystemCallError, IOError => error
        dev.close rescue nil
        File.unlink(path) rescue nil
        raise ShiftFailure.new(:create_replacement, error)
      end
      Replacement.new(dev, path)
    end

    # Opens the replacement log file exclusively.
    #
    # The name is derived from the process and this device, so it can only be
    # left over from an earlier failed rotation of this same device.  Such a
    # leftover is discarded and created afresh, which also guarantees the new
    # live log file starts out empty.
    def open_replacement(path)
      begin
        return File.open(path, MODE_TO_CREATE)
      rescue Errno::EEXIST
        # Stale leftover; nobody else can own this name.
      end
      File.unlink(path) rescue nil
      File.open(path, MODE_TO_CREATE)
    end

    def replacement_path
      "#{@filename}.#{Process.pid}.#{object_id}.tmp"
    end

    # Copies the mode and ownership of the rotated file onto its replacement,
    # so rotating does not silently change permissions.
    def apply_metadata(dev, stat)
      mode, uid, gid = stat.mode, stat.uid, stat.gid
      begin
        dev.chmod(mode) if mode
        mode = nil
        dev.chown(uid, gid)
      rescue Errno::EPERM
        if mode
          # Failed to chmod, probably nothing can be done about it.
        elsif uid
          uid = nil
          retry # to change gid only
        end
      end
    end

    # Moves each existing history file one slot down.  The oldest slot is
    # dropped, as it always was; a failure here happens before the live log
    # file is touched, so it costs nothing but the rotation attempt.
    def shift_history(chain)
      (chain.size - 1).downto(1) do |index|
        source = chain[index - 1]
        next unless File.exist?(source)
        File.rename(source, chain[index])
      end
    end

    # Reserves a name for the current log file without ever taking a name that
    # is already in use.
    #
    # Size rotation works on a fixed ring of numbered slots, so its oldest slot
    # is meant to be replaced; publishing renames the live file onto it, which
    # is atomic and leaves the live file untouched if it fails.  Period
    # rotation keeps every period and must never replace an existing history
    # file, so there the name is reserved with a link or an exclusive
    # placeholder and a conflict moves on to the next candidate.
    def reserve_rotated_name(base, ring: false)
      # A ring slot is meant to be replaced, so no name has to be found for it;
      # #publish_shift decides whether the live file can be renamed onto it
      # while open or has to be closed first.
      return Reservation.new(base, :overwrite) if ring
      if unlink_open_file? and File.respond_to?(:link)
        begin
          return link_rotated_name(base)
        rescue *LINK_UNSUPPORTED_ERRORS
          # This file system refuses hard links; use close-and-rename instead.
        end
      end
      place_rotated_name(base)
    end

    def link_rotated_name(base)
      candidates(base).each do |name|
        begin
          File.link(@filename, name)
        rescue Errno::EEXIST
          next
        end
        return Reservation.new(name, :link)
      end
      raise ShiftFailure.new(:reserve, name_conflict(base))
    end

    def place_rotated_name(base)
      candidates(base).each do |name|
        begin
          File.open(name, MODE_TO_CREATE) {|placeholder| placeholder.flock(File::LOCK_EX)}
        rescue Errno::EEXIST
          next
        end
        return Reservation.new(name, :placeholder)
      end
      raise ShiftFailure.new(:reserve, name_conflict(base))
    end

    # The preferred name first, then numeric alternatives, as period rotation
    # has always done; the limit keeps the search bounded.
    def candidates(base)
      0.upto(SHIFT_NAME_LIMIT - 1).map {|index| index.zero? ? base : "#{base}.#{index}"}
    end

    def name_conflict(base)
      Errno::EEXIST.new(
        "#{base} is already in use (as are #{SHIFT_NAME_LIMIT - 1} alternatives)",
      )
    end

    def discard_replacement(replacement)
      replacement.dev.close rescue nil
      File.unlink(replacement.path) rescue nil
    end

    # Publication phase: make the replacement the live log file.
    #
    # The live handle is kept open whenever the platform allows renaming or
    # unlinking an open file, so the messages already written stay reachable
    # and the logger never holds a closed device.  Only where that is
    # impossible is the file closed, and then it is reopened if a step fails.
    def publish_shift(plan)
      if plan.reservation.kind == :placeholder or
         (plan.reservation.kind == :overwrite and not rename_open_file?)
        publish_reopening(plan)
      else
        publish_holding(plan)
      end
    rescue ShiftFailure
      # The replacement is never installed, so its file and handle are
      # dropped; whatever device is current is the one the next write uses.
      discard_replacement(plan.replacement)
      discard_reservation(plan)
      raise
    end

    # Drops the name this rotation reserved when it is not needed any more.
    #
    # After a failed publication the previous content must exist exactly once.
    # A linked reservation becomes a redundant second name for it once the live
    # name is back, and an unused placeholder is an empty file that nobody
    # wants.  The reservation is kept when it is the only remaining copy.
    def discard_reservation(plan)
      reservation = plan.reservation
      return if reservation.kind == :overwrite
      return unless File.exist?(reservation.name)
      if reservation.kind == :placeholder
        return unless File.size(reservation.name) == 0
      elsif not File.exist?(plan.live_name)
        return
      end
      File.unlink(reservation.name) rescue nil
    end

    # Publication while the open handle stays valid.
    #
    # For a linked reservation the content is already reachable under the
    # rotated name, so releasing the live name is enough.  For the size ring
    # the live file is renamed onto its oldest slot, replacing it atomically.
    def publish_holding(plan)
      reservation = plan.reservation
      begin
        release_live_name(plan.live_name, reservation)
      rescue SystemCallError, IOError => error
        raise ShiftFailure.new(:rotate, error)
      end
      begin
        install_replacement(plan.replacement, plan.live_name)
      rescue SystemCallError, IOError => error
        restore_live_name(plan.live_name, reservation)
        raise ShiftFailure.new(:install, error)
      end
      swap_dev(plan.replacement)
    end

    def release_live_name(live_name, reservation)
      if reservation.kind == :overwrite
        File.rename(live_name, reservation.name)
      else
        File.unlink(live_name)
      end
    end

    # Restores the live name after a failed install.  The previous content is
    # still reachable under the rotated name; for a link it can simply be
    # linked back, while an overwrite has to be renamed back and loses that
    # ring slot instead of the log file.
    def restore_live_name(live_name, reservation)
      return if File.exist?(live_name)
      if reservation.kind == :link
        begin
          File.link(reservation.name, live_name)
          return
        rescue SystemCallError, IOError
          # fall through to the rename
        end
      end
      File.rename(reservation.name, live_name) rescue nil
    end

    # Publication where the open file has to be closed first, either because
    # the platform cannot rename an open file or because only a placeholder
    # was reserved.  The previous log file is reopened if a step fails, so the
    # logger is left with a writable device in every case.
    def publish_reopening(plan)
      reservation = plan.reservation
      previous = @dev
      previous.close rescue nil
      begin
        File.rename(plan.live_name, reservation.name)
      rescue SystemCallError, IOError => error
        @dev = reopen_live_file(plan.live_name) || previous
        raise ShiftFailure.new(:rotate, error)
      end
      begin
        install_replacement(plan.replacement, plan.live_name)
      rescue SystemCallError, IOError => error
        File.rename(reservation.name, plan.live_name) rescue nil
        @dev = reopen_live_file(plan.live_name) ||
               reopen_live_file(reservation.name) ||
               previous
        raise ShiftFailure.new(:install, error)
      end
      swap_dev(plan.replacement)
    end

    def swap_dev(replacement)
      previous = @dev
      @dev = replacement.dev
      previous.close rescue nil
      true
    end

    def install_replacement(replacement, live_name)
      File.rename(replacement.path, live_name)
    end

    def reopen_live_file(live_name)
      open_logfile(live_name)
    rescue SystemCallError, IOError
      nil
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
