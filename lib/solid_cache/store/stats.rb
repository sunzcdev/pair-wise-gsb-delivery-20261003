# frozen_string_literal: true

module SolidCache
  class Store
    module Stats
      def initialize(options = {})
        super(options)
        @key_hash_collisions = {}
        @key_hash_collisions_mutex = Mutex.new
      end

      def stats
        {
          connections: connections.count,
          connection_stats: connections_stats,
          namespace_stats: namespace_stats
        }
      end

      private
        def connections_stats
          with_each_connection.to_h { |connection| [ Entry.current_shard, connection_stats ] }
        end

        def connection_stats
          oldest_created_at = Entry.order(:id).pick(:created_at)

          {
            max_age: max_age,
            oldest_age: oldest_created_at ? Time.now - oldest_created_at : nil,
            max_entries: max_entries,
            entries: Entry.id_range
          }
        end

        # Aggregates the size of each namespace across every shard and adds the
        # key hash collisions observed by this process. Namespaces from different
        # shards are merged by their digest identifier, but entries are never
        # merged into a shared bucket and cached values are never read.
        def namespace_stats
          groups = aggregate_namespace_groups
          groups = merge_collision_counts(groups)
          groups.sort_by { |group_id, _stats| group_id.to_s }.to_h
        end

        def aggregate_namespace_groups
          with_each_connection.reduce({}) do |groups, _connection|
            Entry.namespace_stats.each_with_object(groups) do |(group_id, group_stats), merged|
              merged[group_id] = merge_group_stats(merged[group_id], group_stats)
            end
          end
        end

        def merge_group_stats(existing, incoming)
          return incoming.dup unless existing

          existing.merge(incoming) { |_key, old_value, new_value| old_value + new_value }
        end

        def merge_collision_counts(groups)
          recorded_collisions.each do |group_id, count|
            groups[group_id] ||= empty_namespace_stats
            groups[group_id][:key_hash_collisions] = groups[group_id].fetch(:key_hash_collisions, 0) + count
          end
          groups.each_value do |group_stats|
            group_stats[:key_hash_collisions] ||= 0
          end
        end

        def empty_namespace_stats
          { entries: 0, byte_size: 0 }
        end

        def recorded_collisions
          @key_hash_collisions_mutex.synchronize { @key_hash_collisions.dup }
        end

        # Collisions are counted per namespace group. Only the digest group id and
        # a counter are retained: raw keys and values are never stored here.
        def track_key_hash_collisions(collisions)
          return if collisions.empty?

          counts = collisions.group_by do |collision|
            Entry.namespace_group_id(Entry.namespace_for(collision[:key]))
          end.transform_values(&:count)

          @key_hash_collisions_mutex.synchronize do
            counts.each do |group_id, count|
              @key_hash_collisions[group_id] = @key_hash_collisions.fetch(group_id, 0) + count
            end
          end
        end
    end
  end
end
