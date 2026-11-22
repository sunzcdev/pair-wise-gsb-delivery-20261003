# frozen_string_literal: true

require_relative "helper"
require_relative "helpers/ssl"
require_relative "helpers/tmp_path"

require 'pathname'
require 'puma/control_cli'
require 'json'

# A minimal stand-in for Puma's control/status app over TCP.  It records
# every request path it receives and answers `/identity` with the running
# identity it was constructed with, while stateful commands get the same
# `{ "status": "ok" }` body the real app returns.  Two of these can be bound
# to the same address in sequence to simulate a generation taking over a
# control address after the previous one exited.
class ControlStatusStub
  attr_reader :requests, :identity

  # Pass `port:` to bind a fixed address, which allows a second stub to take
  # over the exact same port after the first is shut down.  SO_REUSEADDR is
  # enabled so the address can be rebound immediately.
  def initialize(identity, port: 0)
    @identity = identity
    @requests = []
    # Build the listener manually so SO_REUSEADDR is set before binding,
    # allowing the address to be rebound by a later generation.
    @server = Socket.new(Socket::AF_INET, Socket::SOCK_STREAM, 0)
    @server.setsockopt(Socket::SOL_SOCKET, Socket::SO_REUSEADDR, true)
    @server.bind(Addrinfo.tcp('127.0.0.1', port))
    @server.listen(Socket::SOMAXCONN)
    @thread = Thread.new { accept_loop }
  end

  def port
    @server.local_address.ip_port
  end

  def control_url
    "tcp://127.0.0.1:#{port}/"
  end

  def shutdown
    @server.close unless @server.closed?
    @thread.join
  end

  private

  def accept_loop
    loop do
      socket, = @server.accept
      request_line = socket.gets
      break unless request_line
      path = request_line.split[1]
      @requests << path
      socket.write response_for(path)
      socket.close
    end
  rescue IOError, Errno::EBADF, Errno::ENOTSOCK
  end

  def response_for(path)
    command = path[/\/([^?]+)/, 1]
    body =
      if command == 'identity'
        JSON.generate('running_identity' => @identity)
      else
        '{ "status": "ok" }'
      end
    "HTTP/1.0 200 OK\r\ncontent-type: application/json\r\n#{body}\r\n"
  end
end

