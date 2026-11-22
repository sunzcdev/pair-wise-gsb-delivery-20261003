# frozen_string_literal: true
#
# server.rb -- GenericServer Class
#
# Author: IPR -- Internet Programming with Ruby -- writers
# Copyright (c) 2000, 2001 TAKAHASHI Masayoshi, GOTOU Yuuzou
# Copyright (c) 2002 Internet Programming with Ruby writers. All rights
# reserved.
#
# $IPR: server.rb,v 1.62 2003/07/22 19:20:43 gotoyuzo Exp $

require 'socket'
require_relative 'config'
require_relative 'log'

module WEBrick

  ##
  # Server error exception

  class ServerError < StandardError; end

  ##
  # Base server class

  class SimpleServer

    ##
    # A SimpleServer only yields when you start it

    def SimpleServer.start
      yield
    end
  end

  ##
  # A generic module for daemonizing a process

  class Daemon

    ##
    # Performs the standard operations for daemonizing a process.  Runs a
    # block, if given.

    def Daemon.start
      Process.daemon
      File.umask(0)
      yield if block_given?
    end
  end

  ##
  # Base TCP server class.  You must subclass GenericServer and provide a #run
  # method.

  class GenericServer

    ##
    # The server status.  One of :Stop, :Running or :Shutdown

    attr_reader :status

    ##
    # The server configuration

    attr_reader :config

    ##
    # The server logger.  This is independent from the HTTP access log.

    attr_reader :logger

    ##
    # Tokens control the number of outstanding clients.  The
    # <code>:MaxClients</code> configuration sets this.

    attr_reader :tokens

    ##
    # Sockets listening for connections.

    attr_reader :listeners

    ##
    # Creates a new generic server from +config+.  The default configuration
    # comes from +default+.

    def initialize(config={}, default=Config::General)
      @config = default.dup.update(config)
      @status = :Stop
      @config[:Logger] ||= Log::new
      @logger = @config[:Logger]

      @tokens = Thread::SizedQueue.new(@config[:MaxClients])
      @config[:MaxClients].times{ @tokens.push(nil) }

      webrickv = WEBrick::VERSION
      rubyv = "#{RUBY_VERSION} (#{RUBY_RELEASE_DATE}) [#{RUBY_PLATFORM}]"
      @logger.info("WEBrick #{webrickv}")
      @logger.info("ruby #{rubyv}")

      @listeners = []
      @listener_tokens = {}
      @shutdown_pipe = nil
      @notify_pipe = nil
      unless @config[:DoNotListen]
        raise ArgumentError, "Port must be an integer" unless @config[:Port].to_s == @config[:Port].to_i.to_s

        @config[:Port] = @config[:Port].to_i
        if @config[:Listen]
          warn(":Listen option is deprecated; use GenericServer#listen", uplevel: 1)
        end
        listen(@config[:BindAddress], @config[:Port],
               max_clients: @config[:MaxClientsPerListener])
        if @config[:Port] == 0
          @config[:Port] = @listeners[0].addr[1]
        end
      end
    end

    ##
    # Retrieves +key+ from the configuration

    def [](key)
      @config[key]
    end

    ##
    # Adds listeners from +address+ and +port+ to the server.  See
    # WEBrick::Utils::create_listeners for details.
    #
    # If +max_clients+ is given, each created listener accepts at most that
    # many concurrent connections.  While a listener has reached its limit it
    # is left out of the accept loop so that it neither busy-wakes the loop
    # nor starves the other listeners; the global <code>:MaxClients</code>
    # limit still applies on top of the per-listener limit.

    def listen(address, port, max_clients: nil)
      listeners = Utils::create_listeners(address, port)
      register_listeners(listeners, max_clients)
    end

    ##
    # Starts the server and runs the +block+ for each connection.  This method
    # does not return until the server is stopped from a signal handler or
    # another thread using #stop or #shutdown.
    #
    # If the block raises a subclass of StandardError the exception is logged
    # and ignored.  If an IOError or Errno::EBADF exception is raised the
    # exception is ignored.  If an Exception subclass is raised the exception
    # is logged and re-raised which stops the server.
    #
    # To completely shut down a server call #shutdown from ensure:
    #
    #   server = WEBrick::GenericServer.new
    #   # or WEBrick::HTTPServer.new
    #
    #   begin
    #     server.start
    #   ensure
    #     server.shutdown
    #   end

    def start(&block)
      raise ServerError, "already started." if @status != :Stop
      server_type = @config[:ServerType] || SimpleServer

      setup_shutdown_pipe

      server_type.start{
        @logger.info \
          "#{self.class}#start: pid=#{$$} port=#{@config[:Port]}"
        @status = :Running
        call_callback(:StartCallback)

        shutdown_pipe = @shutdown_pipe
        notify_pipe = setup_notify_pipe

        thgroup = ThreadGroup.new
        begin
          while @status == :Running
            begin
              sp = shutdown_pipe[0]
              np = notify_pipe && notify_pipe[0]
              # Listeners that reached their per-listener limit are left out
              # of the select set so a saturated listener does not keep
              # waking the loop without being able to accept.
              readers = [sp]
              readers << np if np
              @listeners.each{|svr|
                readers << svr unless listener_limit_reached?(svr)
              }
              if svrs = IO.select(readers)
                if svrs[0].include? sp
                  # swallow shutdown pipe
                  buf = String.new
                  nil while String === sp.read_nonblock(8, buf, exception: false)
                  break
                end
                if np && svrs[0].include?(np)
                  # a per-listener slot was released; swallow the notify pipe
                  # and rebuild the select set.
                  buf = String.new
                  nil while String === np.read_nonblock(8, buf, exception: false)
                  next
                end
                svrs[0].each{|svr|
                  @tokens.pop          # blocks while no token is there.
                  listener_tokens = @listener_tokens[svr]
                  begin
                    listener_tokens&.pop(true)
                  rescue ThreadError
                    # another connection grabbed the last per-listener slot.
                    @tokens.push(nil)
                    next
                  end
                  if sock = accept_client(svr)
                    unless config[:DoNotReverseLookup].nil?
                      sock.do_not_reverse_lookup = !!config[:DoNotReverseLookup]
                    end
                    begin
                      th = start_thread(sock, listener_tokens, &block)
                    rescue Exception
                      # return the reserved slots if the thread could not
                      # be started.
                      @tokens.push(nil)
                      listener_tokens&.push(nil)
                      begin
                        sock.close
                      rescue IOError
                      end
                      raise
                    end
                    th[:WEBrickThread] = true
                    thgroup.add(th)
                  else
                    @tokens.push(nil)
                    listener_tokens&.push(nil)
                  end
                }
              end
            rescue Errno::EBADF, Errno::ENOTSOCK, IOError => ex
              # if the listening socket was closed in GenericServer#shutdown,
              # IO::select raise it.
            rescue StandardError => ex
              msg = "#{ex.class}: #{ex.message}\n\t#{ex.backtrace[0]}"
              @logger.error msg
            rescue Exception => ex
              @logger.fatal ex
              raise
            end
          end
        ensure
          cleanup_shutdown_pipe(shutdown_pipe)
          cleanup_notify_pipe(notify_pipe)
          cleanup_listener
          @status = :Shutdown
          @logger.info "going to shutdown ..."
          thgroup.list.each{|th| th.join if th[:WEBrickThread] }
          call_callback(:StopCallback)
          @logger.info "#{self.class}#start done."
          @status = :Stop
        end
      }
    end

    ##
    # Stops the server from accepting new connections.

    def stop
      if @status == :Running
        @status = :Shutdown
      end

      alarm_shutdown_pipe {|f| f.write_nonblock("\0")}
    end

    ##
    # Shuts down the server and all listening sockets.  New listeners must be
    # provided to restart the server.

    def shutdown
      stop

      alarm_shutdown_pipe(&:close)
    end

    ##
    # You must subclass GenericServer and implement \#run which accepts a TCP
    # client socket

    def run(sock)
      @logger.fatal "run() must be provided by user."
    end

    private

    # :stopdoc:

    ##
    # Accepts a TCP client socket from the TCP server socket +svr+ and returns
    # the client socket.

    def accept_client(svr)
      case sock = svr.to_io.accept_nonblock(exception: false)
      when :wait_readable
        nil
      else
        if svr.respond_to?(:start_immediately)
          sock = OpenSSL::SSL::SSLSocket.new(sock, ssl_context)
          sock.sync_close = true
          # we cannot do OpenSSL::SSL::SSLSocket#accept here because
          # a slow client can prevent us from accepting connections
          # from other clients
        end
        sock
      end
    rescue Errno::ECONNRESET, Errno::ECONNABORTED,
           Errno::EPROTO, Errno::EINVAL
      nil
    rescue StandardError => ex
      msg = "#{ex.class}: #{ex.message}\n\t#{ex.backtrace[0]}"
      @logger.error msg
      nil
    end

    ##
    # Starts a server thread for the client socket +sock+ that runs the given
    # +block+.
    #
    # Sets the socket to the <code>:WEBrickSocket</code> thread local variable
    # in the thread.
    #
    # If +listener_tokens+ is given, the per-listener slot is returned to it
    # when the thread finishes.
    #
    # If any errors occur in the block they are logged and handled.

    def start_thread(sock, listener_tokens=nil, &block)
      Thread.start{
        begin
          Thread.current[:WEBrickSocket] = sock
          begin
            addr = sock.peeraddr
            @logger.debug "accept: #{addr[3]}:#{addr[1]}"
          rescue SocketError
            @logger.debug "accept: <address unknown>"
            raise
          end
          if sock.respond_to?(:sync_close=) && @config[:SSLStartImmediately]
            WEBrick::Utils.timeout(@config[:RequestTimeout]) do
              begin
                sock.accept # OpenSSL::SSL::SSLSocket#accept
              rescue Errno::ECONNRESET, Errno::ECONNABORTED,
                     Errno::EPROTO, Errno::EINVAL
                Thread.exit
              end
            end
          end
          call_callback(:AcceptCallback, sock)
          block ? block.call(sock) : run(sock)
        rescue Errno::ENOTCONN
          @logger.debug "Errno::ENOTCONN raised"
        rescue ServerError => ex
          msg = "#{ex.class}: #{ex.message}\n\t#{ex.backtrace[0]}"
          @logger.error msg
        rescue Exception => ex
          @logger.error ex
        ensure
          @tokens.push(nil)
          if listener_tokens
            listener_tokens.push(nil)
            wakeup_notify_pipe
          end
          Thread.current[:WEBrickSocket] = nil
          if addr
            @logger.debug "close: #{addr[3]}:#{addr[1]}"
          else
            @logger.debug "close: <address unknown>"
          end
          sock.close
        end
      }
    end

    ##
    # Calls the callback +callback_name+ from the configuration with +args+

    def call_callback(callback_name, *args)
      @config[callback_name]&.call(*args)
    end

    def setup_shutdown_pipe
      return @shutdown_pipe ||= IO.pipe
    end

    def cleanup_shutdown_pipe(shutdown_pipe)
      @shutdown_pipe = nil
      shutdown_pipe&.each(&:close)
    end

    ##
    # Pipe used to wake the accept loop when a per-listener connection slot
    # is released, so that a listener which had reached its limit is added
    # back to the select set.

    def setup_notify_pipe
      @notify_pipe = IO.pipe
    end

    def cleanup_notify_pipe(notify_pipe)
      @notify_pipe = nil
      notify_pipe&.each(&:close)
    end

    def wakeup_notify_pipe
      _, pipe = @notify_pipe # another thread may modify @notify_pipe.
      if pipe && !pipe.closed?
        begin
          pipe.write_nonblock("\0")
        rescue IO::WaitWritable, IOError
          # the pipe is full (a wakeup is already pending) or was closed
          # by another thread.
        end
      end
    end

    ##
    # Returns true if the listener +svr+ has a per-listener connection limit
    # and has reached it.

    def listener_limit_reached?(svr)
      tokens = @listener_tokens[svr]
      tokens ? tokens.empty? : false
    end

    ##
    # Adds +listeners+ to the server, giving each one a per-listener
    # connection limit of +max_clients+ unless it is nil.

    def register_listeners(listeners, max_clients) # :nodoc:
      if max_clients
        max_clients = Integer(max_clients)
        if max_clients < 1
          raise ArgumentError, "max_clients must be a positive integer"
        end
        listeners.each{|svr|
          tokens = Thread::SizedQueue.new(max_clients)
          max_clients.times{ tokens.push(nil) }
          @listener_tokens[svr] = tokens
        }
      end
      @listeners += listeners
    end

    def alarm_shutdown_pipe
      _, pipe = @shutdown_pipe # another thread may modify @shutdown_pipe.
      if pipe
        if !pipe.closed?
          begin
            yield pipe
          rescue IOError # closed by another thread.
          end
        end
      end
    end

    def cleanup_listener
      @listeners.each{|s|
        if @logger.debug?
          addr = s.addr
          @logger.debug("close TCPSocket(#{addr[2]}, #{addr[1]})")
        end
        begin
          s.shutdown
        rescue Errno::ENOTCONN
          # when 'Errno::ENOTCONN: Socket is not connected' on some platforms,
          # call #close instead of #shutdown.
          # (ignore @config[:ShutdownSocketWithoutClose])
          s.close
        else
          unless @config[:ShutdownSocketWithoutClose]
            s.close
          end
        end
      }
      @listeners.clear
      @listener_tokens.clear
    end
  end    # end of GenericServer
end
