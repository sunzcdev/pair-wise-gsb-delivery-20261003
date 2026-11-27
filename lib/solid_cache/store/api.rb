# frozen_string_literal: true

module SolidCache
  class Store
    module Api
      DEFAULT_MAX_KEY_BYTESIZE = 1024
      SQL_WILDCARD_CHARS = [ "_", "%" ]

      attr_reader :max_key_bytesize

      def initialize(options = {})
        super(options)

        @max_key_bytesize = options.fetch(:max_key_bytesize, DEFAULT_MAX_KEY_BYTESIZE)
      end

      def increment(name, amount = 1, options = nil)
        options = merged_options(options)
        key = normalize_key(name, options)

        instrument :increment, key, amount: amount do
          adjust(name, amount, options)
        end
      end

      def decrement(name, amount = 1, options = nil)
        options = merged_options(options)
        key = normalize_key(name, options)

        instrument :decrement, key, amount: amount do
          adjust(name, -amount, options)
        end
      end

      # Adjusts a set of counters in one call, taking positive (increment) and negative
      # (decrement) amounts:
      #
      #   result = cache.adjust_multi({ "views" => 1, "credits" => -5 }, token: "order-123")
      #   result.complete?            # => false if a shard was unavailable
      #   result.unavailable          # => ["credits"]
      #   result.retry                # re-sends only the counters that did not land
      #
      # The counters are grouped by shard and each group is applied with the same locked
      # read-modify-write that #increment uses, so two batches touching one counter both
      # land instead of overwriting each other. A shard whose connection is unavailable is
      # reported on its own; counters on healthy shards are still applied.
      #
      # When a +token+ is given, a receipt is written in the same transaction as each
      # counter. Re-sending the same batch with the same token therefore does not apply an
      # adjustment twice: the counters that already landed are reported as
      # +:already_applied+ and only the outstanding ones are changed. Without a token a
      # fresh one is generated, which still lets Result#retry resend only what is missing.
      #
      # Returns a SolidCache::Store::Adjustment::Result.
      def adjust_multi(adjustments, token: nil, **options)
        options = merged_options(options)
        changes = normalize_adjustments(adjustments, options)
        token = (token || SecureRandom.hex(16)).to_s

        if changes.empty?
          return Adjustment::Result.new(store: self, token: token, changes: [], options: options, outcomes: {})
        end

        outcomes = {}

        instrument_multi :adjust_multi, changes.map(&:name), options do |payload|
          entry_adjust_multi(changes, token: token, options: options).each do |connection, grouped_changes, resolutions, error|
            grouped_changes.each do |change|
              outcomes[change.key] =
                if error
                  Adjustment::Outcome.new(name: change.name, shard: connection, status: Adjustment::UNAVAILABLE, error: error)
                else
                  status, value = resolutions.fetch(change.key)
                  Adjustment::Outcome.new(name: change.name, shard: connection, status: status, value: value)
                end
            end
          end

          payload[:statuses] = changes.to_h { |change| [ change.name, outcomes.fetch(change.key).status ] }
        end

        write_adjustments_to_local_cache(changes, outcomes, options)

        Adjustment::Result.new(store: self, token: token, changes: changes, options: options, outcomes: outcomes)
      end

      def cleanup(options = nil)
        raise NotImplementedError.new("#{self.class.name} does not support cleanup")
      end

      def clear(options = nil)
        entry_clear
      end

      private
        def read_entry(key, **options)
          deserialize_entry(read_serialized_entry(key, **options), **options)
        end

        def read_serialized_entry(key, **options)
          entry_read(key)
        end

        def write_entry(key, entry, raw: false, unless_exist: false, **options)
          payload = serialize_entry(entry, raw: raw, **options)

          if unless_exist
            written = false
            entry_lock_and_write(key) do |value|
              if value.nil? || deserialize_entry(value, **options).expired?
                written = true
                payload
              end
            end
          else
            written = entry_write(key, payload)
          end

          write_serialized_entry(key, payload, raw: raw, returning: written, **options)
          written
        end

        def write_serialized_entry(key, payload, raw: false, unless_exist: false, expires_in: nil, race_condition_ttl: nil, returning: true, **options)
          returning
        end

        def read_serialized_entries(keys)
          entry_read_multi(keys).reduce(&:merge!)
        end

        def read_multi_entries(names, **options)
          keys_and_names = names.index_by { |name| normalize_key(name, options) }
          serialized_entries = read_serialized_entries(keys_and_names.keys)

          keys_and_names.each_with_object({}) do |(key, name), results|
            serialized_entry = serialized_entries[key]
            entry = deserialize_entry(serialized_entry, **options)

            next unless entry

            version = normalize_version(name, options)

            if entry.expired?
              delete_entry(key, **options)
            elsif !entry.mismatched?(version)
              if defined? ActiveSupport::Cache::DeserializationError
                begin
                  results[name] = entry.value
                rescue ActiveSupport::Cache::DeserializationError
                end
              else
                results[name] = entry.value
              end
            end
          end
        end

        def write_multi_entries(entries, expires_in: nil, **options)
          if entries.any?
            serialized_entries = serialize_entries(entries, **options)
            # to add them to the local cache
            serialized_entries.each do |entries|
              write_serialized_entry(entries[:key], entries[:value])
            end

            entry_write_multi(serialized_entries).all?
          end
        end

        def delete_entry(key, **options)
          entry_delete(key)
        end

        def delete_multi_entries(entries, **options)
          entry_delete_multi(entries).compact.sum
        end

        def serialize_entry(entry, raw: false, **options)
          super(entry, raw: raw, **options)
        end

        def serialize_entries(entries, **options)
          entries.map do |key, entry|
            { key: key, value: serialize_entry(entry, **options) }
          end
        end

        def deserialize_entry(payload, **)
          super(payload)
        end

        def normalize_key(key, options)
          truncate_key super&.b
        end

        def truncate_key(key)
          if key && key.bytesize > max_key_bytesize
            suffix = ":hash:#{ActiveSupport::Digest.hexdigest(key)}"
            truncate_at = max_key_bytesize - suffix.bytesize
            "#{key.byteslice(0, truncate_at)}#{suffix}".b
          else
            key
          end
        end

        def adjust(name, amount, options)
          options = merged_options(options)
          key = normalize_key(name, options)

          new_value = entry_lock_and_write(key) do |value|
            serialize_entry(adjusted_entry(value, amount, options))
          end
          deserialize_entry(new_value, **options).value if new_value
        end

        def adjusted_entry(value, amount, options)
          entry = deserialize_entry(value, **options)

          if entry && !entry.expired?
            ActiveSupport::Cache::Entry.new \
              amount + entry.value.to_i, **options.dup.merge(expires_in: nil, expires_at: entry.expires_at)
          elsif /\A\d+\z/.match?(value)
            # This is to match old raw values
            ActiveSupport::Cache::Entry.new(amount + value.to_i, **options)
          else
            ActiveSupport::Cache::Entry.new(amount, **options)
          end
        end

        # Normalizes the requested counters, folding together any names that resolve to the
        # same stored key (a truncated or namespaced collision) by summing their amounts, so
        # one request never silently drops an adjustment.
        def normalize_adjustments(adjustments, options)
          adjustments.each_with_object({}) do |(name, amount), changes|
            unless amount.is_a?(Integer)
              raise ArgumentError, "Adjustment for #{name.inspect} must be an Integer, got #{amount.class}"
            end

            key = normalize_key(name, options)
            existing = changes[key]

            changes[key] = if existing
              existing.dup.tap { |change| change.amount += amount }
            else
              Adjustment::Change.new(name: name, key: key, amount: amount)
            end
          end.values
        end

        # Applies one shard's grouped adjustments inside a single locked transaction.
        #
        # Each key is checked against the batch receipt stored in this shard. If a receipt
        # for +token+ is already present the whole group was applied by an earlier attempt,
        # so every key is reported as +:already_applied+ with its stored value. Otherwise
        # the counters are adjusted and the receipt is written in the same transaction, so
        # a shard is never left half-applied.
        #
        # Returns [resolutions, writes], where +writes+ is the payloads to persist.
        def resolve_adjustments(changes, payloads, receipt_payload, receipt_key, token, options)
          already_applied = receipt_payload && deserialize_entry(receipt_payload, **options)&.value == token

          if already_applied
            resolutions = changes.to_h { |change| [ change.key, [ Adjustment::ALREADY_APPLIED, counter_value(payloads[change.key], options) ] ] }
            [ resolutions, {} ]
          else
            entries = changes.to_h { |change| [ change.key, adjusted_entry(payloads[change.key], change.amount, options) ] }
            resolutions = entries.transform_values { |entry| [ Adjustment::APPLIED, entry.value ] }
            writes = entries.transform_values { |entry| serialize_entry(entry, **options) }
            writes[receipt_key] = serialize_entry(receipt_entry(token, options), **options)

            [ resolutions, writes ]
          end
        end

        # A receipt records that one batch token has already been applied in a shard. It is
        # stored without an expiry of its own so it outlives the counters it guards and is
        # removed by the same FIFO expiry that removes them.
        def receipt_key_for(token, options)
          normalize_key("__solid_cache_adjustment__/#{token}", options)
        end

        def receipt_entry(token, options)
          ActiveSupport::Cache::Entry.new(token, **options.dup.merge(expires_in: nil, expires_at: nil))
        end

        def counter_value(payload, options)
          deserialize_entry(payload, **options)&.value&.to_i
        end

        # Keeps an enclosing local cache in step with the counters that landed, the same
        # way #increment does through ActiveSupport::Cache::Strategy::LocalCache.
        def write_adjustments_to_local_cache(changes, outcomes, options)
          return unless local_cache

          changes.each do |change|
            outcome = outcomes[change.key]
            next unless outcome&.applied?

            write_cache_value(change.name, outcome.value, raw: true, **options)
          end
        end
    end
  end
end
