module Dnsruby
  require 'dnsruby/transport_clock'
  require 'dnsruby/query_control'

  # Opt-in, bounded diagnostics for one DNS lookup.
  #
  # A lookup may pass through several distinguishable stages: a local hosts
  # file, a response cache, one or more UDP/TCP attempts, and local DNSSEC
  # validation. Each stage can be selected independently, and every emitted
  # event carries only the question identity, a result category and, where a
  # span really ran, its measured duration. Wire bytes, TSIG material and
  # answer payloads are never recorded.
  #
  # A retry (scheduled or protocol-driven) belongs to the original logical
  # query but receives its own attempt identifier. Stages that did not run
  # produce no event, so no duration is invented for them.
  #
  # Events are capped. Once the cap is reached further events are dropped and
  # a single +:truncated+ marker reports how many were lost. A listener that
  # raises never changes how the query itself is processed.
  module Diagnostics
    STAGES = %i[hosts cache attempt validation terminal].freeze
    DEFAULT_MAX_EVENTS = 64
    # Keep the reported drop count finite for very noisy applications.
    MAX_REPORTED_DROPS = 1_000_000

    RECORDER_KEY = :dnsruby_diagnostics_recorder
    LOOKUP_KEY = :dnsruby_diagnostics_lookup_chain

    # An immutable diagnostic record. +elapsed+ is present only for a span
    # that actually started and finished.
    Event = Struct.new(:query_id, :attempt_id, :sequence, :stage, :outcome,
      :qname, :qtype, :qclass, :server, :port, :transport, :elapsed, :dropped,
      keyword_init: true) do
      def initialize(**values)
        super
        freeze
      end

      def truncated?
        stage == :truncated
      end
    end

    # Map a completion to one terminal category. The same classifier serves
    # synchronous and asynchronous callers, so both report identical outcomes.
    def self.classify(response, error)
      if error
        when_error(error)
      else
        when_response(response)
      end
    rescue StandardError
      :error
    end

    def self.when_error(error)
      case error
      when QueryControl::Cancelled then :cancelled
      when QueryControl::DeadlineExceeded, ResolvTimeout then :timeout
      when ValidationLimitError, RecursionLimitError then :limit
      when NXDomain then :nxdomain
      when ServFail then :servfail
      when Refused then :refused
      when FormErr then :formerr
      when NotImp then :notimp
      when VerifyError then :bogus
      else :error
      end
    end

    def self.when_response(response)
      return :error unless response
      return :bogus if response.security_level == Message::SecurityLevel::BOGUS
      case response.rcode.code
      when RCode::NOERROR then :answer
      when RCode::NXDOMAIN then :nxdomain
      when RCode::SERVFAIL then :servfail
      when RCode::REFUSED then :refused
      when RCode::FORMERR then :formerr
      when RCode::NOTIMP then :notimp
      else :error
      end
    end

    private_class_method :when_error, :when_response

    def self.recorder
      Thread.current[RECORDER_KEY]
    end

    # Deliver any queued listener notifications for a recorder. Safe to call
    # from user code: a raising listener is logged and ignored.
    def self.flush(recorder = self.recorder)
      recorder&.flush
    rescue StandardError
      nil
    end

    # Run a block with a recorder installed in this thread. The block's return
    # value is returned; the recorder is also yielded for convenience.
    def self.capture(**options)
      recorder = options.delete(:recorder) || Recorder.new(**options)
      previous = Thread.current[RECORDER_KEY]
      pending = Thread.current[LOOKUP_KEY]
      Thread.current[RECORDER_KEY] = recorder
      Thread.current[LOOKUP_KEY] = nil
      begin
        yield recorder
      ensure
        recorder.flush
        Thread.current[RECORDER_KEY] = previous
        Thread.current[LOOKUP_KEY] = pending
      end
    end

    # Delimit one user-level lookup so a hosts probe and the DNS query it
    # falls through to are reported on the same chain.
    def self.lookup(_question = nil)
      previous = Thread.current[LOOKUP_KEY]
      Thread.current[LOOKUP_KEY] = nil
      begin
        yield
      ensure
        Thread.current[LOOKUP_KEY] = previous
      end
    end

    # Record a hosts lookup on the pending lookup chain, creating it when a
    # recorder is active. Returns nil (and does nothing) without a recorder.
    def self.lookup_chain(question = nil)
      active = recorder
      return nil unless active
      chain = Thread.current[LOOKUP_KEY]
      unless chain && chain.recorder.equal?(active)
        chain = Chain.new(active, question)
        Thread.current[LOOKUP_KEY] = chain
      end
      chain.bind(question) if question
      chain
    end

    # Return the chain a logical DNS query should report to. A pending hosts
    # chain is adopted by the first query for the same name; otherwise a new
    # chain is started. A finished, unrelated or stale chain is never reused.
    def self.chain_for_query(question, recorder: nil)
      active = self.recorder || recorder
      return nil unless active
      pending = Thread.current[LOOKUP_KEY]
      if pending && pending.recorder.equal?(active) && !pending.finished? &&
          pending.matches?(question)
        Thread.current[LOOKUP_KEY] = nil
        pending.bind(question)
        return pending
      end
      Thread.current[LOOKUP_KEY] = nil if pending && pending.recorder.equal?(active)
      Chain.new(active, question)
    end

    # The chain attached to a Message, if any. A Message keeps its chain across
    # duplicate sends and protocol retries so they share one logical query.
    def self.chain_for(message)
      return nil unless message
      message.instance_variable_get(:@dnsruby_diagnostics_chain)
    rescue StandardError
      nil
    end

    # Ensure a Message carries a chain for its current lookup. Attaching is
    # idempotent, prefers a chain already owned by the active recorder, and
    # starts a fresh chain when the previous one has already completed.
    def self.attach(message, recorder: nil)
      return nil unless message
      active = self.recorder || recorder
      existing = chain_for(message)
      if existing && !existing.finished? && (!active || existing.recorder.equal?(active))
        return existing
      end
      return nil unless active
      chain = chain_for_query(message.question.first, recorder: active)
      message.instance_variable_set(:@dnsruby_diagnostics_chain, chain)
      chain
    rescue StandardError
      nil
    end

    # Record a hosts-file probe. A hit answers the lookup and completes its
    # chain; a miss leaves the chain pending so the DNS query it falls through
    # to reports on the same chain.
    def self.record_hosts(name, found)
      active = recorder
      return nil unless active
      chain = lookup_chain(name)
      return nil unless chain
      chain.record(stage: :hosts, outcome: found ? :hit : :miss)
      chain.finish(:answer) if found
      chain
    rescue StandardError
      nil
    end

    # Record a cache consultation for a query. +result+ is :hit or :miss. Only
    # the first result per chain is kept: a retry that re-consults the cache does
    # not turn one logical lookup into several cache events.
    def self.record_cache(chain, result)
      return nil unless chain
      chain.record_cache(result)
    rescue StandardError
      nil
    end

    # Emit the terminal category for a Message exactly once. This is the single
    # completion point shared by synchronous and asynchronous callers.
    def self.finish(message, response, error)
      chain = chain_for(message)
      return nil unless chain
      chain.finish(classify(response, error))
    rescue StandardError
      nil
    end

    # Begin a measured attempt for a query. Returns nil when diagnostics are
    # not active for this query, so the caller can skip bookkeeping entirely.
    def self.begin_attempt(message, transport:, server:, port:, recorder: nil)
      chain = attach(message, recorder: recorder)
      return nil unless chain
      chain.begin_attempt(transport: transport, server: server, port: port)
    rescue StandardError
      nil
    end

    # Run diagnostic work without letting a wiring fault change the query.
    def self.safely
      yield
    rescue StandardError => error
      begin
        Dnsruby.log.debug { "diagnostics unavailable: #{error.class}: #{error.message}" }
      rescue StandardError
      end
      nil
    end

    # One logical lookup. Retries share +query_id+ but get distinct attempts.
    class Chain
      attr_reader :recorder, :query_id

      def initialize(recorder, question = nil)
        @recorder = recorder
        @query_id = recorder.next_query_id
        @mutex = Mutex.new
        @qname = @qtype = @qclass = nil
        @finished = false
        @cache_seen = false
        bind(question) if question
      end

      def bind(question)
        return self unless question
        qname, qtype, qclass = Diagnostics.identity(question)
        @mutex.synchronize do
          @qname ||= qname
          @qtype ||= qtype
          @qclass ||= qclass
        end
        self
      end

      # Whether a query belongs to this chain. A chain created from a hosts
      # probe holds only the name, so any type/class for that name matches.
      def matches?(question)
        return true if question.nil?
        qname, qtype, qclass = Diagnostics.identity(question)
        @mutex.synchronize do
          if @qname && qname && Diagnostics.normalize_name(@qname) != Diagnostics.normalize_name(qname)
            return false
          end
          (@qtype.nil? || qtype.nil? || @qtype == qtype) &&
            (@qclass.nil? || qclass.nil? || @qclass == qclass)
        end
      end

      def record(stage:, outcome:, **fields)
        qname, qtype, qclass = @mutex.synchronize { [@qname, @qtype, @qclass] }
        @recorder.record(stage: stage, outcome: outcome, query_id: @query_id,
          qname: qname, qtype: qtype, qclass: qclass, **fields)
      end

      # A logical query consults a cache at most once. A retry that reaches the
      # cache again does not add a second cache event.
      def record_cache(result)
        claimed = @mutex.synchronize do
          next false if @cache_seen
          @cache_seen = true
        end
        return nil unless claimed
        record(stage: :cache, outcome: result)
      end

      # Emit the one terminal category. Repeated completion paths are ignored.
      def finish(outcome)
        terminal = @mutex.synchronize do
          next false if @finished
          @finished = true
        end
        return nil unless terminal
        record(stage: :terminal, outcome: outcome)
      end

      def finished?
        @mutex.synchronize { @finished }
      end

      def begin_attempt(server:, port:, transport: nil)
        Attempt.new(self, server: server, port: port, transport: transport)
      end

      # Wrap work that really runs. One validation event is emitted on the way
      # out with its measured duration; a failure keeps its own category and
      # the exception continues unchanged. No event is emitted if the span
      # never starts, so an unused validation stage has no invented duration.
      def measure_validation
        started = TransportClock.now
        outcome = :error
        begin
          result = yield
          outcome = :finished
          result
        ensure
          Diagnostics.safely do
            record(stage: :validation, outcome: outcome,
              elapsed: TransportClock.now - started)
          end
        end
      end
    end

    # A single UDP or TCP attempt belonging to one logical query. Each retry
    # (scheduled or protocol-driven) gets a new attempt identifier. One event is
    # emitted when the attempt finishes, carrying its measured duration and the
    # transport actually used; an attempt that never finishes emits nothing, so
    # no duration is invented for a stage that did not complete.
    class Attempt
      attr_reader :attempt_id

      def initialize(chain, server:, port:, transport: nil)
        @chain = chain
        @attempt_id = chain.recorder.next_attempt_id
        @server = server
        @port = port
        @transport = transport
        @started = TransportClock.now
        @mutex = Mutex.new
        @finished = false
      end

      def transport=(value)
        @transport = value
      end

      def finish(outcome, transport: nil)
        claimed = @mutex.synchronize do
          next false if @finished
          @finished = true
        end
        return false unless claimed
        @transport = transport if transport
        Diagnostics.safely do
          @chain.record(stage: :attempt, outcome: outcome, attempt_id: @attempt_id,
            transport: @transport, server: @server, port: @port,
            elapsed: TransportClock.now - @started)
        end
        true
      end
    end

    # Bounded, thread-safe event store. Listener failures are swallowed.
    class Recorder
      attr_reader :max_events

      def initialize(stages: STAGES, max_events: DEFAULT_MAX_EVENTS, listener: nil)
        selected = Array(stages).map(&:to_sym).uniq
        unknown = selected - STAGES
        unless unknown.empty?
          raise ArgumentError, "unknown diagnostic stage(s): #{unknown.join(', ')}"
        end
        raise ArgumentError, 'at least one diagnostic stage is required' if selected.empty?
        limit = Integer(max_events)
        raise ArgumentError, 'max_events must be at least 2' if limit < 2

        @stages = selected.freeze
        @max_events = limit
        @listener = listener
        @events = []
        @pending = []
        @terminal = nil
        @dropped = 0
        @closed = false
        @mutex = Mutex.new
        @sequence = 0
        @next_query_id = 0
        @next_attempt_id = 0
      end

      def stages
        @stages
      end

      def stage?(stage)
        @stages.include?(stage.to_sym)
      end

      # The bounded event view. Any queued listener notifications are delivered
      # first, so an asynchronous caller that reads its events also observes
      # them through the listener.
      def events
        flush
        snapshot.first
      end

      # Number of events left out of #events, including any clipped by the
      # retained window. Zero when nothing was dropped.
      def dropped
        snapshot.last
      end

      def truncated?
        dropped > 0
      end

      def closed?
        @mutex.synchronize { @closed }
      end

      # Stop accepting further events (for example when a capture is over).
      def close
        @mutex.synchronize { @closed = true }
        self
      end

      def next_query_id
        @mutex.synchronize { @next_query_id += 1 }
      end

      def next_attempt_id
        @mutex.synchronize { @next_attempt_id += 1 }
      end

      def record(stage:, outcome:, **fields)
        stage = stage.to_sym
        return nil unless stage?(stage)

        @mutex.synchronize do
          return nil if @closed
          @sequence += 1
          event = build_event(stage, outcome, @sequence, fields)
          if stage == :terminal
            @terminal = event
          elsif @events.length < @max_events
            @events << event
          else
            @dropped += 1
            @dropped = MAX_REPORTED_DROPS if @dropped > MAX_REPORTED_DROPS
          end
          @pending << event if @listener && @pending.length < @max_events
          event
        end
      end

      # Deliver queued listener notifications. Called from read paths and at the
      # end of a capture, never while a resolver lock is held, so listener code
      # can neither deadlock nor fail the query it is observing.
      def flush
        return self unless @listener
        pending = @mutex.synchronize do
          list = @pending
          @pending = []
          list
        end
        pending.each do |event|
          begin
            @listener.call(event)
          rescue StandardError => error
            begin
              Dnsruby.log.debug { "diagnostics listener failed: #{error.class}: #{error.message}" }
            rescue StandardError
            end
          end
        end
        self
      end

      private

      def snapshot
        @mutex.synchronize do
          terminals = @terminal ? [@terminal] : []
          room = @max_events - terminals.length
          if @dropped == 0 && @events.length <= room
            [(@events + terminals).freeze, 0]
          else
            keep = [room - 1, 0].max
            keep = [keep, @events.length].min
            clipped = @events.length - keep
            dropped = [@dropped + clipped, MAX_REPORTED_DROPS].min
            marker = truncation_event(dropped)
            list = @events.first(keep) + [marker] + terminals
            [list.freeze, dropped]
          end
        end
      end

      def truncation_event(dropped)
        Event.new(query_id: nil, attempt_id: nil, sequence: @sequence,
          stage: :truncated, outcome: :truncated, qname: nil, qtype: nil,
          qclass: nil, server: nil, port: nil, transport: nil, elapsed: nil,
          dropped: dropped)
      end

      def build_event(stage, outcome, sequence, fields)
        Event.new(
          query_id: fields[:query_id],
          attempt_id: fields[:attempt_id],
          sequence: sequence,
          stage: stage,
          outcome: outcome.to_sym,
          qname: freeze_string(fields[:qname]),
          qtype: fields[:qtype],
          qclass: fields[:qclass],
          server: freeze_string(fields[:server]),
          port: fields[:port],
          transport: fields[:transport],
          elapsed: fields[:elapsed] && Float(fields[:elapsed]),
          dropped: nil)
      end

      def freeze_string(value)
        value.nil? ? nil : String.new(value.to_s).freeze
      end
    end

    # A compact, payload-free identity for a Question, Name or String.
    def self.identity(question)
      if question.respond_to?(:qname)
        [name_string(question.qname), code_of(question.qtype), code_of(question.qclass)]
      else
        [name_string(question), nil, nil]
      end
    end

    def self.name_string(name)
      return nil if name.nil?
      name.respond_to?(:to_s) ? name.to_s(true) : name.to_s
    rescue StandardError
      name.to_s
    end

    # Case- and trailing-dot-insensitive comparison key for a name string.
    def self.normalize_name(name)
      return nil if name.nil?
      value = name.to_s.downcase
      value.end_with?('.') ? value : "#{value}."
    end

    def self.code_of(value)
      return nil if value.nil?
      value.respond_to?(:code) ? value.code : value
    end

    private_class_method :name_string, :code_of, :when_error, :when_response
  end
end