# frozen_string_literal: true
require "monitor"
require_relative "file_utils"

module Rake
  #
  # FileUtilsExt provides a custom version of the FileUtils methods
  # that respond to the <tt>verbose</tt> and <tt>nowrite</tt>
  # commands.
  #
  module FileUtilsExt
    include FileUtils

    class << self
      attr_accessor :verbose_flag, :nowrite_flag
    end

    DEFAULT = Object.new

    FileUtilsExt.verbose_flag = DEFAULT
    FileUtilsExt.nowrite_flag = false

    # Process wide lock that serializes every block which changes the
    # process wide working directory through #in_dir.
    #
    # Dir.chdir is global for the whole process (it is not scoped to the
    # current Thread), so a multitask running its prerequisites in parallel
    # cannot rely on two blocks each calling Dir.chdir at the same time.
    # Holding this lock while the directory is changed makes that boundary
    # explicit and guarantees such blocks never interleave.
    CHDIR_LOCK = Monitor.new

    FileUtils.commands.each do |name|
      opts = FileUtils.options_of name
      default_options = []
      if opts.include?("verbose")
        default_options << "verbose: FileUtilsExt.verbose_flag"
      end
      if opts.include?("noop")
        default_options << "noop: FileUtilsExt.nowrite_flag"
      end

      next if default_options.empty?
      module_eval(<<-EOS, __FILE__, __LINE__ + 1)
      def #{name}(*args, **options, &block)
        super(*args,
            #{default_options.join(', ')},
            **options, &block)
      end
      EOS
    end

    # Get/set the verbose flag controlling output from the FileUtils
    # utilities.  If verbose is true, then the utility method is
    # echoed to standard output.
    #
    # Examples:
    #    verbose              # return the current value of the
    #                         # verbose flag
    #    verbose(v)           # set the verbose flag to _v_.
    #    verbose(v) { code }  # Execute code with the verbose flag set
    #                         # temporarily to _v_.  Return to the
    #                         # original value when code is done.
    def verbose(value=nil)
      oldvalue = FileUtilsExt.verbose_flag
      FileUtilsExt.verbose_flag = value unless value.nil?

      if block_given?
        begin
          yield
        ensure
          FileUtilsExt.verbose_flag = oldvalue
        end
      end
      FileUtilsExt.verbose_flag
    end

    # Get/set the nowrite flag controlling output from the FileUtils
    # utilities.  If verbose is true, then the utility method is
    # echoed to standard output.
    #
    # Examples:
    #    nowrite              # return the current value of the
    #                         # nowrite flag
    #    nowrite(v)           # set the nowrite flag to _v_.
    #    nowrite(v) { code }  # Execute code with the nowrite flag set
    #                         # temporarily to _v_. Return to the
    #                         # original value when code is done.
    def nowrite(value=nil)
      oldvalue = FileUtilsExt.nowrite_flag
      FileUtilsExt.nowrite_flag = value unless value.nil?
      if block_given?
        begin
          yield
        ensure
          FileUtilsExt.nowrite_flag = oldvalue
        end
      end
      oldvalue
    end

    # Use this function to prevent potentially destructive ruby code
    # from running when the :nowrite flag is set.
    #
    # Example:
    #
    #   when_writing("Building Project") do
    #     project.build
    #   end
    #
    # The following code will build the project under normal
    # conditions. If the nowrite(true) flag is set, then the example
    # will print:
    #
    #      DRYRUN: Building Project
    #
    # instead of actually building the project.
    #
    def when_writing(msg=nil)
      if FileUtilsExt.nowrite_flag
        $stderr.puts "DRYRUN: #{msg}" if msg
      else
        yield
      end
    end

    # Run a block with +dir+ as the process wide working directory.
    #
    # Unlike Kernel#Dir.chdir this helper never pretends to be thread local:
    # the working directory is shared by every thread in the process, so the
    # whole change (chdir, block and the restore of the original directory)
    # is wrapped in the process wide FileUtilsExt::CHDIR_LOCK.  When this
    # helper is used inside a +multitask+ the affected blocks therefore run
    # one at a time instead of racing on Dir.pwd.
    #
    # The original working directory is always restored, even when the block
    # raises, so a failing command cannot leave the process in +dir+.
    #
    # Prefer <tt>sh "command", chdir: "dir"</tt> when a block only launches an
    # external command.  The +chdir:+ option starts that single subprocess in
    # +dir+ through the native spawn +chdir+ option and does not touch the
    # process wide directory at all, which stays safe for parallel tasks.
    #
    # Example:
    #
    #   in_dir "build" do
    #     File.write "config.log", "configured"
    #   end
    #
    def in_dir(dir)
      raise ArgumentError, "a block is required" unless block_given?
      FileUtilsExt::CHDIR_LOCK.synchronize do
        original = Dir.pwd
        begin
          Dir.chdir dir
          yield
        ensure
          Dir.chdir original
        end
      end
    end

    # Send the message to the default rake output (which is $stderr).
    def rake_output_message(message)
      $stderr.puts(message)
    end

    # Check that the options do not contain options not listed in
    # +optdecl+.  An ArgumentError exception is thrown if non-declared
    # options are found.
    def rake_check_options(options, *optdecl)
      h = options.dup
      optdecl.each do |name|
        h.delete name
      end
      raise ArgumentError, "no such option: #{h.keys.join(' ')}" unless
        h.empty?
    end

    extend self
  end
end
