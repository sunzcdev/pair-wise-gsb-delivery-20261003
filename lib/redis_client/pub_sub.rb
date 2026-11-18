# frozen_string_literal: true

class RedisClient
  # Stateful Pub/Sub connection that tracks two distinct sets of subscriptions:
  #
  # * the *desired* set: what the caller asked this client to be subscribed to
  #   (`SUBSCRIBE` / `PSUBSCRIBE`, minus `UNSUBSCRIBE` / `PUNSUBSCRIBE`);
  # * the *confirmed* set: what the server actually acknowledged on the
  #   current physical connection.
  #
  # Sending a command only records intent. A channel or pattern becomes
  # confirmed only when the matching acknowledgement event (`subscribe`,
  # `unsubscribe`, `psubscribe`, `punsubscribe`) is consumed from the
  # connection, so a command that was written but not answered is never
  # reported as effective.
  #
  # A `PubSub` instance owns a single physical connection at a time. Every
  # connection has a monotonically increasing {#generation}. Losing the
  # connection, or replacing it with {#reconnect}, discards the confirmed set
  # (that state died with the old connection), opens a {Gap} and starts a new
  # generation. Events attributed to a superseded generation are dropped and
  # never mutate the current subscription state.
  class PubSub
    # An immutable set of channel and pattern subscriptions.
    class SubscriptionSet
      attr_reader :channels, :patterns

      def initialize(channels, patterns)
        @channels = channels.dup.freeze
        @patterns = patterns.dup.freeze
        freeze
      end

      def empty?
        @channels.empty? && @patterns.empty?
      end

      def size
        @channels.size + @patterns.size
      end
      alias_method :count, :size

      def include_channel?(channel)
        @channels.include?(channel)
      end

      def include_pattern?(pattern)
        @patterns.include?(pattern)
      end

      def include?(name)
        include_channel?(name) || include_pattern?(name)
      end
      alias_method :subscribed?, :include?

      def to_a
        @channels + @patterns
      end

      def to_h
        { channels: @channels, patterns: @patterns }
      end

      def ==(other)
        other.is_a?(SubscriptionSet) &&
          channels == other.channels &&
          patterns == other.patterns
      end
      alias_method :eql?, :==

      def hash
        [channels, patterns].hash
      end

      def inspect
        "#<#{self.class.name} channels=#{@channels.inspect} patterns=#{@patterns.inspect}>"
      end
    end

    # A window during which the client could not receive messages.
    #
    # The gap is meant to be observed by the caller. Pub/Sub has no backlog, so
    # anything published while the physical connection was down is gone:
    # {#replayable?} is always false and {#replayed?} stays false. Reconnecting
    # restores the subscriptions, not the messages.
    class Gap
      attr_reader :generation, :next_generation, :started_at, :ended_at,
        :resubscribed_channels, :resubscribed_patterns

      def initialize(generation:, started_at:)
        @generation = generation
        @next_generation = nil
        @started_at = started_at
        @ended_at = nil
        @resubscribed_channels = [].freeze
        @resubscribed_patterns = [].freeze
      end

      # Was a replacement connection established and were the subscriptions
      # re-announced on it?
      def reconnected?
        !@next_generation.nil?
      end

      def open?
        @ended_at.nil?
      end

      def closed?
        !open?
      end

      def duration
        (@ended_at || RedisClient.now) - @started_at
      end

      # Pub/Sub messages published during the gap were not received and are not
      # recoverable.
      def replayable?
        false
      end
      alias_method :recoverable?, :replayable?

      def replayed?
        false
      end

      # Any publication during this window may have been missed.
      def may_have_missed_messages?
        true
      end

      # Copy of the gap for a state snapshot: later transitions keep updating
      # the live object, never this copy.
      def snapshot
        clone.freeze
      end

      def to_h
        {
          generation: @generation,
          next_generation: @next_generation,
          started_at: @started_at,
          ended_at: @ended_at,
          open: open?,
          duration: duration,
          reconnected: reconnected?,
          resubscribed_channels: @resubscribed_channels,
          resubscribed_patterns: @resubscribed_patterns,
          may_have_missed_messages: may_have_missed_messages?,
          replayable: replayable?,
          replayed: replayed?,
        }
      end

      def inspect
        "#<#{self.class.name} generation=#{@generation}->#{@next_generation.inspect} open=#{open?}>"
      end

      def _reconnect!(generation, channels, patterns) # :nodoc:
        @next_generation = generation
        @resubscribed_channels = channels.dup.freeze
        @resubscribed_patterns = patterns.dup.freeze
        self
      end

      def _close!(time) # :nodoc:
        @ended_at = time
        self
      end
    end

    # Immutable snapshot returned by {PubSub#state}.
    class State
      attr_reader :generation, :desired, :confirmed, :gap,
        :reconnects, :reconnect_attempts, :disconnects, :dropped_events

      def initialize(
        generation:, desired:, confirmed:, gap:, connected:, closed:,
        reconnects:, reconnect_attempts:, disconnects:, dropped_events:
      )
        @generation = generation
        @desired = desired
        @confirmed = confirmed
        @gap = gap
        @connected = connected
        @closed = closed
        @reconnects = reconnects
        @reconnect_attempts = reconnect_attempts
        @disconnects = disconnects
        @dropped_events = dropped_events
        freeze
      end

      def connected?
        @connected
      end

      def disconnected?
        !@connected && !@closed
      end

      def closed?
        @closed
      end

      def desired_channels
        @desired.channels
      end

      def desired_patterns
        @desired.patterns
      end

      def confirmed_channels
        @confirmed.channels
      end

      def confirmed_patterns
        @confirmed.patterns
      end

      # Wanted, but not acknowledged on the current connection yet.
      def pending_channels
        @desired.channels.reject { |channel| @confirmed.include_channel?(channel) }
      end

      def pending_patterns
        @desired.patterns.reject { |pattern| @confirmed.include_pattern?(pattern) }
      end

      # Acknowledged, but no longer wanted (the removal acknowledgement is still
      # in flight).
      def stale_channels
        @confirmed.channels.reject { |channel| @desired.include_channel?(channel) }
      end

      def stale_patterns
        @confirmed.patterns.reject { |pattern| @desired.include_pattern?(pattern) }
      end

      # Number of subscriptions acknowledged on the current connection.
      # Re-confirming an existing subscription does not change it.
      def confirmed_count
        @confirmed.size
      end
      alias_method :subscription_count, :confirmed_count

      def desired_count
        @desired.size
      end

      # The confirmed set matches the desired set in both directions.
      def settled?
        pending_channels.empty? &&
          pending_patterns.empty? &&
          stale_channels.empty? &&
          stale_patterns.empty?
      end
      alias_method :synchronized?, :settled?

      def pending?
        !settled?
      end
      alias_method :unconfirmed?, :pending?

      # Every desired subscription has been acknowledged. A still-pending
      # removal of a subscription that is no longer wanted does not make this
      # false; use {#settled?} to also require that.
      def confirmed?
        pending_channels.empty? && pending_patterns.empty?
      end

      def gap?
        !@gap.nil?
      end

      def to_h
        {
          generation: @generation,
          connected: connected?,
          closed: closed?,
          desired: @desired.to_h,
          confirmed: @confirmed.to_h,
          pending_channels: pending_channels,
          pending_patterns: pending_patterns,
          stale_channels: stale_channels,
          stale_patterns: stale_patterns,
          desired_count: desired_count,
          confirmed_count: confirmed_count,
          settled: settled?,
          all_confirmed: confirmed?,
          gap: @gap&.to_h,
          reconnects: @reconnects,
          reconnect_attempts: @reconnect_attempts,
          disconnects: @disconnects,
          dropped_events: @dropped_events,
        }
      end

      def inspect
        "#<#{self.class.name} generation=#{@generation} desired=#{desired_count} " \
          "confirmed=#{confirmed_count} settled=#{settled?} gap=#{gap?}>"
      end
    end

    ACK_TYPES = %w[subscribe unsubscribe psubscribe punsubscribe].freeze

    # A minimal connection interface is expected: `read(timeout)`, `write(command)`
    # and `close`.
    def initialize(raw_connection, command_builder, reconnect: nil, connection_factory: nil)
      @raw_connection = raw_connection
      @command_builder = command_builder
      @connection_factory = reconnect || connection_factory
      @generation = 1
      @desired_channels = {}
      @desired_patterns = {}
      @confirmed_channels = {}
      @confirmed_patterns = {}
      @gaps = []
      @gap = nil
      @reconnects = 0
      @reconnect_attempts = 0
      @disconnects = 0
      @dropped_events = 0
      @closed = false
      @mutex = Mutex.new
    end

    # Write a Pub/Sub command without marking it as effective.
    #
    # The caller's intent is recorded (so a later {#reconnect} can re-send it),
    # but the confirmed set only changes when the corresponding acknowledgement
    # is read by {#next_event} or fed to {#handle_event}.
    def call(*command, **kwargs)
      command = @command_builder.generate(command, kwargs)
      write_command(command)
      nil
    end

    # Same as {#call} for an already built command.
    def call_v(command)
      write_command(@command_builder.generate(command))
      nil
    end

    def subscribe(*channels)
      raise ArgumentError, "no channel provided" if channels.empty?

      call("SUBSCRIBE", *channels)
    end

    def unsubscribe(*channels)
      call("UNSUBSCRIBE", *channels)
    end

    def psubscribe(*patterns)
      raise ArgumentError, "no pattern provided" if patterns.empty?

      call("PSUBSCRIBE", *patterns)
    end

    def punsubscribe(*patterns)
      call("PUNSUBSCRIBE", *patterns)
    end

    # Read the next event from the current connection.
    #
    # Acknowledgement events update the confirmed set. Events received from a
    # superseded connection generation are dropped and never update the current
    # state. Returns `nil` when no event arrived within `timeout`.
    #
    # `timeout` semantics match the previous behaviour: `nil` and `0` block
    # until an event arrives.
    def next_event(timeout = nil)
      deadline = timeout && timeout > 0 ? RedisClient.now + timeout : nil

      loop do
        connection, generation = current_connection!
        remaining = deadline && (deadline - RedisClient.now)
        return nil if remaining && remaining <= 0

        begin
          event = connection.read(remaining)
        rescue ReadTimeoutError
          return nil
        rescue ConnectionError
          # The connection may have been replaced while this read was blocked.
          # A failure on a superseded connection says nothing about the current
          # one, so retry there instead of reporting a loss.
          next unless current_generation?(generation)

          mark_disconnected!(connection)
          raise
        end

        return event if handle_event(event, generation: generation)
        # Otherwise the event belonged to an old connection: dropped, read on.
      end
    end

    # Feed an event observed on a specific connection generation.
    #
    # This is the entry point for adapters that read from the socket
    # themselves. Returns `true` when the event belongs to the current
    # generation and was processed, `false` when it came from a superseded
    # connection (counted in `state.dropped_events`).
    def handle_event(event, generation: @generation)
      @mutex.synchronize do
        if @closed || generation != @generation
          @dropped_events += 1
          return false
        end

        name = event_name(event)
        apply_acknowledgement(name, event) if ACK_TYPES.include?(name)
        true
      end
    end
    alias_method :ingest_event, :handle_event

    # End this client's Pub/Sub session.
    #
    # Only the subscriptions owned by this instance are ended: the confirmed
    # channels and patterns are explicitly unsubscribed (never with a bare
    # `UNSUBSCRIBE`/`PUNSUBSCRIBE`, which could be misread as ending somebody
    # else's subscriptions) and then this instance's connection is closed.
    def close
      connection = nil
      channels = nil
      patterns = nil
      @mutex.synchronize do
        connection = @raw_connection
        @raw_connection = nil
        # Only this instance's subscriptions: wanted or already acknowledged.
        channels = (@confirmed_channels.keys | @desired_channels.keys)
        patterns = (@confirmed_patterns.keys | @desired_patterns.keys)
        @desired_channels.clear
        @desired_patterns.clear
        @confirmed_channels.clear
        @confirmed_patterns.clear
        close_gap
        @closed = true
      end

      if connection
        unsubscribe_owned(connection, channels, patterns)
        begin
          connection.close
        rescue IOError, SystemCallError
        end
      end
      self
    end

    # Replace the physical connection while preserving the desired set.
    #
    # The confirmed set is discarded, a new generation starts, a {Gap} opens and
    # the desired subscriptions are re-sent on the new connection. The gap stays
    # open until all of them are acknowledged again, making the delivery hole
    # observable. No message is replayed.
    def reconnect(connection = nil)
      begin_reconnect!
      fresh = connection || build_connection
      complete_reconnect(fresh)
      self
    end

    # Re-send the desired subscriptions on the current connection without
    # changing the generation.
    def resubscribe
      connection, channels, patterns = @mutex.synchronize do
        connection = @raw_connection
        raise ConnectionError, "Connection was closed or lost" if @closed || connection.nil?

        [connection, @desired_channels.keys, @desired_patterns.keys]
      end

      resubscribe_on(connection, channels, patterns)
      @mutex.synchronize { close_gap if settled_state? }
      self
    end

    def connected?
      @mutex.synchronize { !@closed && !@raw_connection.nil? }
    end

    def closed?
      @closed
    end

    # Identifier of the current physical connection.
    attr_reader :generation
    alias_method :connection_generation, :generation

    # What the caller asked for.
    def desired_subscriptions
      @mutex.synchronize { subscription_set(@desired_channels, @desired_patterns) }
    end
    alias_method :desired, :desired_subscriptions

    # What the server acknowledged on the current connection.
    def confirmed_subscriptions
      @mutex.synchronize { subscription_set(@confirmed_channels, @confirmed_patterns) }
    end
    alias_method :confirmed, :confirmed_subscriptions

    def desired_channels
      desired_subscriptions.channels
    end

    def desired_patterns
      desired_subscriptions.patterns
    end

    def confirmed_channels
      confirmed_subscriptions.channels
    end

    def confirmed_patterns
      confirmed_subscriptions.patterns
    end

    def pending_channels
      @mutex.synchronize do
        @desired_channels.keys.reject { |channel| @confirmed_channels.key?(channel) }
      end
    end

    def pending_patterns
      @mutex.synchronize do
        @desired_patterns.keys.reject { |pattern| @confirmed_patterns.key?(pattern) }
      end
    end

    def pending?
      @mutex.synchronize { !settled_state? }
    end
    alias_method :unconfirmed?, :pending?

    def settled?
      @mutex.synchronize { settled_state? }
    end
    alias_method :synchronized?, :settled?

    # Every desired subscription has been acknowledged. A pending removal of a
    # subscription that is no longer wanted does not make this false; use
    # {#settled?} to also require that.
    def confirmed?
      @mutex.synchronize do
        @desired_channels.each_key.all? { |channel| @confirmed_channels.key?(channel) } &&
          @desired_patterns.each_key.all? { |pattern| @confirmed_patterns.key?(pattern) }
      end
    end

    def subscribed?(name, kind: nil)
      @mutex.synchronize do
        case kind
        when :channel
          @confirmed_channels.key?(name)
        when :pattern
          @confirmed_patterns.key?(name)
        else
          @confirmed_channels.key?(name) || @confirmed_patterns.key?(name)
        end
      end
    end

    def wanted?(name, kind: nil)
      @mutex.synchronize do
        case kind
        when :channel
          @desired_channels.key?(name)
        when :pattern
          @desired_patterns.key?(name)
        else
          @desired_channels.key?(name) || @desired_patterns.key?(name)
        end
      end
    end

    # Number of subscriptions acknowledged on the current connection.
    def subscription_count
      @mutex.synchronize { @confirmed_channels.size + @confirmed_patterns.size }
    end
    alias_method :confirmed_count, :subscription_count

    # Number of subscriptions the caller currently wants.
    def desired_count
      @mutex.synchronize { @desired_channels.size + @desired_patterns.size }
    end

    # Subscriptions still waiting for an acknowledgement.
    def pending_count
      @mutex.synchronize do
        pending = @desired_channels.keys.count { |channel| !@confirmed_channels.key?(channel) }
        pending + @desired_patterns.keys.count { |pattern| !@confirmed_patterns.key?(pattern) }
      end
    end

    def dropped_events
      @mutex.synchronize { @dropped_events }
    end

    # Immutable snapshot of the current state.
    def state
      @mutex.synchronize do
        State.new(
          generation: @generation,
          desired: subscription_set(@desired_channels, @desired_patterns),
          confirmed: subscription_set(@confirmed_channels, @confirmed_patterns),
          gap: @gap&.snapshot,
          connected: !@closed && !@raw_connection.nil?,
          closed: @closed,
          reconnects: @reconnects,
          reconnect_attempts: @reconnect_attempts,
          disconnects: @disconnects,
          dropped_events: @dropped_events,
        )
      end
    end
    alias_method :status, :state
    alias_method :subscription_state, :state

    # Every recorded gap, including the currently open one.
    def gaps
      @mutex.synchronize { @gaps.dup.freeze }
    end

    # The currently open gap, if any (live object).
    def current_gap
      @mutex.synchronize { @gap }
    end
    alias_method :gap, :current_gap

    # There is an open delivery gap right now, i.e. some messages may be missed.
    def gap?
      @mutex.synchronize { !@gap.nil? }
    end

    def disconnected?
      @mutex.synchronize { !@closed && @raw_connection.nil? }
    end

    def inspect
      @mutex.synchronize do
        "#<#{self.class.name} generation=#{@generation} " \
          "desired=#{@desired_channels.size + @desired_patterns.size} " \
          "confirmed=#{@confirmed_channels.size + @confirmed_patterns.size} " \
          "closed=#{@closed}>"
      end
    end

    private

    def write_command(command)
      connection = nil
      @mutex.synchronize do
        connection = @raw_connection
        raise ConnectionError, "Connection was closed or lost" if @closed || connection.nil?

        apply_intent(command)
      end
      connection.write(command)
      nil
    rescue ConnectionError
      # Only report a loss if it happened on the connection that is still the
      # current one; a concurrent reconnect has already handled its own.
      mark_disconnected!(connection)
      raise
    end

    def apply_intent(command)
      name = command.first.to_s.upcase
      arguments = command.drop(1)
      case name
      when "SUBSCRIBE"
        arguments.each { |channel| @desired_channels[channel] = true }
      when "UNSUBSCRIBE"
        if arguments.empty?
          @desired_channels.clear
        else
          arguments.each { |channel| @desired_channels.delete(channel) }
        end
      when "PSUBSCRIBE"
        arguments.each { |pattern| @desired_patterns[pattern] = true }
      when "PUNSUBSCRIBE"
        if arguments.empty?
          @desired_patterns.clear
        else
          arguments.each { |pattern| @desired_patterns.delete(pattern) }
        end
      end
    end

    def apply_acknowledgement(name, event)
      subject = event[1]
      case name
      when "subscribe"
        @confirmed_channels[subject] = true unless subject.nil?
      when "unsubscribe"
        if subject.nil?
          @confirmed_channels.clear
        else
          @confirmed_channels.delete(subject)
        end
      when "psubscribe"
        @confirmed_patterns[subject] = true unless subject.nil?
      when "punsubscribe"
        if subject.nil?
          @confirmed_patterns.clear
        else
          @confirmed_patterns.delete(subject)
        end
      end
      close_gap if @gap && settled_state?
    end

    def event_name(event)
      return nil unless event.is_a?(Array) && !event.empty?

      event[0].to_s.downcase
    end

    def begin_reconnect!
      previous = @mutex.synchronize do
        raise ConnectionError, "Connection was closed" if @closed

        @reconnect_attempts += 1
        previous = @raw_connection
        @raw_connection = nil
        @confirmed_channels.clear
        @confirmed_patterns.clear
        open_gap
        previous
      end

      close_quietly(previous)
    end

    def build_connection
      factory = @connection_factory
      unless factory
        raise ConnectionError, "No connection available and no reconnect handler configured"
      end

      factory.call || raise(ConnectionError, "Reconnect handler did not return a connection")
    rescue ConnectionError
      # No replacement connection was obtained. The client stays disconnected
      # (the gap remains open) until a later reconnect succeeds.
      mark_disconnected!
      raise
    end

    def complete_reconnect(connection)
      channels, patterns = @mutex.synchronize do
        raise ConnectionError, "Connection was closed" if @closed

        @generation += 1
        @raw_connection = connection
        channels = @desired_channels.keys
        patterns = @desired_patterns.keys
        @gap&._reconnect!(@generation, channels, patterns)
        @reconnects += 1
        [channels, patterns]
      end

      # Re-announce after the state transition, so the new connection's
      # acknowledgements can be processed as they are read.
      resubscribe_on(connection, channels, patterns)

      # Nothing to restore: the gap is both opened and closed by this
      # transition, and stays observable through {#gaps}.
      @mutex.synchronize { close_gap if settled_state? }
    rescue ConnectionError
      # The replacement connection failed before the subscriptions were
      # re-announced. Keep the desired set, but do not pretend it is live.
      mark_disconnected!
      raise
    end

    def resubscribe_on(connection, channels, patterns)
      connection.write(["SUBSCRIBE", *channels]) unless channels.empty?
      connection.write(["PSUBSCRIBE", *patterns]) unless patterns.empty?
    end

    def unsubscribe_owned(connection, channels, patterns)
      connection.write(["UNSUBSCRIBE", *channels]) unless channels.empty?
      connection.write(["PUNSUBSCRIBE", *patterns]) unless patterns.empty?
    rescue ConnectionError, IOError, SystemCallError
      # Closing anyway; best effort.
    end

    def current_connection!
      @mutex.synchronize do
        connection = @raw_connection
        raise ConnectionError, "Connection was closed or lost" if @closed || connection.nil?

        [connection, @generation]
      end
    end

    def current_generation?(generation)
      generation == @generation
    end

    # Record that the physical connection is gone.
    #
    # With an explicit `connection`, the loss only applies when it is still the
    # current connection: a failure on a superseded connection must not change
    # the state of the replacement.
    def mark_disconnected!(connection = @raw_connection)
      gone = @mutex.synchronize do
        current = @raw_connection
        next nil if current.nil?
        next nil if connection && !connection.equal?(current)

        @raw_connection = nil
        @confirmed_channels.clear
        @confirmed_patterns.clear
        open_gap
        @disconnects += 1
        current
      end

      close_quietly(gone)
    end

    def close_quietly(connection)
      connection&.close
    rescue IOError, SystemCallError
      nil
    end

    def open_gap
      return @gap if @gap

      @gap = Gap.new(generation: @generation, started_at: RedisClient.now)
      @gaps << @gap
      @gap
    end

    def close_gap
      return unless @gap

      @gap._close!(RedisClient.now)
      @gap = nil
    end

    def settled_state?
      @desired_channels.each_key.all? { |channel| @confirmed_channels.key?(channel) } &&
        @desired_patterns.each_key.all? { |pattern| @confirmed_patterns.key?(pattern) } &&
        @confirmed_channels.each_key.all? { |channel| @desired_channels.key?(channel) } &&
        @confirmed_patterns.each_key.all? { |pattern| @desired_patterns.key?(pattern) }
    end

    def subscription_set(channels_hash, patterns_hash)
      SubscriptionSet.new(channels_hash.keys, patterns_hash.keys)
    end
  end
end
