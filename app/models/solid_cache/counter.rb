# frozen_string_literal: true

module SolidCache
  # A per-shard counter, stored in the +solid_cache_counters+ table.
  #
  # Counters live on the same connection (and therefore the same shard) as
  # the cache entries, so each shard keeps its own independent count.
  #
  # Updates are single atomic statements that join any surrounding
  # transaction: if that transaction rolls back, the counter is left
  # untouched.
  class Counter < Record
    self.table_name = "solid_cache_counters"

    class << self
      def increment(name, amount: 1)
        amount = amount.to_i

        # A single UPDATE is atomic, so concurrent increments can't lose
        # each other. The row only needs to be created on the first write;
        # if two processes race to create it, one increment may be lost,
        # which is acceptable for an estimate.
        if where(name: name).update_all([ "value = value + ?", amount ]) == 0
          upsert({ name: name, value: amount }, unique_by: upsert_unique_by, on_duplicate: :update, update_only: [ :value ])
        end
      end

      def set(name, amount:)
        upsert({ name: name, value: amount }, unique_by: upsert_unique_by, on_duplicate: :update, update_only: [ :value ])
      end

      def read(name)
        without_query_cache do
          where(name: name).pick(:value) || 0
        end
      end

      private
        def upsert_unique_by
          with_connection do |connection|
            connection.supports_insert_conflict_target? ? :name : nil
          end
        end

        def without_query_cache(&block)
          uncached(dirties: false, &block)
        end
    end
  end
end
