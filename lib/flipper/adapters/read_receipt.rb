module Flipper
  module Adapters
    # Public: Optional provenance record for failover reads. A Failover adapter
    # that is built with `receipt: true` collects one of these and updates it
    # every time a read is served from the primary or recovered from the
    # secondary. Reads performed through a memoizing adapter are stamped with
    # the :memo source so a value fetched during an outage can be told apart
    # from a value that was actively read from a healthy adapter.
    #
    # Each feature key maps to an Entry describing the most recent read of
    # that key. Batch-level exceptions are additionally appended to
    # #failures so every failure point can be audited even after the primary
    # has recovered.
    class ReadReceipt
      # Public: Where a readable value came from.
      #
      # :primary   - actively read from the healthy primary adapter.
      # :secondary - served by the secondary after the primary raised.
      # :memo      - reused from the memoizer cache rather than read.
      SOURCES = [:primary, :secondary, :memo].freeze

      # Public: What happened on the most recent read of a key.
      #
      # :ok       - the primary returned a stored value.
      # :missing  - the primary explicitly had no value (the default config
      #             was returned); a stale secondary value must not replace it.
      # :degraded - the primary raised and the secondary value was used.
      STATUSES = [:ok, :missing, :degraded].freeze

      # Internal: Keys that are not feature names but still get entries.
      FEATURES_KEY = :__flipper_receipt_features__
      SYNC_VERSION_KEY = :__flipper_receipt_sync_version__

      # Public: A single key's most recent provenance. When a value is reused
      # from the memoizer cache, source becomes :memo and original_source /
      # original_status preserve where the value was actively read from.
      Entry = Struct.new(:key, :source, :status, :value, :error, :operation, :original_source, :original_status, keyword_init: true) do
        def degraded?
          status == :degraded
        end

        def missing?
          status == :missing
        end

        def primary?
          source == :primary
        end

        def secondary?
          source == :secondary
        end

        def memo?
          source == :memo
        end

        # Public: The source the value was actively read from, even if the
        # most recent access reused the memoizer cache.
        def active_source
          original_source || source
        end

        # Public: The status of the active read, even if the most recent
        # access reused the memoizer cache.
        def active_status
          original_status || status
        end
      end

      # Public: A primary failure observed during a read operation.
      Failure = Struct.new(:operation, :keys, :error, keyword_init: true)

      def initialize
        @entries = {}
        @failures = []
        @lock = Mutex.new
      end

      # Public: Returns the Entry for a feature key (or a reserved key such as
      # ReadReceipt.features_key), nil if the key was never read.
      def for(key)
        @lock.synchronize { @entries[key.to_s] }
      end
      alias_method :[], :for

      # Public: Yields each recorded feature key with its Entry.
      def each(&block)
        @lock.synchronize { @entries.each(&block) }
      end

      # Public: Returns the Hash of feature key to Entry (excluding reserved
      # entries such as the features set).
      def entries
        @lock.synchronize do
          @entries.reject { |key, _| key.start_with?("__flipper_receipt_") }.dup
        end
      end

      # Public: Returns all primary failures observed during reads, oldest
      # first.
      attr_reader :failures

      # Public: Convenience accessors for the reserved entries.
      def features_entry
        self[FEATURES_KEY]
      end

      def sync_version_entry
        self[SYNC_VERSION_KEY]
      end

      # Internal: Record the most recent read of one or more keys.
      #
      # keys      - Array of String/Symbol keys that were read.
      # source    - Symbol in SOURCES.
      # status    - Symbol in STATUSES.
      # operation - Symbol adapter operation (e.g. :get, :get_multi).
      # values    - Hash of key to the readable value, or a single value for
      #             non-feature reads.
      # error     - The exception that triggered failover, if any.
      #
      # Returns nothing.
      def record(keys, source:, status:, operation:, values: nil, error: nil)
        @lock.synchronize do
          Array(keys).each do |key|
            key = key.to_s
            value = values.is_a?(Hash) ? values[key] : values
            @entries[key] = Entry.new(
              key: key,
              source: source,
              status: status,
              value: value,
              error: error,
              operation: operation,
            )
          end
        end
        nil
      end

      # Internal: Append a batch-level primary failure.
      def record_failure(operation, keys, error)
        @lock.synchronize do
          @failures << Failure.new(
            operation: operation,
            keys: Array(keys).map(&:to_s),
            error: error,
          )
        end
        nil
      end

      # Internal: Mark an existing entry as served from the memoizer cache.
      # The original source/status/error are preserved on the entry so an
      # actively-read value can always be told apart from a reused one and so
      # #restore can put the original provenance back later.
      def restamp(key, operation)
        @lock.synchronize do
          key = key.to_s
          entry = @entries[key]
          return unless entry

          entry.original_source ||= entry.source
          entry.original_status ||= entry.status
          entry.source = :memo
          entry.operation = operation
        end
        nil
      end

      # Internal: Put the pre-memo provenance back on an entry after the key
      # is actively read again.
      def restore(key)
        @lock.synchronize do
          key = key.to_s
          entry = @entries[key]
          return unless entry

          if entry.original_source
            entry.source = entry.original_source
            entry.status = entry.original_status
            entry.original_source = nil
            entry.original_status = nil
          end
        end
        nil
      end

      # Internal: Reserved key used for Adapter#features reads.
      def self.features_key
        FEATURES_KEY
      end

      # Internal: Reserved key used for Adapter#sync_version reads.
      def self.sync_version_key
        SYNC_VERSION_KEY
      end

      # Public: Resets all collected provenance.
      def clear
        @lock.synchronize do
          @entries.clear
          @failures.clear
        end
      end

      def inspect
        "#<#{self.class.name} entries=#{entries.keys.inspect} failures=#{@failures.size}>"
      end
    end
  end
end
