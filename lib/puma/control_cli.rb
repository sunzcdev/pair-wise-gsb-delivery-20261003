# frozen_string_literal: true

require 'optparse'
require_relative 'const'
require_relative 'detect'
require 'uri'
require 'socket'

module Puma
  class ControlCLI

    # values must be string or nil
    # value of `nil` means command cannot be processed via signal
    # @version 5.0.3
    CMD_PATH_SIG_MAP = {
      'gc'       => nil,
      'gc-stats' => nil,
      'halt'              => 'SIGQUIT',
      'info'              => Puma.backtrace_signal,
      'phased-restart'    => 'SIGUSR1',
      'refork'            => 'SIGURG',
      'reload-worker-directory' => nil,
      'reopen-log'        => 'SIGHUP',
      'restart'           => 'SIGUSR2',
      'start'    => nil,
      'stats'    => nil,
      'status'   => '',
      'stop'              => 'SIGTERM',
      'thread-backtraces' => nil,
      'worker-count-down' => 'SIGTTOU',
      'worker-count-up'   => 'SIGTTIN'
    }.freeze

    # commands that cannot be used in a request
    NO_REQ_COMMANDS = %w[info reopen-log worker-count-down worker-count-up].freeze

    # @version 5.0.0
    PRINTABLE_COMMANDS = %w[gc-stats stats thread-backtraces].freeze

    # Commands that only read information and never change the state of the
    # server.  These are sent without a running-identity check, since sending
    # them to a newer generation occupying the same control address is
    # harmless.  Every other request command is stateful and is only sent
    # after the peer's running identity has been verified.
    READ_ONLY_REQUEST_COMMANDS = %w[gc-stats stats status thread-backtraces].freeze

    def initialize(argv, stdout=STDOUT, stderr=STDERR, env: ENV)
      @state = nil
      @quiet = false
      @pidfile = nil
      @pid = nil
      @control_url = nil
      @control_auth_token = nil
      @control_ca_file = nil
      @config_file = nil
      @command = nil
      # Set from the state file's `running_identity` key.  nil means the
      # state file was written by an older Puma that does not record an
      # identity, in which case identity verification is skipped for
      # backwards compatibility.
      @running_identity = nil
      # Set when the state file records an identity, so verification is
      # required before a stateful request is sent.
      @verify_running_identity = false
      @environment = env['APP_ENV'] || env['RACK_ENV'] || env['RAILS_ENV']

      @argv = argv.dup
      @stdout = stdout
      @stderr = stderr
      @cli_options = {}

      opts = OptionParser.new do |o|
        o.banner = "Usage: pumactl (-p PID | -P pidfile | -S status_file | -C url -T token | -F config.rb) (#{CMD_PATH_SIG_MAP.keys.join("|")})"

        o.on "-S", "--state PATH", "Where the state file to use is" do |arg|
          @state = arg
        end

        o.on "-Q", "--quiet", "Do not display messages" do |arg|
          @quiet = true
        end

        o.on "-P", "--pidfile PATH", "Pid file" do |arg|
          @pidfile = arg
        end

        o.on "-p", "--pid PID", "Pid" do |arg|
          @pid = arg.to_i
        end

        o.on "-C", "--control-url URL", "The bind url to use for the control server" do |arg|
          @control_url = arg
        end

        o.on "-T", "--control-token TOKEN", "The token to use as authentication for the control server" do |arg|
          @control_auth_token = arg
        end

        o.on "--control-ca-file PATH", "CA certificate bundle for the TLS control server" do |arg|
          @control_ca_file = arg
        end

        o.on "-F", "--config-file PATH", "Puma config script" do |arg|
          @config_file = arg
        end

        o.on "-e", "--environment ENVIRONMENT",
          "The environment to run the Rack app on (default development)" do |arg|
          @environment = arg
        end

        o.on_tail("-H", "--help", "Show this message") do
          @stdout.puts o
          exit
        end

        o.on_tail("-V", "--version", "Show version") do
          @stdout.puts Const::PUMA_VERSION
          exit
        end
      end

      opts.order!(argv) { |a| opts.terminate a }
      opts.parse!

      @command = argv.shift

      # check presence of command
      unless @command
        raise "Available commands: #{CMD_PATH_SIG_MAP.keys.join(", ")}"
      end

      unless CMD_PATH_SIG_MAP.key? @command
        raise "Invalid command: #{@command}"
      end

      unless @config_file == '-'
        environment = @environment || 'development'

        if @config_file.nil?
          @config_file = %W(config/puma/#{environment}.rb config/puma.rb).find do |f|
            File.exist?(f)
          end
        end

        if @config_file
          # needed because neither `Puma::CLI` or `Puma::Server` are loaded
          require_relative '../puma'

          require_relative 'configuration'
          require_relative 'log_writer'

          config = Puma::Configuration.new({ config_files: [@config_file] }, {} , env)
          config.clamp

          @state              ||= config.options[:state]
          @control_url        ||= config.options[:control_url]
          @control_auth_token ||= config.options[:control_auth_token]
          @pidfile            ||= config.options[:pidfile]
        end
      end
    rescue => e
      @stdout.puts e.message
      exit 1
    end

    def message(msg)
      @stdout.puts msg unless @quiet
    end

    def prepare_configuration
      if @state
        unless File.exist? @state
          raise "State file not found: #{@state}"
        end

        require_relative 'state_file'

        sf = Puma::StateFile.new
        sf.load @state

        @control_url = sf.control_url
        @control_auth_token = sf.control_auth_token
        @pid = sf.pid
        prepare_running_identity sf
      elsif @pidfile
        # get pid from pid_file
        @pid = File.read(@pidfile, mode: 'rb:UTF-8').to_i
      end
    end

    # Reads the state file's running identity.  A state file written by an
    # older Puma has no `running_identity` key; such a file remains usable,
    # but identity verification cannot be performed, so stateful requests
    # behave exactly as they did before the feature existed.
    def prepare_running_identity(state_file)
      identity = state_file.running_identity
      return unless identity && !identity.to_s.strip.empty?

      @running_identity = identity
      @verify_running_identity = true
    end

    def send_request
      verify_running_identity if identity_verification_required?

      uri = URI.parse @control_url

      server = open_control_socket uri

      if @command == 'status'
        message 'Puma is started'
      else
        url = request_url "/#{@command}"

        server.syswrite "GET #{url} HTTP/1.0\r\n\r\n"

        unless data = server.read
          raise 'Server closed connection before responding'
        end

        response = data.split("\r\n")

        if response.empty?
          raise "Server sent empty response"
        end

        @http, @code, @message = response.first.split(' ',3)

        if @code == '403'
          raise 'Unauthorized access to server (wrong auth token)'
        elsif @code == '404'
          raise "Command error: #{response.last}"
        elsif @code != '200'
          raise "Bad response from server: #{@code}"
        end

        message "Command #{@command} sent success"
        message response.last if PRINTABLE_COMMANDS.include?(@command)
      end
    ensure
      close_control_socket server, uri
    end

    # Identity verification is only possible when the state file recorded a
    # `running_identity` (Puma 8.2+), and only matters for requests that can
    # change server state.  Read-only commands and `status` are safe to send
    # to whichever generation currently owns the address.
    def identity_verification_required?
      @verify_running_identity && !READ_ONLY_REQUEST_COMMANDS.include?(@command)
    end

    # Confirms the server listening on the control address is the same Puma
    # generation that wrote the state file, by comparing its `/identity`
    # response with the identity loaded from the state file.  Raises without
    # sending the stateful command when they differ, so a command can never be
    # delivered to a later generation that took over the same address.
    def verify_running_identity
      body = control_request('/identity')[:body]

      begin
        require 'json'
        peer_identity = JSON.parse(body)['running_identity']
      rescue JSON::ParserError
        raise "Invalid identity response from control server: #{body.inspect}"
      end

      unless peer_identity.is_a?(String) && peer_identity == @running_identity
        raise "Control server identity mismatch: state file expects " \
              "#{@running_identity.inspect}, but the server at " \
              "#{@control_url} returned #{peer_identity.inspect}. The control " \
              "address is likely held by a different Puma generation; " \
              "refusing to send '#{@command}'"
      end

      message "Control server identity verified"
    end

    # Opens its own connection to the control server, sends a GET request for
    # `path`, and returns `{ code:, body: }`.  The identity pre-check uses a
    # separate connection so the actual stateful command is only written after
    # the peer has been confirmed.
    def control_request(path)
      uri = URI.parse @control_url
      server = open_control_socket uri

      server.syswrite "GET #{request_url(path)} HTTP/1.0\r\n\r\n"

      data = server.read or raise 'Server closed connection before responding'
      response = data.split("\r\n")
      raise "Server sent empty response" if response.empty?

      _http, code, _message = response.first.split(' ', 3)

      if code == '403'
        raise 'Unauthorized access to server (wrong auth token)'
      elsif code == '404'
        # An older Puma that does not serve `/identity` cannot be verified.
        # Only reachable when a new state file points at an older server, so
        # fail closed rather than guessing at the peer's generation.
        raise "Control server does not support identity verification " \
              "(no /identity endpoint); refusing to send a stateful command"
      elsif code != '200'
        raise "Bad response from server: #{code}"
      end

      { code: code, body: response.last.to_s }
    ensure
      close_control_socket server, uri
    end

    def request_url(path)
      return path unless @control_auth_token

      "#{path}?token=#{@control_auth_token}"
    end

    def open_control_socket(uri)
      host = uri.host

      case uri.scheme
      when 'ssl'
        require 'openssl'
        host = host[1..-2] if host&.start_with? '['
        ctx = OpenSSL::SSL::SSLContext.new
        ctx.ca_file = @control_ca_file if @control_ca_file
        ctx.set_params
        ssl = OpenSSL::SSL::SSLSocket.new(TCPSocket.new(host, uri.port), ctx)
        ssl.tap { |socket| socket.sync_close = true }
          .tap { |socket| socket.hostname = host }
          .tap(&:connect)
          .tap { |socket| socket.post_connection_check host }
      when 'tcp'
        host = host[1..-2] if host&.start_with? '['
        TCPSocket.new host, uri.port
      when 'unix'
        # check for abstract UNIXSocket
        UNIXSocket.new(@control_url.start_with?('unix://@') ?
          "\0#{host}#{uri.path}" : "#{host}#{uri.path}")
      else
        raise "Invalid scheme: #{uri.scheme}"
      end
    end

    def close_control_socket(server, uri)
      return unless server
      if uri.scheme == 'ssl'
        server.sysclose
      else
        server.close unless server.closed?
      end
    end

    def send_signal
      unless @pid
        raise 'Neither pid nor control url available'
      end

      begin
        sig = CMD_PATH_SIG_MAP[@command]

        if sig.nil?
          @stdout.puts "'#{@command}' not available via pid only"
          @stdout.flush unless @stdout.sync
          return
        elsif sig.start_with? 'SIG'
          if Signal.list.key? sig.delete_prefix('SIG')
            Process.kill sig, @pid
          else
            raise "Signal '#{sig}' not available'"
          end
        elsif @command == 'status'
          begin
            Process.kill 0, @pid
            @stdout.puts 'Puma is started'
            @stdout.flush unless @stdout.sync
          rescue Errno::ESRCH
            raise 'Puma is not running'
          end
          return
        end
      rescue SystemCallError
        if @command == 'restart'
          start
        else
          raise "No pid '#{@pid}' found"
        end
      end

      message "Command #{@command} sent success"
    end

    def run
      return start if @command == 'start'
      prepare_configuration

      if Puma.windows? || @control_url && !NO_REQ_COMMANDS.include?(@command)
        send_request
      else
        send_signal
      end

    rescue => e
      message e.message
      exit 1
    end

    private
    def start
      require_relative 'cli'

      run_args = []

      run_args += ["-S", @state]  if @state
      run_args += ["-q"] if @quiet
      run_args += ["--pidfile", @pidfile] if @pidfile
      run_args += ["--control-url", @control_url] if @control_url
      run_args += ["--control-token", @control_auth_token] if @control_auth_token
      run_args += ["-C", @config_file] if @config_file
      run_args += ["-e", @environment] if @environment

      log_writer = Puma::LogWriter.new(@stdout, @stderr)

      # replace $0 because puma use it to generate restart command
      puma_cmd = $0.gsub(/pumactl$/, 'puma')
      $0 = puma_cmd if File.exist?(puma_cmd)

      cli = Puma::CLI.new run_args, log_writer
      cli.run
    end
  end
end
