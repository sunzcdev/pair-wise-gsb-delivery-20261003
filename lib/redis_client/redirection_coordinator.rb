# frozen_string_literal: true

class RedisClient
  # Optional, bounded coordinator for Redis Cluster slot redirections.
  #
  # It is intentionally transport agnostic: it does not know how to open a
  # connection nor how to maintain a slot map. Instead, the caller supplies a
  # +client_factory+ callable that is handed every parsed {Redirection} and is
  # expected to return a client (a raw +RedisClient+, a +RedisClient::Pooled+,
  # or any object exposing +#call_v+, +#with+ and +#close+) connected to the
  # redirection target. The factory is also the natural place to update any
  # routing cache, using {Redirection#moved?} / {Redirection#ask?} to tell the
  # two redirection kinds apart.
  class RedirectionCoordinator
    # Default maximum number of redirections followed for a single command.
    DEFAULT_MAX_HOPS = 8

    # Parsed representation of a +MOVED+ or +ASK+ cluster redirection reply.
    #
    # Redis replies look like +MOVED 3999 127.0.0.1:6381+ or
    # +ASK 12182 [2001:db8::1]:6381+; the same address without brackets is
    # accepted as well.
    class Redirection
      # Matches "<KIND> <SLOT> <HOST>:<PORT>", including bracketed and bare
      # IPv6 literals. The host is greedy up to the final colon so that the
      # colons inside IPv6 addresses are not mistaken for the port separator.
      PATTERN = /\A\s*(MOVED|ASK)\s+(\d+)\s+\[?([^\]\s]+)\]?:(\d+)\s*\z/.freeze

      # @return [Integer] the hash slot the key belongs to.
      attr_reader :slot

      # @return [String] the target host or IP literal, without brackets.
      attr_reader :host

      # @return [Integer] the target port.
      attr_reader :port

      # @return [Symbol] +:moved+ or +:ask+.
      attr_reader :kind

      # @param kind [Symbol] +:moved+ or +:ask+.
      # @param slot [Integer] the redirected hash slot.
      # @param host [String] the target host or IP literal.
      # @param port [Integer] the target port.
      def initialize(kind, slot, host, port)
        @kind = kind
        @slot = Integer(slot)
        @host = host
        @port = Integer(port)
      end

      # Parse a cluster redirection error.
      #
      # @param error [RedisClient::CommandError, #message] the raised error.
      # @return [Redirection, nil] the parsed redirection, or +nil+ if the
      #   error is not a +MOVED+/+ASK+ redirection.
      def self.parse(error)
        match = PATTERN.match(error.message)
        return unless match

        new(match[1].downcase.to_sym, match[2], match[3], match[4])
      end

      # A +MOVED+ redirection means the slot has definitively moved to the
      # target node: the routing table/cache must be updated.
      def moved?
        @kind == :moved
      end

      # An +ASK+ redirection is only valid for the single command that triggered
      # it (typically during a slot migration): it must not be cached.
      def ask?
        @kind == :ask
      end

      # Whether the redirection may be persisted in a routing cache. Only
      # +MOVED+ redirections are authoritative; +ASK+ redirections are ephemeral.
      def cacheable?
        moved?
      end

      # @return [Boolean] whether the target host is an IPv6 literal (more than
      #   one colon, the same rule used by +Config#server_url+).
      def ipv6?
        @host.count(":") >= 2
      end

      # @return [String] the normalized +host:port+ target address, with IPv6
      #   literals wrapped in brackets.
      def address
        if ipv6?
          "[#{@host}]:#{@port}"
        else
          "#{@host}:#{@port}"
        end
      end

      # Identity used for redirection loop detection: the same slot being sent
      # back to the same target with the same semantics is a genuine loop.
      def loop_key
        [@kind, @slot, @host, @port]
      end

      def ==(other)
        other.is_a?(Redirection) && loop_key == other.loop_key
      end
      alias_method :eql?, :==

      def hash
        loop_key.hash
      end

      def inspect
        "#<#{self.class.name} #{@kind.upcase} #{@slot} #{address}>"
      end
    end

    class Error < RedisClient::Error
    end

    # Raised when the same slot keeps being redirected to the same target,
    # i.e. the redirection chain loops instead of converging.
    class RedirectionLoopError < Error
    end

    # Raised when the configured hop limit is reached without convergence.
    class RedirectionLimitError < Error
    end

    # Raised when automatic following is attempted for a pipeline or a
    # MULTI/EXEC transaction. A batch may already have been partially executed,
    # so it can never be split and replayed safely.
    class BatchNotSupportedError < Error
    end

    # @return [Integer] maximum number of redirections followed per command.
    attr_reader :max_hops

    # @param client_factory [#call] callable invoked as
    #   +client_factory.call(redirection)+ for every followed {Redirection}; it
    #   must return a client connected to the redirection target.
    # @param max_hops [Integer] maximum number of redirections to follow for a
    #   single command. Defaults to {DEFAULT_MAX_HOPS}.
    def initialize(client_factory, max_hops: DEFAULT_MAX_HOPS)
      unless client_factory.respond_to?(:call)
        raise ArgumentError, "client_factory must respond to #call"
      end

      @client_factory = client_factory
      @max_hops = Integer(max_hops)
      raise ArgumentError, "max_hops must be positive" unless @max_hops.positive?
    end

    # Execute a single command, following +MOVED+/+ASK+ redirections until the
    # command succeeds or a limit is reached.
    #
    # Only a single, already-built command (an Array of arguments) is accepted:
    # pipelines and transactions can never be safely replayed, see
    # {#pipelined} and {#multi}.
    #
    # @param client the client to send the first attempt to.
    # @param command [Array] the command and its arguments.
    # @return the command reply.
    def call(client, command)
      hops = 0
      seen = {}
      redirection = nil

      loop do
        # ASKING and the actual command must run inside one checkout so that
        # they share the same connection. The ASKING flag is consumed by the
        # very next command, which guarantees no state is leaked to whatever
        # user of the pool checks that connection out afterwards.
        result = send_command(client, command, redirection)
        return result
      rescue CommandError => error
        redirection = Redirection.parse(error)
        raise unless redirection

        seen_key = redirection.loop_key
        if seen.key?(seen_key)
          raise RedirectionLoopError,
            "slot #{redirection.slot} keeps being redirected to #{redirection.address}"
        end
        seen[seen_key] = true

        if hops >= @max_hops
          raise RedirectionLimitError,
            "exceeded #{@max_hops} redirection hop#{"s" if @max_hops > 1} while serving " \
              "#{command.first.inspect} (last: #{redirection.kind.upcase} #{redirection.slot} " \
              "#{redirection.address})"
        end
        hops += 1

        # The factory owns connection lifecycle and any slot-map/cache update,
        # so its errors are allowed to propagate untouched.
        client = @client_factory.call(redirection)
      end
    end

    # Pipelines cannot be followed automatically: some of the batched commands
    # may already have been executed when the redirection is returned, so
    # splitting and re-sending the batch could apply it twice.
    def pipelined(*_args)
      raise BatchNotSupportedError,
        "automatic redirection is not supported inside a pipeline; handle MOVED/ASK per command instead"
    end

    # A MULTI/EXEC transaction is an atomic batch and has the same problem as a
    # pipeline: it must not be split and replayed after a redirection.
    def multi(*_args)
      raise BatchNotSupportedError,
        "automatic redirection is not supported inside a MULTI/EXEC transaction; handle MOVED/ASK per command instead"
    end

    private

    def send_command(client, command, redirection)
      if redirection&.ask?
        client.with do |connection|
          connection.call_v(["ASKING"])
          connection.call_v(command)
        end
      else
        client.call_v(command)
      end
    end
  end
end
