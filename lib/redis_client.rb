# frozen_string_literal: true

require "redis_client/version"
require "redis_client/command_builder"
require "redis_client/url_config"
require "redis_client/config"
require "redis_client/pid_cache"
require "redis_client/sentinel_config"
require "redis_client/middlewares"
require "redis_client/observability"

class RedisClient
  @driver_definitions = {}
  @drivers = {}

  @default_driver = nil

  class << self
    def register_driver(name, &block)
      @driver_definitions[name] = block
    end

    def driver(name)
      return name if name.is_a?(Class)

      name = name.to_sym
      unless @driver_definitions.key?(name)
        raise ArgumentError, "Unknown driver #{name.inspect}, expected one of: `#{@driver_definitions.keys.inspect}`"
      end

      @drivers[name] ||= @driver_definitions[name]&.call
    end

    def default_driver
      unless @default_driver
        @driver_definitions.each_key do |name|
          if @default_driver = driver(name)
            break
          end
        rescue LoadError
        end
      end
      @default_driver
    end

    def default_driver=(name)
      @default_driver = driver(name)
    end

    def now
      Process.clock_gettime(Process::CLOCK_MONOTONIC)
    end

    def now_ms
      Process.clock_gettime(Process::CLOCK_MONOTONIC, :float_millisecond)
    end
  end

  register_driver :ruby do
    require "redis_client/ruby_connection"
    RubyConnection
  end

  module Common
    attr_reader :config, :id, :nodes
    attr_accessor :connect_timeout, :read_timeout, :write_timeout

    def initialize(
      config,
      id: config.id,
      connect_timeout: config.connect_timeout,
      read_timeout: config.read_timeout,
      write_timeout: config.write_timeout
    )
      @config = config
      @id = id&.to_s
      @connect_timeout = connect_timeout
      @read_timeout = read_timeout
      @write_timeout = write_timeout
      @command_builder = config.command_builder
      @pid = PIDCache.pid
      @nodes = [self].freeze
    end

    def timeout=(timeout)
      @connect_timeout = @read_timeout = @write_timeout = timeout
    end

    def node_for(_key)
      self
    end

    def nodes_for(*keys)
      keys.flatten!
      { self => keys }
    end
  end

  module HasConfig
    attr_reader :config

    def _set_config(config)
      @config = config
    end

    def message
      return super unless config&.resolved?

      "#{super} (#{config.server_url})"
    end
  end

  module Retriable
    def _set_retry_attempt(retry_attempt)
      @retry_attempt = retry_attempt
    end

    def retry_attempt
      @retry_attempt || 0
    end

    def retriable?
      !!@retry_attempt
    end

    def final?
      !@retry_attempt
    end
  end

  module Final
    def _set_retry_attempt(_retry_attempt)
    end

    def retry_attempt
      0
    end

    def retriable?
      false
    end

    def final?
      true
    end
  end

  class Error < StandardError
    include HasConfig
    include Retriable

    attr_reader :next_error

    def self.with_config(message, config = nil)
      error = new(message)
      error._set_config(config)
      error
    end

    def _set_next_error(error) # :nodoc:
      @next_error = error
    end
  end

  ProtocolError = Class.new(Error)
  UnsupportedServer = Class.new(Error)

  ConnectionError = Class.new(Error)
  CannotConnectError = Class.new(ConnectionError)

  FailoverError = Class.new(ConnectionError)

  TimeoutError = Class.new(ConnectionError)
  ReadTimeoutError = Class.new(TimeoutError)
  WriteTimeoutError = Class.new(TimeoutError)
  CheckoutTimeoutError = Class.new(TimeoutError)

  module HasCommand
    attr_reader :command

    def _set_command(command)
      @command = command
    end
  end

  module HasCode
    attr_reader :code

    def initialize(message = nil, code = nil)
      super(message)
      @code = code
    end
  end

  class CommandError < Error
    include HasCommand
    include HasCode
    include Final

    class << self
      def parse(error_message)
        code = if error_message.start_with?("ERR Error running script")
          # On older redis servers script errors are nested.
          # So we need to parse some more.
          if (match = error_message.match(/:\s-([A-Z]+) /))
            match[1]
          end
        end
        code ||= error_message.split(' ', 2).first
        klass = ERRORS.fetch(code, self)
        klass.new(error_message.strip, code.freeze)
      end
    end
  end

  AuthenticationError = Class.new(CommandError)
  PermissionError = Class.new(CommandError)
  WrongTypeError = Class.new(CommandError)
  OutOfMemoryError = Class.new(CommandError)
  NoScriptError = Class.new(CommandError)

  ReadOnlyError = Class.new(ConnectionError)
  ReadOnlyError.include(HasCommand)
  ReadOnlyError.include(HasCode)

  MasterDownError = Class.new(ConnectionError)
  MasterDownError.include(HasCommand)
  MasterDownError.include(HasCode)

  CommandError::ERRORS = {
    "WRONGPASS" => AuthenticationError,
    "NOPERM" => PermissionError,
    "READONLY" => ReadOnlyError,
    "MASTERDOWN" => MasterDownError,
    "WRONGTYPE" => WrongTypeError,
    "OOM" => OutOfMemoryError,
    "NOSCRIPT" => NoScriptError,
  }.freeze

  class << self
    def config(**kwargs)
      Config.new(client_implementation: self, **kwargs)
    end

    def sentinel(**kwargs)
      SentinelConfig.new(client_implementation: self, **kwargs)
    end

    def ring(*clients, **options)
      clients.flatten!
      require "redis_client/hash_ring" unless defined?(HashRing)
      HashRing.new(clients, **options)
    end

    def new(arg = nil, **kwargs)
      if arg.is_a?(Config::Common)
        super
      else
        super(config(**(arg || {}), **kwargs))
      end
    end

    def register(middleware)
      Middlewares.include(middleware)
    end
  end

  include Common

  def initialize(config, **)
    super
    @middlewares = config.middlewares_stack.new(self)
    @instrumenter = config.observer ? Observability::Instrumenter.new(config) : nil
    @raw_connection = nil
    @disable_reconnection = false
    @retry_attempt = nil
    @attempt_span_suppressed = false
  end

  def inspect
    id_string = " id=#{id}" if id
    "#<#{self.class.name} #{config.server_url}#{id_string}>"
  end

  def server_url
    config.server_url
  end

  def id
    config.id
  end

  def timeout
    config.read_timeout
  end

  def idle_timeout
    config.idle_timeout
  end

  def db
    config.db
  end

  def host
    config.host unless config.path
  end

  def port
    config.port unless config.path
  end

  def path
    config.path
  end

  def username
    config.username
  end

  def password
    config.password
  end

  def size
    1
  end

  def with(_options = nil)
    yield self
  end
  alias_method :then, :with

  def timeout=(timeout)
    super
    @raw_connection&.read_timeout = timeout
    @raw_connection&.write_timeout = timeout
  end

  def read_timeout=(timeout)
    super
    @raw_connection&.read_timeout = timeout
  end

  def write_timeout=(timeout)
    super
    @raw_connection&.write_timeout = timeout
  end

  def pubsub
    sub = PubSub.new(ensure_connected, @command_builder)
    @raw_connection = nil
    sub
  end

  def measure_round_trip_delay
    ensure_connected do |connection|
      @middlewares.call(["PING"], config) do
        connection.measure_round_trip_delay
      end
    end
  end

  def call(*command, **kwargs)
    command = @command_builder.generate(command, kwargs)
    result = observe_command(:call, command) do
      ensure_connected do |connection|
        @middlewares.call(command, config) do
          connection.call(command, nil)
        end
      end
    end

    if block_given?
      yield result
    else
      result
    end
  end

  def call_v(command)
    command = @command_builder.generate(command)
    result = observe_command(:call, command) do
      ensure_connected do |connection|
        @middlewares.call(command, config) do
          connection.call(command, nil)
        end
      end
    end

    if block_given?
      yield result
    else
      result
    end
  end

  def call_once(*command, **kwargs)
    command = @command_builder.generate(command, kwargs)
    result = observe_command(:call, command, retryable: false) do
      ensure_connected(retryable: false) do |connection|
        @middlewares.call(command, config) do
          connection.call(command, nil)
        end
      end
    end

    if block_given?
      yield result
    else
      result
    end
  end

  def call_once_v(command)
    command = @command_builder.generate(command)
    result = observe_command(:call, command, retryable: false) do
      ensure_connected(retryable: false) do |connection|
        @middlewares.call(command, config) do
          connection.call(command, nil)
        end
      end
    end

    if block_given?
      yield result
    else
      result
    end
  end

  def blocking_call(timeout, *command, **kwargs)
    command = @command_builder.generate(command, kwargs)
    error = nil
    result = observe_command(:blocking_call, command) do
      ensure_connected do |connection|
        @middlewares.call(command, config) do
          connection.call(command, timeout)
        end
      rescue ReadTimeoutError => error
        break
      end
    end

    if error
      raise error
    elsif block_given?
      yield result
    else
      result
    end
  end

  def blocking_call_v(timeout, command)
    command = @command_builder.generate(command)
    error = nil
    result = observe_command(:blocking_call, command) do
      ensure_connected do |connection|
        @middlewares.call(command, config) do
          connection.call(command, timeout)
        end
      rescue ReadTimeoutError => error
        break
      end
    end

    if error
      raise error
    elsif block_given?
      yield result
    else
      result
    end
  end

  def scan(*args, **kwargs, &block)
    unless block_given?
      return to_enum(__callee__, *args, **kwargs)
    end

    args = @command_builder.generate(["SCAN", 0] + args, kwargs)
    scan_list(1, args, &block)
  end

  def sscan(key, *args, **kwargs, &block)
    unless block_given?
      return to_enum(__callee__, key, *args, **kwargs)
    end

    args = @command_builder.generate(["SSCAN", key, 0] + args, kwargs)
    scan_list(2, args, &block)
  end

  def hscan(key, *args, **kwargs, &block)
    unless block_given?
      return to_enum(__callee__, key, *args, **kwargs)
    end

    args = @command_builder.generate(["HSCAN", key, 0] + args, kwargs)
    scan_pairs(2, args, &block)
  end

  def zscan(key, *args, **kwargs, &block)
    unless block_given?
      return to_enum(__callee__, key, *args, **kwargs)
    end

    args = @command_builder.generate(["ZSCAN", key, 0] + args, kwargs)
    scan_pairs(2, args, &block)
  end

  def connected?
    @raw_connection&.revalidate
  end

  def close
    @raw_connection&.close
    self
  end

  def disable_reconnection(&block)
    ensure_connected(retryable: false, &block)
  end

  def pipelined(exception: true)
    pipeline = Pipeline.new(@command_builder)
    yield pipeline

    if pipeline._size == 0
      []
    else
      results = observe_command(:pipelined, nil, commands: pipeline._commands,
                                               retryable: pipeline._retryable?) do
        ensure_connected(retryable: pipeline._retryable?) do |connection|
          commands = pipeline._commands
          @middlewares.call_pipelined(commands, config) do
            connection.call_pipelined(commands, pipeline._timeouts, exception: exception)
          end
        end
      end

      pipeline._coerce!(results)
    end
  end

  def multi(watch: nil, &block)
    transaction = nil

    results = if watch
      # WATCH is stateful, so we can't reconnect if it's used, the whole transaction
      # has to be redone.
      ensure_connected(retryable: false) do |connection|
        call("WATCH", *watch)
        begin
          if transaction = build_transaction(&block)
            commands = transaction._commands
            results = @middlewares.call_pipelined(commands, config) do
              connection.call_pipelined(commands, nil)
            end.last
          else
            call("UNWATCH")
            []
          end
        rescue
          call("UNWATCH") if connected? && watch
          raise
        end
      end
    else
      transaction = build_transaction(&block)
      if transaction._empty?
        []
      else
        observe_command(:multi, nil, commands: transaction._commands,
                                    retryable: transaction._retryable?) do
          ensure_connected(retryable: transaction._retryable?) do |connection|
            commands = transaction._commands
            @middlewares.call_pipelined(commands, config) do
              connection.call_pipelined(commands, nil)
            end.last
          end
        end
      end
    end

    if transaction
      transaction._coerce!(results)
    else
      results
    end
  end

  class PubSub
    def initialize(raw_connection, command_builder)
      @raw_connection = raw_connection
      @command_builder = command_builder
    end

    def call(*command, **kwargs)
      raw_connection.write(@command_builder.generate(command, kwargs))
      nil
    end

    def call_v(command)
      raw_connection.write(@command_builder.generate(command))
      nil
    end

    def close
      @raw_connection&.close
      @raw_connection = nil # PubSub can't just reconnect
      self
    end

    def next_event(timeout = nil)
      unless raw_connection
        raise ConnectionError, "Connection was closed or lost"
      end

      raw_connection.read(timeout)
    rescue ReadTimeoutError
      nil
    end

    private

    attr_reader :raw_connection
  end

  class Multi
    def initialize(command_builder)
      @command_builder = command_builder
      @size = 0
      @commands = []
      @blocks = nil
      @retryable = true
    end

    def call(*command, **kwargs, &block)
      command = @command_builder.generate(command, kwargs)
      (@blocks ||= [])[@commands.size] = block if block_given?
      @commands << command
      nil
    end

    def call_v(command, &block)
      command = @command_builder.generate(command)
      (@blocks ||= [])[@commands.size] = block if block_given?
      @commands << command
      nil
    end

    def call_once(*command, **kwargs, &block)
      command = @command_builder.generate(command, kwargs)
      @retryable = false
      (@blocks ||= [])[@commands.size] = block if block_given?
      @commands << command
      nil
    end

    def call_once_v(command, &block)
      command = @command_builder.generate(command)
      @retryable = false
      (@blocks ||= [])[@commands.size] = block if block_given?
      @commands << command
      nil
    end

    def _commands
      @commands
    end

    def _blocks
      @blocks
    end

    def _size
      @commands.size
    end

    def _empty?
      @commands.size <= 2
    end

    def _timeouts
      nil
    end

    def _retryable?
      @retryable
    end

    def _coerce!(results)
      results&.each_with_index do |result, index|
        if result.is_a?(CommandError)
          result._set_command(@commands[index + 1])
          raise result
        end

        if @blocks && block = @blocks[index + 1]
          results[index] = block.call(result)
        end
      end

      results
    end
  end

  class Pipeline < Multi
    def initialize(_command_builder)
      super
      @timeouts = nil
    end

    def blocking_call(timeout, *command, **kwargs, &block)
      command = @command_builder.generate(command, kwargs)
      @timeouts ||= []
      @timeouts[@commands.size] = timeout
      (@blocks ||= [])[@commands.size] = block if block_given?
      @commands << command
      nil
    end

    def blocking_call_v(timeout, command, &block)
      command = @command_builder.generate(command)
      @timeouts ||= []
      @timeouts[@commands.size] = timeout
      (@blocks ||= [])[@commands.size] = block if block_given?
      @commands << command
      nil
    end

    def _timeouts
      @timeouts
    end

    def _empty?
      @commands.empty?
    end

    def _coerce!(results)
      return results unless results

      @blocks&.each_with_index do |block, index|
        if block
          results[index] = block.call(results[index])
        end
      end

      results
    end
  end

  private

  # Fiber-local key used to stash the active observation context. Using the
  # current thread/fiber keeps concurrent commands on a shared client isolated
  # while remaining invisible to the rest of the object's state. The key is a
  # frozen shared symbol rather than an object-id-derived string, so it does not
  # accumulate entries in Thread.current across client instances.
  OBSERVATION_STORAGE_KEY = :redis_client_observation_context

  # Wrap a logical command (single or pipelined) with the diagnostics layer.
  # Business middlewares are invoked exactly as before, once per physical try
  # inside the block; the observer instead sees one stable command pair and one
  # attempt pair per try. When no observer is configured this is a plain yield.
  #
  # The context is kept in thread/fiber-local storage rather than an instance
  # variable, because a single client may be shared by concurrent threads and
  # each logical command needs its own independent correlation state.
  def observe_command(name, command, commands: nil, retryable: true)
    instrumenter = @instrumenter
    return yield unless instrumenter

    call_id = instrumenter.new_id
    pipeline_id = commands ? instrumenter.new_id : nil
    context = instrumenter.new_context(
      call_id, pipeline_id, name: name, command: command, commands: commands
    )
    context.retryable = retryable
    if retryable
      # Pure query (no sleep), mirroring the scheduling done by
      # Config#retry_connecting? inside the retry loop.
      context.retry_policy = ->(attempt) { !!config.retriable?(attempt) }
    end

    set_observation_context(context)
    begin
      instrumenter.instrument(
        name, command: command, commands: commands, context: context
      ) { yield }
    ensure
      set_observation_context(nil)
    end
  end

  def observation_context
    Thread.current[OBSERVATION_STORAGE_KEY]
  end

  def set_observation_context(context)
    Thread.current[OBSERVATION_STORAGE_KEY] = context
  end

  # Emit the attempt events around one full physical try of the retry loop,
  # including connection acquisition and the prelude. A fresh attempt_id is
  # allocated per try; the parent call_id/pipeline_id stay stable so every try
  # can be correlated back to the single logical command.
  def observe_attempt_body(number)
    context = observation_context
    return yield unless context
    return yield unless @instrumenter
    # The non-retryable branch acquires its connection recursively; suppress
    # that nested span so a call_once command reports exactly one attempt.
    return yield if @attempt_span_suppressed

    context.attempt = number
    context.attempt_id = @instrumenter.new_id
    context.begin_attempt!

    # Bind the context before the block runs: raw_connection/connect inside the
    # block may create a brand new connection, which must already know which
    # observation context its prelude and writes belong to.
    @raw_connection&.observing_context = context

    @instrumenter.instrument_attempt(
      context.name, command: context.command, commands: context.commands,
                    context: context
    ) do
      yield
    end
  ensure
    @raw_connection&.observing_context = nil
  end

  def build_transaction
    transaction = Multi.new(@command_builder)
    transaction.call("MULTI")
    yield transaction
    transaction.call("EXEC")
    transaction
  end

  def scan_list(cursor_index, command, &block)
    cursor = 0
    while cursor != "0"
      command[cursor_index] = cursor
      cursor, elements = call(*command)
      elements.each(&block)
    end
    nil
  end

  def scan_pairs(cursor_index, command)
    cursor = 0
    while cursor != "0"
      command[cursor_index] = cursor
      cursor, elements = call(*command)

      index = 0
      size = elements.size
      while index < size
        yield elements[index], elements[index + 1]
        index += 2
      end
    end
    nil
  end

  def ensure_connected(retryable: true)
    close if !config.inherit_socket && @pid != PIDCache.pid

    if @disable_reconnection
      @raw_connection.retry_attempt = nil
      if block_given?
        yield @raw_connection
      else
        @raw_connection
      end
    elsif retryable
      tries = 0
      connection = nil
      preferred_error = nil
      begin
        @retry_attempt = config.retriable?(tries) ? tries : nil
        # The whole physical attempt - including establishing the connection
        # and running its prelude - lives inside the attempt event boundary, so
        # failures during connect are attributed to the correct attempt.
        observe_attempt_body(tries) do
          # tries > 0 means a previous physical attempt failed and the loop is
          # reconnecting; connect() reads this (via the context) to tag its
          # events accurately. Kept off instance state for thread safety.
          context = observation_context
          context.reconnecting = tries > 0 if context
          connection = raw_connection
          if block_given?
            result = yield connection
            @last_used_at = RedisClient.now
            result
          else
            connection
          end
        end
      rescue ConnectionError, ProtocolError => error
        preferred_error ||= error
        close

        if error.is_a?(CircuitBreaker::OpenCircuitError)
          raise preferred_error
        else
          preferred_error = error
        end

        if !@disable_reconnection && config.retry_connecting?(tries, error)
          tries += 1
          retry
        else
          raise preferred_error
        end
      end
    else
      previous_disable_reconnection = @disable_reconnection
      # Suppress the attempt span that the recursive (retryable) connection
      # acquisition would otherwise open: the single physical execution below
      # is the only business attempt we report for call_once-style calls. The
      # connection itself is still established exactly as before, so error
      # semantics are unchanged.
      connection = with_attempt_span_suppressed { ensure_connected }
      begin
        @disable_reconnection = true
        @raw_connection.retry_attempt = nil
        observe_attempt_body(0) do
          result = yield connection
          @last_used_at = RedisClient.now
          result
        end
      rescue ConnectionError, ProtocolError
        close
        raise
      ensure
        @disable_reconnection = previous_disable_reconnection
      end
    end
  end

  # When active, observe_attempt_body becomes a plain yield instead of opening
  # an attempt span. Used only while the non-retryable branch acquires its
  # connection, to avoid attributing an extra attempt to a call_once command.
  def with_attempt_span_suppressed
    previous = @attempt_span_suppressed
    @attempt_span_suppressed = true
    yield
  ensure
    @attempt_span_suppressed = previous
  end

  def raw_connection
    if @raw_connection.nil? || !@raw_connection.revalidate
      connect
    end

    if config.idle_timeout && (@last_used_at + config.idle_timeout) < RedisClient.now
      @middlewares.call(["PING"], config) do
        @raw_connection.call(["PING"], nil)
      rescue ConnectionError, ProtocolError
        close
        connect
      end
    end

    @raw_connection.retry_attempt = @retry_attempt
    @raw_connection.observing_context = observation_context
    @raw_connection
  end

  # Observe a (re)connection. connect() always runs from raw_connection, which
  # itself runs inside an attempt block, so the active observation context is
  # the current attempt's context and must be reused as-is (same call_id and
  # attempt_id). The fallback only covers exotic out-of-band connections.
  def instrument_connect(reconnecting)
    instrumenter = @instrumenter
    return yield unless instrumenter

    context = observation_context
    unless context
      context = instrumenter.new_context(instrumenter.new_id, nil, name: :connect)
      context.attempt_id = instrumenter.new_id
    end
    if @raw_connection
      @raw_connection.observing_context = context
    end

    instrumenter.instrument_connect(context: context, reconnect: reconnecting) { yield }
  end

  def observe_prelude(commands, results)
    return unless @instrumenter

    context = observation_context
    unless context
      context = @instrumenter.new_context(@instrumenter.new_id, nil, name: :connect)
      context.attempt_id = @instrumenter.new_id
    end
    @instrumenter.observe_prelude(commands, results, context: context)
  end

  # Mirror partial-send evidence from a connection error into the active
  # diagnostics context. Connect errors can be raised after a prelude write, so
  # this keeps the unknown-outcome bookkeeping accurate across reconnections.
  def remember_unknown_outcome(error)
    context = observation_context
    return unless context
    return unless error.is_a?(ConnectionMixin::SentButUnknown)

    context.mark_sent!(error.sent_commands.to_i)
    context.mark_acknowledged!(error.acknowledged_commands.to_i)
    context.last_error = error
  end

  def connect
    @pid = PIDCache.pid

    existing_connection = @raw_connection&.revalidate
    # Prefer the retry-loop flag carried on the observation context (accurate
    # after close and thread-local); fall back to whether a live connection
    # object is being reused.
    context = observation_context
    flagged_reconnect = context ? context.reconnecting? : false
    reconnecting = flagged_reconnect || existing_connection ? true : false
    instrument_connect(reconnecting) do
      if existing_connection
        @middlewares.connect(config) do
          @raw_connection.reconnect
        end
      else
        @raw_connection = @middlewares.connect(config) do
          config.driver.new(
            config,
            connect_timeout: connect_timeout,
            read_timeout: read_timeout,
            write_timeout: write_timeout,
          )
        end
      end
    end
    @last_used_at = RedisClient.now
    @raw_connection.retry_attempt = @retry_attempt

    prelude = config.connection_prelude.dup

    if id
      prelude << ["CLIENT", "SETNAME", id]
    end

    if config.sentinel?
      prelude << ["ROLE"]
    end

    unless prelude.empty?
      results = @middlewares.call_pipelined(prelude, config) do
        @raw_connection.call_pipelined(prelude, nil, exception: false)
      end
      observe_prelude(prelude, results)

      results.each do |result|
        # CLIENT SETINFO is unsupported on Redis < 7.2. The pipeline drained all responses before raising,
        # so if that's the only error, the socket is healthy: keep it.
        if result.is_a?(CommandError) && !(result.command[0] == "CLIENT" && result.command[1] == "SETINFO")
          raise result
        end
      end

      if config.sentinel?
        config.check_role!(results.last.first)
      end
    end
  rescue FailoverError, CannotConnectError => error
    @raw_connection&.close
    error._set_config(config)
    remember_unknown_outcome(error)
    raise error
  rescue ConnectionError => error
    @raw_connection&.close
    remember_unknown_outcome(error)
    connect_error = CannotConnectError.with_config(error.message, config)
    connect_error.set_backtrace(error.backtrace)
    # Preserve the "bytes were already in flight" evidence on the wrapped
    # error so observers and callers can still tell an unknown-outcome failure
    # from a never-sent one.
    if error.is_a?(ConnectionMixin::SentButUnknown)
      connect_error.extend(ConnectionMixin::SentButUnknown)
      connect_error.sent_commands = error.sent_commands
      connect_error.acknowledged_commands = error.acknowledged_commands
    end
    raise connect_error
  rescue CommandError => error
    @raw_connection&.close

    if error.command&.first == "HELLO" && error.message.match?(/ERR unknown command/)
      raise UnsupportedServer,
        "redis-client requires Redis 6+ with HELLO command available (#{config.server_url})", cause: error
    else
      raise
    end
  ensure
    if @raw_connection && observation_context.nil?
      @raw_connection.observing_context = nil
    end
  end
end

require "redis_client/pooled"
require "redis_client/circuit_breaker"

RedisClient.default_driver
