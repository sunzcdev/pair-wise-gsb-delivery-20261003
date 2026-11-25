require "set"
require "flipper/adapter"
require "flipper/typecast"

module Flipper
  module Adapters
    # Public: Adapter that wraps another adapter (typically a Poll adapter) and
    # lets an admin temporarily override one or more features without waiting on
    # a remote sync. Each override carries an explicit expiry and a recorded
    # source, so reads can say whether they are a local decision or the remote
    # version.
    #
    # While an override is active it wins over the wrapped adapter. When it
    # expires (or is removed) reads fall through to the wrapped adapter, which
    # keeps advancing with its own polls. Other features are never touched by an
    # override and always follow the wrapped adapter.
    #
    # Examples
    #
    #   poll = Flipper::Adapters::Poll.new(poller, local_adapter)
    #   adapter = Flipper::Adapters::Override.new(poll)
    #
    #   # Temporarily force a feature on for one minute.
    #   adapter.enable_override(:search, ttl: 60, source: "admin:123")
    #
    #   adapter.source(:search)       # => :local
    #   adapter.expires_at(:search)   # => 1732521660.0
    #   adapter.override?(:search)    # => true
    #
    #   # After the minute passes, the override is gone and the just-polled
    #   # remote values show through again.
    #   adapter.source(:search)       # => :remote
    class Override
      include ::Flipper::Adapter

      # Public: Source marker for values decided by a local override.
      LOCAL_SOURCE = :local

      # Public: Source marker for values coming from the wrapped adapter.
      REMOTE_SOURCE = :remote

      # Public: A single feature override with its deadline and provenance.
      class Entry
        attr_reader :key, :value, :expires_at, :source

        def initialize(key, value, expires_at:, source:)
          @key = key
          @value = value
          @expires_at = expires_at
          @source = source
        end

        # Public: Is this override still in effect at the given monotonic time?
        def active?(now)
          expires_at.nil? || now < expires_at
        end

        # Public: A copy that won't mutate this entry when changed.
        def dup
          self.class.new(key, Override.deep_dup_value(value), expires_at: expires_at, source: source)
        end
      end

      # Public: The adapter whose values are used when no override is active.
      attr_reader :adapter

      # Public: Build a new override adapter.
      #
      # adapter - The wrapped Flipper adapter, usually Flipper::Adapters::Poll.
      # options - The Hash of options:
      #           :clock - Object responding to #now, returning seconds as a
      #                    Numeric. Defaults to wall-clock Time. Inject a fake
      #                    clock in tests to make overrides expire deterministically.
      def initialize(adapter, options = {})
        @adapter = adapter
        @clock = options.fetch(:clock, Clock)
        @overrides = {}
        @lock = Mutex.new
      end

      #
      # Reading
      #

      def features
        wrapped = @adapter.features
        synchronize do
          prune
          (wrapped.to_a + @overrides.keys).uniq.to_set
        end
      end

      def get(feature)
        key = key_for(feature)
        synchronize do
          if entry = active_override(key)
            return deep_dup_value(entry.value)
          end
        end
        @adapter.get(feature)
      end

      def get_multi(features)
        result = {}
        remaining = []

        synchronize do
          prune
          features.each do |feature|
            key = key_for(feature)
            if entry = active_override(key)
              result[key] = deep_dup_value(entry.value)
            else
              remaining << feature
            end
          end
        end

        @adapter.get_multi(remaining).each { |key, value| result[key] = value } unless remaining.empty?
        result
      end

      def get_all(**kwargs)
        synchronize { prune }
        result = @adapter.get_all(**kwargs)
        synchronize do
          @overrides.each do |key, entry|
            result[key] = deep_dup_value(entry.value)
          end
        end
        result
      end

      #
      # Writing
      #
      # Writes that come through the normal Flipper API go straight to the
      # wrapped adapter (the remote-backed source of truth); they do not create
      # overrides. Use #set_override and friends for local decisions.
      #

      def add(feature)
        @adapter.add(feature)
      end

      def remove(feature)
        synchronize { @overrides.delete(key_for(feature)) }
        @adapter.remove(feature)
      end

      def clear(feature)
        synchronize { @overrides.delete(key_for(feature)) }
        @adapter.clear(feature)
      end

      def enable(feature, gate, thing)
        @adapter.enable(feature, gate, thing)
      end

      def disable(feature, gate, thing)
        @adapter.disable(feature, gate, thing)
      end

      #
      # Override management
      #

      # Public: Record a local override for a feature.
      #
      # feature - The Flipper::Feature or String/Symbol feature key.
      # gates   - The Hash of gate values to serve while active. When omitted,
      #           the feature's boolean gate is forced on.
      # ttl:          - Numeric seconds the override stays active.
      # expires_at:   - Absolute deadline (seconds from the clock). Mutually
      #                 exclusive with :ttl. Neither given means no expiry.
      # source:       - Who/what made the decision (default: "local").
      #
      # Returns the Entry that was stored.
      def set_override(feature, gates = nil, ttl: nil, expires_at: nil, source: "local")
        raise ArgumentError, "provide either :ttl or :expires_at, not both" if ttl && expires_at

        key = key_for(feature)
        value = deep_dup_value(gates || default_boolean_gates)
        deadline =
          if ttl
            now + ttl.to_f
          elsif expires_at
            expires_at.to_f
          end

        entry = Entry.new(key, value, expires_at: deadline, source: source)
        synchronize { @overrides[key] = entry }
        entry
      end

      # Public: Force a feature's boolean gate on (or to the given value) for a
      # bounded time. Convenience wrapper around #set_override.
      def enable_override(feature, value = true, ttl: nil, expires_at: nil, source: "local")
        set_override(feature, default_boolean_gates(value), ttl: ttl, expires_at: expires_at, source: source)
      end

      # Public: Force a feature fully off for a bounded time.
      def disable_override(feature, ttl: nil, expires_at: nil, source: "local")
        set_override(feature, default_config, ttl: ttl, expires_at: expires_at, source: source)
      end

      # Public: Remove the local override for a feature. Future reads return to
      # the wrapped adapter's current values.
      #
      # Returns the removed Entry, or nil when there was nothing to remove.
      def remove_override(feature)
        synchronize do
          prune
          @overrides.delete(key_for(feature))
        end
      end
      alias_method :clear_override, :remove_override

      # Public: Remove every local override, including expired ones.
      def clear_overrides
        synchronize { @overrides.clear }
      end

      # Public: The active Entry for a feature, or nil. Expired entries are
      # removed and treated as absent.
      def override_for(feature)
        synchronize do
          prune
          entry = @overrides[key_for(feature)]
          entry&.dup
        end
      end

      # Public: Is a local override currently deciding this feature?
      def override?(feature)
        !override_for(feature).nil?
      end

      # Public: Where the current value for a feature is decided:
      # :local while an override is active, :remote otherwise.
      def source(feature)
        override?(feature) ? LOCAL_SOURCE : REMOTE_SOURCE
      end

      # Public: The clock time at which the override expires, or nil when the
      # feature has no active override (including an override with no expiry).
      def expires_at(feature)
        override_for(feature)&.expires_at
      end

      # Public: Who recorded the active override, or nil when there is none.
      def override_source(feature)
        override_for(feature)&.source
      end

      # Public: All currently active overrides as a Hash of feature key => Entry.
      def overrides
        synchronize do
          prune
          @overrides.each_with_object({}) { |(key, entry), memo| memo[key] = entry.dup }
        end
      end

      def read_only?
        @adapter.read_only?
      end

      def adapter_stack
        "override -> #{@adapter.adapter_stack}"
      end

      private

      def key_for(feature)
        feature.respond_to?(:key) ? feature.key.to_s : feature.to_s
      end

      def active_override(key)
        entry = @overrides[key]
        return nil if entry.nil?

        if entry.active?(now)
          entry
        else
          @overrides.delete(key)
          nil
        end
      end

      # Internal: Drop every expired override. Call under @lock.
      def prune
        current = now
        @overrides.delete_if { |_, entry| !entry.active?(current) }
      end

      def now
        @clock.now.to_f
      end

      def default_boolean_gates(value = true)
        default_config.merge(boolean: value ? "true" : nil)
      end

      def synchronize(&block)
        if @lock
          @lock.synchronize(&block)
        else
          block.call
        end
      end

      # Internal: Deep copy gate hashes so callers can never mutate stored
      # overrides through a returned reference.
      def self.deep_dup_value(value)
        case value
        when Hash
          value.each_with_object({}) { |(k, v), memo| memo[k] = deep_dup_value(v) }
        when Set
          value.dup
        when Array
          value.map { |item| deep_dup_value(item) }
        else
          value.dup rescue value
        end
      end

      # Internal: Deep copy gate hashes so callers can never mutate stored
      # overrides through a returned reference.
      def deep_dup_value(value)
        self.class.deep_dup_value(value)
      end

      # Internal: Default wall clock; inject a fake one in specs.
      module Clock
        def self.now
          Time.now.to_f
        end
      end
    end
  end
end