class TestPumaControlCli < PumaTest
  include SSLHelper
  include TmpPath

  def setup
    # use a pipe to get info across thread boundary
    @wait, @ready = IO.pipe
  end

  def wait_booted(log: nil)
    @server_log = +''
    begin
      sleep 0.01 until @wait.wait_readable(0.01)
      line = @wait.gets
      STDOUT.syswrite "#{line}\n" if log
      @server_log << line
    end until line&.include?('Use Ctrl-C to stop')
  end

  def teardown
    @wait.close
    @ready.close unless @ready.closed?
  end

  def with_config_file(path_to_config, port)
    path = Pathname.new(path_to_config)
    Dir.mktmpdir do |tmp_dir|
      Dir.chdir(tmp_dir) do
        FileUtils.mkdir_p(path.dirname)
        File.open(path, "w") { |f| f << "port #{port}" }
        yield
      end
    end
  end

  def test_blank_command
    assert_system_exit_with_cli_output [], "Available commands: #{Puma::ControlCLI::CMD_PATH_SIG_MAP.keys.join(", ")}"
  end

  def test_invalid_command
    assert_system_exit_with_cli_output ['an-invalid-command'], 'Invalid command: an-invalid-command'
  end

  def test_config_file
    control_cli = Puma::ControlCLI.new ["--config-file", "test/config/state_file_testing_config.rb", "halt"]
    assert_equal "t3-pid", control_cli.instance_variable_get(:@pidfile)
  ensure
    File.unlink "t3-pid" if File.file? "t3-pid"
  end

  def test_app_env_without_environment
    env = { 'APP_ENV' => 'test' }
    control_cli = Puma::ControlCLI.new ['halt'], env: env
    assert_equal 'test', control_cli.instance_variable_get(:@environment)
  end

  def test_rack_env_without_environment
    env = { "RACK_ENV" => "test" }
    control_cli = Puma::ControlCLI.new ["halt"], env: env
    assert_equal "test", control_cli.instance_variable_get(:@environment)
  end

  def test_app_env_precedence
    env = { 'APP_ENV' => nil, 'RACK_ENV' => nil, 'RAILS_ENV' => 'production' }
    control_cli = Puma::ControlCLI.new ['halt'], env: env
    assert_equal 'production', control_cli.instance_variable_get(:@environment)

    env = { 'APP_ENV' => nil, 'RACK_ENV' => 'test', 'RAILS_ENV' => 'production' }
    control_cli = Puma::ControlCLI.new ['halt'], env: env
    assert_equal 'test', control_cli.instance_variable_get(:@environment)

    env = { 'APP_ENV' => 'development', 'RACK_ENV' => 'test', 'RAILS_ENV' => 'production' }
    control_cli = Puma::ControlCLI.new ['halt'], env: env
    assert_equal 'development', control_cli.instance_variable_get(:@environment)

    control_cli = Puma::ControlCLI.new ['-e', 'test', 'halt'], env: env
    assert_equal 'test', control_cli.instance_variable_get(:@environment)
  end

  def test_environment_without_app_env
    env = { 'APP_ENV' => nil, 'RACK_ENV' => nil, 'RAILS_ENV' => nil }

    control_cli = Puma::ControlCLI.new ['halt'], env: env
    assert_nil control_cli.instance_variable_get(:@environment)

    control_cli = Puma::ControlCLI.new ['-e', 'test', 'halt'], env: env
    assert_equal 'test', control_cli.instance_variable_get(:@environment)
  end

  def test_environment_without_rack_env
    env = { "RACK_ENV" => nil, 'RAILS_ENV' => nil }

    control_cli = Puma::ControlCLI.new ["halt"], env: env
    assert_nil control_cli.instance_variable_get(:@environment)

    control_cli = Puma::ControlCLI.new ["-e", "test", "halt"]
    assert_equal "test", control_cli.instance_variable_get(:@environment)
  end

  def test_environment_with_rack_env
    env = { "RACK_ENV" => "production" }

    control_cli = Puma::ControlCLI.new ["halt"], env: env
    assert_equal "production", control_cli.instance_variable_get(:@environment)

    control_cli = Puma::ControlCLI.new ["-e", "test", "halt"], env: env
    assert_equal "test", control_cli.instance_variable_get(:@environment)
  end

  def test_environment_specific_config_file_exist
    port = UniquePort.call
    puma_config_file = "config/puma.rb"
    production_config_file = "config/puma/production.rb"

    env = { "RACK_ENV" => nil }

    with_config_file(puma_config_file, port) do
      control_cli = Puma::ControlCLI.new ["-e", "production", "halt"], env: env
      assert_equal puma_config_file, control_cli.instance_variable_get(:@config_file)
    end

    with_config_file(production_config_file, port) do
      control_cli = Puma::ControlCLI.new ["-e", "production", "halt"], env: env
      assert_equal production_config_file, control_cli.instance_variable_get(:@config_file)
    end
  end

  def test_default_config_file_exist
    port = UniquePort.call
    puma_config_file = "config/puma.rb"
    development_config_file = "config/puma/development.rb"

    env = { "RACK_ENV" => nil, 'RAILS_ENV' => nil }

    with_config_file(puma_config_file, port) do
      control_cli = Puma::ControlCLI.new ["halt"], env: env
      assert_equal puma_config_file, control_cli.instance_variable_get(:@config_file)
    end

    with_config_file(development_config_file, port) do
      control_cli = Puma::ControlCLI.new ["halt"], env: env
      assert_equal development_config_file, control_cli.instance_variable_get(:@config_file)
    end
  end

  def test_control_no_token
    opts = [
      "--config-file", "test/config/control_no_token.rb",
      "start"
    ]

    control_cli = Puma::ControlCLI.new opts, @ready, @ready
    assert_equal 'none', control_cli.instance_variable_get(:@control_auth_token)
  end

  def test_control_url_and_status
    host = "localhost"
    port = UniquePort.call
    url = "tcp://#{host}:#{port}/"

    opts = [
      "--control-url", url,
      "--control-token", "ctrl",
      "--config-file", "test/config/app.rb"
    ]

    control_cli = Puma::ControlCLI.new (opts + ["start"]), @ready, @ready
    t = Thread.new do
      control_cli.run
    end

    wait_booted # read server log

    bind_port = @server_log[/Listening on http:.+:(\d+)$/, 1].to_i
    s = TCPSocket.new host, bind_port
    s << "GET / HTTP/1.0\r\n\r\n"
    body = s.read
    assert_includes body, "200 OK"
    assert_includes body, "embedded app"

    assert_command_cli_output opts + ["status"], "Puma is started"
    assert_command_cli_output opts + ["stop"], "Command stop sent success"

    assert_kind_of Thread, t.join, "server didn't stop"
  end

  # This checks that a 'signal only' command is sent
  # they are defined by the `Puma::ControlCLI::NO_REQ_COMMANDS` array
  # test is skipped unless NO_REQ_COMMANDS is defined
  def test_control_url_with_signal_only_cmd
    skip_if :windows
    skip unless defined? Puma::ControlCLI::NO_REQ_COMMANDS
    host = "127.0.0.1"
    port = UniquePort.call
    url = "tcp://#{host}:#{port}/"

    opts = [
      "--control-url", url,
      "--control-token", "ctrl",
      "--config-file", "test/config/app.rb",
      "--pid", "1234"
    ]
    cmd = Puma::ControlCLI::NO_REQ_COMMANDS.first
    log = +''
    control_cli = Puma::ControlCLI.new (opts + [cmd]), @ready, @ready

    def control_cli.send_signal
      message "send_signal #{@command}\n"
    end
    def control_cli.send_request
      message "send_request #{@command}\n"
    end

    control_cli.run
    @ready.close

    log = @wait.read

    assert_includes log, "send_signal #{cmd}"
    refute_includes log, 'send_request'
  end

  def control_ssl_certificate(ip)
    require 'openssl'
    key = OpenSSL::PKey::RSA.generate(2048)
    cert = OpenSSL::X509::Certificate.new
    cert.version = 2
    cert.serial = 1
    cert.subject = cert.issuer = OpenSSL::X509::Name.parse('/CN=localhost')
    cert.public_key = key.public_key
    cert.not_before = Time.now - 60
    cert.not_after = Time.now + 3600
    factory = OpenSSL::X509::ExtensionFactory.new
    factory.subject_certificate = factory.issuer_certificate = cert
    cert.add_extension factory.create_extension('subjectAltName', "IP:#{ip}")
    cert.add_extension factory.create_extension('basicConstraints', 'CA:FALSE', true)
    cert.sign(key, OpenSSL::Digest::SHA256.new)
    [key, cert]
  end

  def control_ssl(host)
    skip_unless :ssl
    ip = host&.start_with?('[') ? host[1..-2] : host
    port = UniquePort.call(ip)

    Dir.mktmpdir do |dir|
      key, cert = control_ssl_certificate(ip)
      cert_path = File.join(dir, 'cert.pem')
      File.write(cert_path, cert.to_pem)
      query = if Puma.jruby?
        keystore = File.join(dir, 'control.p12')
        File.binwrite(keystore, OpenSSL::PKCS12.create('password', 'puma', key, cert).to_der)
        "keystore=#{keystore}&keystore-pass=password&keystore-type=pkcs12"
      else
        key_path = File.join(dir, 'key.pem')
        File.write(key_path, key.to_pem)
        "key=#{key_path}&cert=#{cert_path}"
      end

      url = "ssl://#{host}:#{port}?#{query}"
      opts = [
        "--control-url", url,
        "--control-token", "ctrl",
        "--control-ca-file", cert_path,
        "--config-file", "test/config/app.rb"
      ]

      control_cli = Puma::ControlCLI.new (opts + ["start"]), @ready, @ready
      t = Thread.new { control_cli.run }

      wait_booted

      assert_command_cli_output opts + ["status"], "Puma is started"
      assert_command_cli_output opts + ["stop"], "Command stop sent success"

      assert_kind_of Thread, t.join, "server didn't stop"
    end
  end


  def test_control_ssl_ipv4
    skip_unless :ssl
    control_ssl '127.0.0.1'
  end

  def test_control_ssl_ipv6
    skip_unless :ssl
    control_ssl '[::1]'
  end

  def assert_control_ssl_rejected(cert_ip, trusted:)
    key, cert = control_ssl_certificate(cert_ip)
    ctx = OpenSSL::SSL::SSLContext.new
    ctx.cert = cert
    ctx.key = key
    server = TCPServer.new('127.0.0.1', 0)
    thread = Thread.new do
      socket = OpenSSL::SSL::SSLSocket.new(server.accept, ctx)
      socket.sync_close = true
      socket.accept
      socket.gets
    rescue OpenSSL::SSL::SSLError, EOFError, Errno::ECONNRESET
      nil
    ensure
      socket&.sysclose
    end

    Dir.mktmpdir do |dir|
      cert_path = File.join(dir, 'cert.pem')
      File.write(cert_path, cert.to_pem)
      opts = ['--control-url', "ssl://127.0.0.1:#{server.addr[1]}", '--control-token', 'secret']
      opts.concat(['--control-ca-file', cert_path]) if trusted

      assert_raises(OpenSSL::SSL::SSLError) do
        Puma::ControlCLI.new(opts + ['stats'], @ready, @ready).send_request
      end
      assert_kind_of Thread, thread.join(5), 'control server did not finish'
      assert_nil thread.value
    end
  ensure
    thread&.kill&.join
    server&.close
  end

  def test_control_ssl_rejects_untrusted_certificate
    skip_unless :ssl
    assert_control_ssl_rejected('127.0.0.1', trusted: false)
  end

  def test_control_ssl_rejects_wrong_host
    skip_unless :ssl
    assert_control_ssl_rejected('127.0.0.2', trusted: true)
  end

  def test_control_aunix
    skip_unless :aunix

    url = "unix://@test_control_aunix.unix"

    opts = [
      "--control-url", url,
      "--control-token", "ctrl",
      "--config-file", "test/config/app.rb"
    ]

    control_cli = Puma::ControlCLI.new (opts + ["start"]), @ready, @ready
    t = Thread.new do
      control_cli.run
    end

    wait_booted

    assert_command_cli_output opts + ["status"], "Puma is started"
    assert_command_cli_output opts + ["stop"], "Command stop sent success"

    assert_kind_of Thread, t.join, "server didn't stop"
  end

  def test_control_ipv6
    port = UniquePort.call '::1'
    url = "tcp://[::1]:#{port}"

    opts = [
      "--control-url", url,
      "--control-token", "ctrl",
      "--config-file", "test/config/app.rb"
    ]

    control_cli = Puma::ControlCLI.new (opts + ["start"]), @ready, @ready
    t = Thread.new do
      control_cli.run
    end

    wait_booted

    assert_command_cli_output opts + ["status"], "Puma is started"
    assert_command_cli_output opts + ["stop"], "Command stop sent success"

    assert_kind_of Thread, t.join, "server didn't stop"
  end

  # A stateful command sent while the original generation still owns the
  # control address must first verify `/identity`, then deliver the command.
  def test_running_identity_verified_before_stateful_command
    identity = 'a1b2c3d4e5f60718293a4b5c6d7e8f90'
    stub = ControlStatusStub.new(identity)
    state_path = write_state_file(stub.control_url, identity)

    out = run_pumactl ['-S', state_path, 'stop']

    assert_includes out, 'Control server identity verified'
    assert_includes out, 'Command stop sent success'
    assert_equal ['/identity?token=ctrl', '/stop?token=ctrl'], stub.requests
  ensure
    stub&.shutdown
  end

  # Read-only commands must not be gated on the running identity, since
  # querying a newer generation on the same address is harmless.
  def test_read_only_command_skips_identity_check
    identity = 'a1b2c3d4e5f60718293a4b5c6d7e8f90'
    stub = ControlStatusStub.new(identity)
    state_path = write_state_file(stub.control_url, identity)

    out = run_pumactl ['-S', state_path, 'stats']

    assert_includes out, 'Command stats sent success'
    refute_includes out, 'identity verified'
    assert_equal ['/stats?token=ctrl'], stub.requests
  ensure
    stub&.shutdown
  end

  def test_status_command_skips_identity_check
    identity = 'a1b2c3d4e5f60718293a4b5c6d7e8f90'
    stub = ControlStatusStub.new(identity)
    state_path = write_state_file(stub.control_url, identity)

    out = run_pumactl ['-S', state_path, 'status']

    assert_includes out, 'Puma is started'
    assert_empty stub.requests
  ensure
    stub&.shutdown
  end

  # The central scenario: generation one exits and generation two takes over
  # the same control address.  pumactl must detect the identity mismatch and
  # refuse to deliver the stateful command to the new generation.
  def test_stateful_command_refused_after_generation_takeover
    identity_one = 'a1b2c3d4e5f60718293a4b5c6d7e8f90'
    identity_two = 'fedcba98765432100123456789abcdef'

    stub_one = ControlStatusStub.new(identity_one)
    port = stub_one.port
    state_path = write_state_file(stub_one.control_url, identity_one)

    # generation one receives and accepts a command before it exits
    out = run_pumactl ['-S', state_path, 'stop']
    assert_includes out, 'Command stop sent success'

    stub_one.shutdown

    # generation two binds the same address with a different identity
    stub_two = ControlStatusStub.new(identity_two, port: port)
    assert_equal port, stub_two.port, 'second stub must reuse the same address'

    out = run_pumactl ['-S', state_path, 'halt'], expect_failure: true

    assert_match(/identity mismatch/, out)
    # it only ever answered the identity probe; halt must not be delivered
    assert_equal ['/identity?token=ctrl'], stub_two.requests
  ensure
    stub_one&.shutdown
    stub_two&.shutdown
  end

  # A state file written by an older Puma has no running_identity.  It must
  # keep working exactly as before: no identity probe, command delivered.
  def test_legacy_state_file_without_identity_still_works
    stub = ControlStatusStub.new('fedcba98765432100123456789abcdef')
    state_path = write_state_file(stub.control_url, nil)

    out = run_pumactl ['-S', state_path, 'stop']

    assert_includes out, 'Command stop sent success'
    refute_match(/identity/i, out)
    assert_equal ['/stop?token=ctrl'], stub.requests
  ensure
    stub&.shutdown
  end

  # A new state file pointing at a server that lacks the /identity endpoint
  # fails closed instead of blindly sending the stateful command.
  def test_missing_identity_endpoint_fails_closed
    identity = 'a1b2c3d4e5f60718293a4b5c6d7e8f90'
    stub = ControlStatusStub.new(identity)
    stub.define_singleton_method(:response_for) do |_path|
      "HTTP/1.0 404 Not Found\r\nconnection: close\r\n\r\n"
    end
    state_path = write_state_file(stub.control_url, identity)

    out = run_pumactl ['-S', state_path, 'stop'], expect_failure: true

    assert_match(/identity verification/, out)
    assert_equal ['/identity?token=ctrl'], stub.requests
  ensure
    stub&.shutdown
  end

  private

  def write_state_file(control_url, identity)
    path = tmp_path('.state')
    contents = +"---\n"
    contents << "pid: #{Process.pid}\n"
    contents << "control_url: #{control_url}\n"
    contents << "control_auth_token: ctrl\n"
    contents << "running_identity: #{identity}\n" if identity
    File.write path, contents
    path
  end

  # Runs pumactl against a pipe (matching the existing helpers), returning
  # the captured output.  A failing run exits with status 1, which is the
  # expected outcome when `expect_failure:` is set.
  def run_pumactl(options, expect_failure: false)
    rd, wr = IO.pipe
    if expect_failure
      exit_status = assert_raises(SystemExit) do
        Puma::ControlCLI.new(options, wr, wr).run
      end.status
      assert_equal 1, exit_status
    else
      Puma::ControlCLI.new(options, wr, wr).run
    end
    wr.close
    rd.read
  ensure
    rd&.close
  end

  def assert_command_cli_output(options, expected_out)
    @rd, @wr = IO.pipe
    cmd = Puma::ControlCLI.new(options, @wr, @wr)
    begin
      cmd.run
    rescue SystemExit
    end
    @wr.close
    if String === expected_out
      assert_includes @rd.read, expected_out
    else
      assert_match expected_out, @rd.read
    end
  ensure
    @rd.close
  end

  def assert_system_exit_with_cli_output(options, expected_out)
    @rd, @wr = IO.pipe

    response = assert_raises(SystemExit) do
      Puma::ControlCLI.new(options, @wr, @wr).run
    end
    @wr.close

    assert_equal(response.status, 1)
    if String === expected_out
      assert_includes @rd.read, expected_out
    else
      assert_match expected_out, @rd.read
    end
  ensure
    @rd.close
  end
end
