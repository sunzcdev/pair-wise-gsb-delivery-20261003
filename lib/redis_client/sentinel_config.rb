# frozen_string_literal: true

class RedisClient
  class SentinelConfig
    include Config::Common

    SENTINEL_DELAY = 0.25
    DEFAULT_RECONNECT_ATTEMPTS = 2

    # A single address advertised by a Sentinel, together with everything that
    # was learned about it while resolving the role.
    #
    # `sentinel_host`/`sentinel_port` record *which* Sentinel answered, so a
    # resolution can be attributed after the fact and disagreements between
    # Sentinels can be reported per source.
    #
    # `role` is the role reported by the node itself (through `ROLE`), not the
    # role the Sentinel claimed. It stays `nil` until the node has been probed.
    #
    # `error` is set when the candidate was rejected. A rejected candidate is
    # never treated as confirmed and is never published as the reusable main
    # connection.
    Candidate = Struct.new(
      :host,
      :port,
      :sentinel_host,
      :sentinel_port,
      :role,
      :error,
      keyword_init: true
    ) do
      def address
        "#{host}:#{port}"
      end

      def sentinel_address
        "#{sentinel_host}:#{sentinel_port}"
      end

      def server_key
        [nil, host, port].freeze
      end

      def rejected?
        !error.nil?
      end

      def confirmed?(expected_role = nil)
        return false if rejected? || role.nil?
        return true if expected_role.nil?

        role == expected_role
      end

      def reject!(error)
        self.error = error
        self
      end

      def to_h
        {
          address: address,
          sentinel: sentinel_address,
          role: role,
          error: error && "#{error.class.name}: #{error.message}",
        }
      end
    end

    # The result of one discovery round.
    #
    # `candidates` holds every address a Sentinel advertised, in the order the
    # Sentinels were asked, including the ones later rejected by the role probe.
    # `failures` holds Sentinel-level errors (unreachable Sentinel, unknown name,
    # no replica). `selected` is the candidate that was published, if any.
    #
    # `generation` counts how many times the published address actually changed,
    # which is what lets a caller attribute an old connection's exit and a new
    # connection's establishment to one specific switch.
    Discovery = Struct.new(
      :role,
      :candidates,
      :failures,
      :selected,
      :generation,
      :previous_address,
      :consensus,
      :probed,
      :completed_at,
      keyword_init: true
    ) do
      def confirmed
        candidates.select { |candidate| !candidate.role.nil? && !candidate.rejected? }
      end

      def rejected
        candidates.select(&:rejected?)
      end

      def addresses
        candidates.map(&:address).uniq
      end

      def confirmed_addresses
        confirmed.map(&:address).uniq
      end

      # Every distinct confirmed address together with the Sentinels that
      # vouched for it. A stale Sentinel answer is rejected by the role probe,
      # so it never shows up here as a disagreement.
      def disagreements
        return [] if confirmed_addresses.size < 2

        confirmed.group_by(&:address).map do |address, group|
          { address: address, sentinels: group.map(&:sentinel_address).uniq }
        end
      end

      def disagreement?
        confirmed_addresses.size > 1
      end

      # True when no usable candidate was left, either because every Sentinel
      # failed or because every advertised candidate was rejected.
      def all_failed?
        candidates.empty? || candidates.all?(&:rejected?)
      end

      def all_rejected?
        !candidates.empty? && candidates.all?(&:rejected?)
      end

      def to_h
        {
          role: role,
          candidates: candidates.map(&:to_h),
          failures: failures.map { |failure| failure_to_h(failure) },
          selected: selected&.to_h,
          generation: generation,
          previous_address: previous_address,
          consensus: consensus,
          probed: probed,
          disagreement: disagreement?,
          all_failed: all_failed?,
        }
      end

      private

      def failure_to_h(failure)
        sentinel = failure[:sentinel]
        {
          sentinel: sentinel && "#{sentinel.host}:#{sentinel.port}",
          reason: failure[:reason],
          error: failure[:error] && "#{failure[:error].class.name}: #{failure[:error].message}",
        }
      end
    end

    module HasDiscovery
      attr_reader :discovery

      def _set_discovery(discovery) # :nodoc:
        @discovery = discovery
        self
      end
    end

    # One connection lifecycle transition, attributed to the address and the
    # generation it belongs to, so an old connection's exit can't be confused
    # with a new connection's establishment across two failovers.
    ConnectionEvent = Struct.new(:action, :address, :generation, :at, keyword_init: true) do
      def exit?
        action == :exit
      end

      def establish?
        action == :establish
      end
    end

    MAX_CONNECTION_EVENTS = 64

    attr_reader :name

    def initialize(
      sentinels:,
      sentinel_password: nil,
      sentinel_username: nil,
      role: :master,
      name: nil,
      url: nil,
      consensus: false,
      **client_config
    )
      unless %i(master replica slave).include?(role.to_sym)
        raise ArgumentError, "Expected role to be either :master or :replica, got: #{role.inspect}"
      end

      # Track whether SSL was explicitly provided by user
      ssl_explicitly_set = client_config.key?(:ssl)

      if url
        url_config = URLConfig.new(url)
        client_config = {
          username: url_config.username,
          password: url_config.password,
          db: url_config.db,
          ssl: url_config.ssl?,
        }.compact.merge(client_config)
        name ||= url_config.host
      end

      @name = name
      unless @name
        raise ArgumentError, "RedisClient::SentinelConfig requires either a name or an url with a host"
      end

      @to_list_of_hash = @to_hash = nil
      password = if sentinel_password && !sentinel_password.respond_to?(:call)
        ->(_) { sentinel_password }
      else
        sentinel_password
      end
      @extra_config = {
        username: sentinel_username,
        password: password,
        db: nil,
      }
      if client_config[:protocol] == 2
        @extra_config[:protocol] = client_config[:protocol]
        @to_list_of_hash = lambda do |may_be_a_list|
          if may_be_a_list.is_a?(Array)
            may_be_a_list.map { |l| l.each_slice(2).to_h }
          else
            may_be_a_list
          end
        end
      end

      @sentinels = {}.compare_by_identity
      @role = role.to_sym
      @consensus = !!consensus
      @mutex = Mutex.new
      @config = nil
      @discovery = nil
      @selected_candidate = nil
      @published_server_key = nil
      @confirmed_server_key = nil
      @generation = 0
      @previous_address = nil
      @connection_events = []

      client_config[:reconnect_attempts] ||= DEFAULT_RECONNECT_ATTEMPTS
      @client_config = client_config || {}
      @sentinel_client_config = @client_config.dup
      # Only remove SSL from sentinel config if it was derived from URL,
      # not if explicitly set by user.
      @sentinel_client_config.delete(:ssl) unless ssl_explicitly_set
      super(**client_config)
      @sentinel_configs = sentinels_to_configs(sentinels)
    end

    def sentinels
      @mutex.synchronize do
        @sentinel_configs.dup
      end
    end

    # The last discovery snapshot, or nil if nothing was resolved yet.
    #
    # The snapshot keeps which Sentinel advertised which address, the role each
    # candidate reported when probed, and the Sentinel-level failures.
    def discovery
      @mutex.synchronize { @discovery }
    end

    # The candidate backing the currently published config, if any.
    def selected_candidate
      @mutex.synchronize { @selected_candidate }
    end

    # How many times the published address changed. Two simulated failovers
    # produce a generation of 2.
    def generation
      @mutex.synchronize { @generation }
    end

    # The connection lifecycle transitions recorded so far, oldest first.
    #
    # Each event carries the address and the generation it belongs to, so an
    # old connection's exit can be attributed to the generation that is being
    # retired rather than to the one that replaced it.
    def connection_events
      @mutex.synchronize { @connection_events.dup }
    end

    # Records that a connection to `server_key` is being established (or was
    # just established). Called by the client around the connection prelude.
    def record_connection_established(server_key = nil)
      record_connection_event(:establish, server_key)
    end

    # Records that a connection to `server_key` is being closed. The generation
    # is resolved from the address, so a late close of the old connection is
    # attributed to the generation it actually belonged to.
    def record_connection_closed(server_key = nil)
      record_connection_event(:exit, server_key)
    end

    def consensus?
      @consensus
    end

    # Ask every known Sentinel, without publishing anything and without
    # affecting the reusable connection cache.
    #
    # Sentinels that disagree are reported through the returned snapshot even
    # when the resolver would have stopped at the first answer. This never
    # raises for Sentinel-level trouble.
    #
    # With `probe: true` (the default) each candidate is asked for its own role
    # so the snapshot records a role-confirmation result per candidate, which is
    # what makes a stale Sentinel answer visible before any connection is
    # published.
    def discover(probe: true)
      @mutex.synchronize do
        @discovery = run_discovery_locked(stop_early: false, probe: probe)
        @discovery
      end
    end

    # Re-resolves and publishes the address, discarding any cached one, and
    # verifies every candidate's role before it can be published.
    #
    # This is the explicit "resolve now and check the role first" entry point.
    # The lazy path used by `host`/`server_key` keeps the historical behaviour:
    # it publishes the Sentinel answer immediately and the role is verified by
    # the connection prelude (#check_role!) before the connection is handed out.
    #
    # Raises SentinelDisagreementError when Sentinels disagree without a
    # majority (consensus mode), and SentinelUnavailableError when every
    # Sentinel or every candidate failed.
    def resolve!
      @mutex.synchronize do
        @config = nil
        @discovery = nil
        snapshot = run_discovery_locked(stop_early: false, probe: true)
        @discovery = snapshot

        candidate = snapshot.selected
        raise_discovery_failure_locked(snapshot) unless candidate

        publish_locked(candidate)
        self
      end
    end

    # Called by the connection while it is being established, after the server
    # answered `ROLE`, so the role is checked before the connection is handed
    # to the caller.
    #
    # On a mismatch the candidate is rejected and dropped from the reusable
    # config *only if that config still points at it*, so a newer resolution is
    # never torn down by an older connection that was already on its way out.
    def check_role!(role)
      unless role.is_a?(String)
        raise TypeError, "Expected role to be a string, got: #{role.inspect}"
      end

      error = nil

      @mutex.synchronize do
        candidate = @selected_candidate || (@config && Candidate.new(host: @config.host, port: @config.port))

        if role == expected_role
          confirm_locked(candidate, role)
        else
          error = failover_error(role)
          reject_locked(candidate, role, error)
        end
      end

      if error
        sleep SENTINEL_DELAY
        raise error
      end
    end

    # True once an address has been resolved. Check #confirmed? to know whether
    # the node itself vouched for the role.
    #
    # Lock-free on purpose: `Error#message` asks `resolved?` while a discovery
    # error is being formatted, and the discovery may still be inside the mutex.
    def resolved?
      !@config.nil?
    end

    # True when the published address has been confirmed by the node's own
    # `ROLE` reply. Only confirmed candidates are treated as reusable.
    def confirmed?
      @config && @confirmed_server_key == @config.server_key ? true : false
    end

    def reset
      @mutex.synchronize do
        @config = nil
        @selected_candidate = nil
        @confirmed_server_key = nil
      end
    end

    def server_key
      config.server_key
    end

    def host
      config.host
    end

    def port
      config.port
    end

    def path
      nil
    end

    def retry_connecting?(attempt, error)
      reset unless error.is_a?(TimeoutError)
      super
    end

    def sentinel?
      true
    end

    private

    def expected_role
      @role == :master ? "master" : "slave"
    end

    def failover_error(role)
      expected = @role == :master ? "master" : "replica"
      actual = @role == :master ? "replica" : "master"
      FailoverError.new(
        "Expected to connect to a #{expected}, but the server is a #{actual} (ROLE reported #{role.inspect})"
      )
    end

    # Record a successful role probe. The candidate only becomes reusable from
    # this point on.
    def confirm_locked(candidate, role)
      return if candidate.nil?

      candidate.role = role
      candidate.error = nil
      @confirmed_server_key = candidate.server_key
      merge_candidate_locked(candidate)
    end

    # Record a rejected candidate. The candidate is removed from the reusable
    # cache, but only when the cache still belongs to that very candidate.
    def reject_locked(candidate, role, error)
      return if candidate.nil?

      candidate.role = role
      candidate.reject!(error)

      if @confirmed_server_key == candidate.server_key
        @confirmed_server_key = nil
      end

      if @config && @config.server_key == candidate.server_key
        @config = nil
        @selected_candidate = nil
      end

      merge_candidate_locked(candidate)
    end

    # Fold a role-confirmation result into the discovery snapshot so it stays
    # visible after the fact, including for candidates advertised by more than
    # one Sentinel.
    def merge_candidate_locked(candidate)
      snapshot = @discovery
      return unless snapshot

      matches = snapshot.candidates.select { |c| c.address == candidate.address }
      if matches.empty?
        snapshot.candidates << candidate
      else
        matches.each do |existing|
          existing.role = candidate.role
          existing.error = candidate.error
        end
      end

      if candidate.rejected? && snapshot.selected&.address == candidate.address
        snapshot.selected = nil
      end
    end

    # Records a connection lifecycle transition. The generation is the one that
    # is current right now; the address is the one the connection was actually
    # opened against, so a late close of the retired connection keeps pointing
    # at the retired address.
    def record_connection_event(action, server_key)
      @mutex.synchronize do
        key = server_key || (@config && @config.server_key)
        address = key && key[1] && key[2] ? "#{key[1]}:#{key[2]}" : nil
        @connection_events << ConnectionEvent.new(
          action: action,
          address: address,
          generation: @generation,
          at: RedisClient.now
        )
        @connection_events.shift while @connection_events.size > MAX_CONNECTION_EVENTS
      end
    end

    def sentinels_to_configs(sentinels)
      sentinels.map do |sentinel|
        case sentinel
        when String
          Config.new(**@sentinel_client_config, **@extra_config, url: sentinel)
        else
          Config.new(**@sentinel_client_config, **@extra_config, **sentinel)
        end
      end
    end

    def config
      @mutex.synchronize { config_locked }
    end

    def config_locked
      @config ||= begin
        snapshot = run_discovery_locked(stop_early: !@consensus, probe: false)
        @discovery = snapshot

        candidate = snapshot.selected
        raise_discovery_failure_locked(snapshot) unless candidate

        publish_locked(candidate)
      end
    end

    def raise_discovery_failure_locked(snapshot)
      error = if snapshot.all_rejected?
        reasons = snapshot.candidates.map do |candidate|
          "#{candidate.address} (via #{candidate.sentinel_address}): #{candidate.error.message}"
        end
        SentinelUnavailableError.new("All Sentinel candidates were rejected: #{reasons.join(', ')}")
      elsif snapshot.disagreement? && @consensus
        description = snapshot.disagreements.map do |entry|
          "#{entry[:address]} (from #{entry[:sentinels].join(', ')})"
        end
        SentinelDisagreementError.new("Sentinels disagree about #{@name.inspect}: #{description.join(' vs ')}")
      else
        details = snapshot.failures.map do |failure|
          sentinel = failure[:sentinel]
          source = sentinel ? "#{sentinel.host}:#{sentinel.port}" : "sentinel"
          "#{source}: #{failure[:reason]}#{failure[:error] ? " (#{failure[:error].message})" : ''}"
        end
        message = "Couldn't locate a #{@role} for role: #{@name}"
        message = "#{message} - #{details.join('; ')}" unless details.empty?
        SentinelUnavailableError.new(message)
      end

      error._set_config(self)
      error._set_discovery(snapshot)
      raise error
    end

    def publish_locked(candidate)
      new_config = Config.new(host: candidate.host, port: candidate.port, **@client_config)

      if @published_server_key && @published_server_key != new_config.server_key
        @generation += 1
      end
      @previous_address = @selected_candidate&.address
      @published_server_key = new_config.server_key
      @selected_candidate = candidate

      if @discovery
        @discovery.selected = candidate
        @discovery.generation = @generation
        @discovery.previous_address = @previous_address
      end

      new_config
    end

    def run_discovery_locked(stop_early:, probe: false)
      candidates = []
      failures = []

      @sentinel_configs.dup.each do |sentinel_config|
        sentinel_client = sentinel_client(sentinel_config)
        success = true
        begin
          found = query_sentinel(sentinel_config, sentinel_client)
          if found.nil? || found.empty?
            failures << { sentinel: sentinel_config, reason: :no_address }
          else
            candidates.concat(found)
          end
        rescue RedisClient::Error => error
          failures << { sentinel: sentinel_config, reason: :error, error: error }
          success = false
          sleep SENTINEL_DELAY
        ensure
          if success
            @sentinel_configs.unshift(@sentinel_configs.delete(sentinel_config))
          end
          # Redis Sentinels may be configured to have a lower maxclients setting than
          # the Redis nodes. Close the connection to the Sentinel node to avoid using
          # a connection.
          sentinel_client.close
        end

        break if stop_early && candidates.any?
      end

      probe_candidates(candidates) if probe

      Discovery.new(
        role: @role,
        candidates: candidates,
        failures: failures,
        selected: select_candidate(candidates, probe: probe),
        generation: @generation,
        previous_address: @previous_address,
        consensus: @consensus,
        probed: probe,
        completed_at: RedisClient.now
      )
    end

    # Ask every distinct candidate for its own role in one pass, so each address
    # is probed once even when several Sentinels advertised it.
    def probe_candidates(candidates)
      roles = {}
      candidates.map(&:address).uniq.each do |address|
        sample = candidates.find { |candidate| candidate.address == address }
        roles[address] = probe_role(sample.host, sample.port)
      end

      candidates.each do |candidate|
        role = roles[candidate.address]
        if role.is_a?(Exception)
          candidate.role = nil
          candidate.reject!(role)
        elsif role == expected_role
          candidate.role = role
          candidate.error = nil
        else
          candidate.role = role
          candidate.reject!(failover_error(role))
        end
      end
    end

    def query_sentinel(sentinel_config, sentinel_client)
      if @role == :master
        host, port = sentinel_client.call_v(["SENTINEL", "get-master-addr-by-name", @name])
        return [] unless host && port

        refresh_sentinels(sentinel_client)
        [build_candidate(sentinel_config, host, port)]
      else
        replicas = sentinel_client.call_v(["SENTINEL", "replicas", @name], &@to_list_of_hash)
        replicas.reject! do |replica|
          flags = replica["flags"].to_s.split(",")
          flags.include?("s_down") || flags.include?("o_down")
        end
        replicas.map { |replica| build_candidate(sentinel_config, replica["ip"], replica["port"]) }
      end
    end

    def build_candidate(sentinel_config, host, port)
      Candidate.new(
        host: host,
        port: Integer(port),
        sentinel_host: sentinel_config.host,
        sentinel_port: sentinel_config.port,
        role: nil,
        error: nil
      )
    end

    # Pick the address to publish.
    #
    # Replicas keep the historical random choice. For a master:
    #
    # * When the candidates were probed (explicit `discover`/`resolve!`), only
    #   candidates whose own `ROLE` reply matched are usable, so a stale Sentinel
    #   answer is already gone. If several *confirmed* addresses remain, a strict
    #   majority of the answers is required.
    # * Without a probe (the lazy path), the first usable answer wins unless
    #   `consensus` is on.
    def select_candidate(candidates, probe: false)
      usable = candidates.reject(&:rejected?)
      usable = usable.select { |candidate| candidate.confirmed?(expected_role) } if probe
      return nil if usable.empty?

      return usable.sample if @role != :master

      if (@consensus || probe) && candidates.map(&:address).uniq.size > 1
        votes = Hash.new(0)
        usable.each { |candidate| votes[candidate.address] += 1 }
        best_address, best_votes = votes.max_by { |_address, count| count }
        return nil unless best_votes * 2 > candidates.size

        usable.find { |candidate| candidate.address == best_address }
      else
        usable.first
      end
    end

    def sentinel_client(sentinel_config)
      @sentinels[sentinel_config] ||= sentinel_config.new_client
    end

    # Ask the node itself what role it currently has.
    #
    # Returns the role string, or the error as an exception instance so callers
    # can record it on the candidate without raising. Uses the same credentials,
    # SSL and timeouts as the main connection, but a short-lived client with no
    # retries or middlewares: the probe must never be the reusable connection,
    # and a probe failure must not be retried into a different node.
    def probe_role(host, port)
      probe_config = Config.new(
        **@client_config.merge(
          host: host,
          port: port,
          reconnect_attempts: false,
          middlewares: false,
          # A named connection could confuse `CLIENT KILL`/`CLIENT LIST` audits;
          # the probe is anonymous on purpose.
          id: nil
        )
      )
      client = probe_config.new_client
      begin
        role = client.call("ROLE").first
        role.is_a?(String) ? role : nil
      ensure
        client.close
      end
    rescue RedisClient::Error => error
      error
    end

    def refresh_sentinels(sentinel_client)
      sentinel_response = sentinel_client.call_v(["SENTINEL", "sentinels", @name], &@to_list_of_hash)
      sentinels = sentinel_response.map do |sentinel|
        { host: sentinel.fetch("ip"), port: Integer(sentinel.fetch("port")) }
      end
      new_sentinels = sentinels.select do |sentinel|
        @sentinel_configs.none? do |sentinel_config|
          sentinel_config.host == sentinel.fetch(:host) && sentinel_config.port == sentinel.fetch(:port)
        end
      end

      @sentinel_configs.concat sentinels_to_configs(new_sentinels)
    end
  end
end