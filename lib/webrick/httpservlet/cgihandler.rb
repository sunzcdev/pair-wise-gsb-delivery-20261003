# frozen_string_literal: true
#
# cgihandler.rb -- CGIHandler Class
#
# Author: IPR -- Internet Programming with Ruby -- writers
# Copyright (c) 2001 TAKAHASHI Masayoshi, GOTOU Yuuzou
# Copyright (c) 2002 Internet Programming with Ruby writers. All rights
# reserved.
#
# $IPR: cgihandler.rb,v 1.27 2003/03/21 19:56:01 gotoyuzo Exp $

require 'rbconfig'
require_relative '../config'
require_relative 'abstract'

module WEBrick
  module HTTPServlet

    ##
    # Servlet for handling CGI scripts
    #
    # Example:
    #
    #  server.mount('/cgi/my_script', WEBrick::HTTPServlet::CGIHandler,
    #               '/path/to/my_script')

    class CGIHandler < AbstractServlet
      Ruby = RbConfig.ruby # :nodoc:
      CGIRunner = "\"#{Ruby}\" \"#{WEBrick::Config::LIBDIR}/httpservlet/cgi_runner.rb\"" # :nodoc:
      CGIRunnerArray = [Ruby, "#{WEBrick::Config::LIBDIR}/httpservlet/cgi_runner.rb".freeze].freeze # :nodoc:

      # Size of the chunks moved through the CGI pipes.
      PIPE_BUFFER_SIZE = 65536 # :nodoc:

      ##
      # Creates a new CGI script servlet for the script at +name+

      def initialize(server, name)
        super(server, name)
        @script_filename = name
        interpreter = server[:CGIInterpreter]
        if interpreter.is_a?(Array)
          @cgicmd = CGIRunnerArray + interpreter
        else
          @cgicmd = "#{CGIRunner} #{interpreter}"
        end
      end

      # :stopdoc:

      def do_GET(req, res)
        meta = req.meta_vars
        meta["SCRIPT_FILENAME"] = @script_filename
        meta["PATH"] = @config[:CGIPathEnv]
        meta.delete("HTTP_PROXY")
        if /mswin|bccwin|mingw/ =~ RUBY_PLATFORM
          meta["SystemRoot"] = ENV["SystemRoot"]
        end
        dump = Marshal.dump(meta)

        # Three independent pipes connect the parent with the CGI process:
        # * stdin carries the framed environment followed by the request body,
        # * stdout carries the CGI response headers and body,
        # * stderr carries diagnostics that must never corrupt the response.
        # Each direction is pumped by its own thread so that a script which
        # starts emitting output before consuming its input (or one which is
        # chatty on stderr) cannot fill a pipe buffer and deadlock either end.
        stdin_r, stdin_w = IO.pipe
        stdout_r, stdout_w = IO.pipe
        stderr_r, stderr_w = IO.pipe
        stdin_w.sync = true
        [stdin_w, stdout_r, stderr_r].each { |io| io.set_encoding(Encoding::ASCII_8BIT) }

        begin
          pid = spawn_cgi(stdin_r, stdout_w, stderr_w)
        rescue Exception
          # The process never started; nothing will close these ends for us.
          [stdin_r, stdin_w, stdout_r, stdout_w, stderr_r, stderr_w].each do |io|
            io.close unless io.closed?
          end
          raise
        end
        # The parent only keeps the ends it owns; the child-side ends are
        # closed here so EOF is observed once the process exits.
        stdin_r.close
        stdout_w.close
        stderr_w.close

        writer, stdout_thread, stderr_thread =
          pump_cgi(req, dump, stdin_w, stdout_r, stderr_r)
        pumps = [writer, stdout_thread, stderr_thread]
        begin
          # Join the writer first: it owns the request body and is the only
          # pump that can fail before the CGI process exits.  The readers
          # are deliberately joined afterwards because on the happy path they
          # only finish once the process closes its output pipes.
          writer.join
          if writer.error
            # The CGI runner used to hit this failure in the child and report
            # its status class on stderr; keep the diagnostic in the log.
            if writer.error.is_a?(HTTPStatus::Status)
              @logger.error("#{writer.error.class} occurred.")
            end
            raise writer.error
          end
          stdout_thread.join
          stderr_thread.join
        rescue Exception
          # Cancellation (request timeout, client disconnect, malformed
          # request body, ...): stop the process, then close every pipe so
          # the blocked reader pumps unblock and can be reclaimed.  The
          # process itself is reaped exactly once below.
          kill_cgi(pid)
          [stdin_w, stdout_r, stderr_r].each do |io|
            io.close unless io.closed?
          end
          pumps.each(&:join)
          raise
        ensure
          # Guarantee no pump thread outlives the request even if closing the
          # pipe failed to unblock one on some platform.
          pumps.each(&:kill)
          [stdin_w, stdout_r, stderr_r].each do |io|
            io.close unless io.closed?
          end
          _pid, status = Process.wait2(pid)
        end

        data = stdout_thread.value
        errmsg = stderr_thread.value
        exit_status = status.exitstatus
        if exit_status != 0
          @logger.error("CGIHandler: #{@script_filename} exit with #{exit_status}")
        end
        if errmsg && errmsg.bytesize > 0
          @logger.error("CGIHandler: #{@script_filename}:\n" + errmsg)
        end

        data = "" unless data
        raw_header, body = data.split(/^[\xd\xa]+/, 2)
        raise HTTPStatus::InternalServerError,
          "Premature end of script headers: #{@script_filename}" if body.nil?

        begin
          header = HTTPUtils::parse_header(raw_header)
          if /^(\d+)/ =~ header['status'][0]
            res.status = $1.to_i
            header.delete('status')
          end
          if header.has_key?('location')
            # RFC 3875 6.2.3, 6.2.4
            res.status = 302 unless (300...400) === res.status
          end
          if header.has_key?('set-cookie')
            header['set-cookie'].each{|k|
              res.cookies << Cookie.parse_set_cookie(k)
            }
            header.delete('set-cookie')
          end
          header.each{|key, val| res[key] = val.join(", ") }
        rescue => ex
          raise HTTPStatus::InternalServerError, ex.message
        end
        res.body = body
      end
      alias do_POST do_GET

      private

      # Starts the CGI runner with the three pipes attached to its standard
      # streams.  The runner reads the framed environment from +stdin_r+ and
      # then execs the actual script, keeping the same descriptors.
      def spawn_cgi(stdin_r, stdout_w, stderr_w)
        options = {
          in: stdin_r,
          out: stdout_w,
          err: stderr_w,
        }
        if @cgicmd.is_a?(Array)
          Process.spawn(*@cgicmd, options)
        else
          Process.spawn(@cgicmd, options)
        end
      end

      # Starts the three pump threads that own the parent-side pipes.
      #
      # The writer thread owns +stdin_w+: it sends the framed environment and
      # the request body, then closes the pipe.  Two reader threads own
      # +stdout_r+ and +stderr_r+ respectively, so a verbose script cannot
      # block on stderr while the response is still being collected.
      #
      # Every thread swallows pipe-level failures (the process exiting early,
      # a pipe being closed during cancellation) and exposes the collected
      # data through Thread#value; a failure while reading the request body is
      # captured by the writer and re-raised by #do_GET with its original
      # exception class preserved.
      def pump_cgi(req, dump, stdin_w, stdout_r, stderr_r)
        data = +""
        errmsg = +""

        # The writer never lets an exception escape from the thread.  A
        # failure while writing to the CGI pipe only means the process went
        # away first and is swallowed; a failure while reading the request
        # body (e.g. a client disconnect or a malformed body) propagates to
        # the Pump and is re-raised verbatim by #do_GET with its original
        # exception class preserved.
        writer = Pump.new(stdin_w) do |io|
          framing_ok = true
          begin
            io.write("%8d" % dump.bytesize)
            io.write(dump)
          rescue Errno::EPIPE, Errno::EINVAL, IOError
            # The process exited before the framing was consumed.
            framing_ok = false
          end
          if framing_ok
            req.body do |chunk|
              begin
                io.write(chunk)
              rescue Errno::EPIPE, Errno::EINVAL, IOError
                # The process went away mid body; stop feeding it.
                break
              end
            end
          end
        end

        stdout_thread = Pump.new(stdout_r) do |io|
          while (chunk = io.read(PIPE_BUFFER_SIZE))
            data << chunk
          end
          data
        end

        stderr_thread = Pump.new(stderr_r) do |io|
          while (chunk = io.read(PIPE_BUFFER_SIZE))
            errmsg << chunk
          end
          errmsg
        end

        [writer, stdout_thread, stderr_thread]
      end

      # A pump thread owns one end of a CGI pipe.  It always closes the pipe
      # when finished and never lets an exception surface as an unhandled
      # thread error; #error exposes the captured exception (if any) and
      # #value the block's result.
      class Pump
        attr_reader :error

        def initialize(io, &block)
          @error = nil
          @thread = Thread.new do
            Thread.current.report_on_exception = false
            begin
              @result = block.call(io)
            rescue Exception => error
              @error = error
            ensure
              io.close unless io.closed?
            end
          end
        end

        def join
          @thread.join
          self
        end

        def kill
          @thread.kill
          @thread.join
          self
        end

        def value
          @result
        end
      end

      # Signals a CGI process during cancellation.  On POSIX the runner
      # execs the script in place, so a single PID covers everything: a
      # graceful :TERM is sent first and a forced :KILL escalates if it does
      # not exit in time.  On Windows exec is emulated with a child process,
      # so the whole process tree is terminated; otherwise the actual script
      # would be orphaned.  Reaping stays the sole responsibility of the
      # +Process.wait2+ in #do_GET; a process that has already exited is
      # simply ignored.
      def kill_cgi(pid)
        if /mswin|bccwin|mingw/ =~ RUBY_PLATFORM
          terminate_process_tree(pid)
          return
        end

        begin
          Process.kill(:TERM, pid)
        rescue Errno::ESRCH
          return
        end
        deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + 1
        while Process.clock_gettime(Process::CLOCK_MONOTONIC) < deadline
          return unless process_alive?(pid)
          sleep 0.05
        end
        forced_kill(pid)
      end

      def forced_kill(pid)
        Process.kill(:KILL, pid)
      rescue Errno::ESRCH, Errno::EINVAL
        # The process has already exited.
      end

      if Process.respond_to?(:getpgid)
        def process_alive?(pid)
          Process.getpgid(pid)
          true
        rescue Errno::ESRCH
          false
        end
      else
        # Windows: signal 0 only checks whether the process can be signalled.
        def process_alive?(pid)
          Process.kill(0, pid)
          true
        rescue Errno::ESRCH, Errno::EPERM, Errno::EINVAL
          false
        end
      end

      # taskkill /T terminates a process together with the child processes
      # created by the Windows exec emulation in cgi_runner.rb.
      def terminate_process_tree(pid)
        null = File::NULL
        system("taskkill /F /T /PID #{pid}", out: null, err: null)
      end

      # :startdoc:
    end

  end
end
