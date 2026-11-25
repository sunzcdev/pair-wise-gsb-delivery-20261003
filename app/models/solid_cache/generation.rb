# frozen_string_literal: true

module SolidCache
  # Stores the current write generation for a cache namespace on a single shard.
  #
  # A namespace-scoped, point-in-time clear bumps the generation. Entries written
  # afterwards carry the new generation, while entries that existed at the time of
  # the clear keep the previous one and can therefore be swept precisely.
  class Generation < Record
    self.table_name = "solid_cache_generations"

    # Namespace used to track the generation of stores that run without an
    # explicit Active Support cache namespace.
    DEFAULT_NAMESPACE = "_solid_cache_default"

    class << self
      # Returns the generation currently stamped onto writes for +namespace+.
      # Defaults to 0 when no clear has ever happened for the namespace.
      def current(namespace)
        without_query_cache do
          where(namespace: namespace).pick(:generation) || 0
        end
      end

      # Atomically bumps the generation for +namespace+ and returns the new
      # value. The row is locked for the duration of the transaction so
      # concurrent clears on the same namespace can never interleave or lose an
      # increment. A unique-index race on first insert is retried once.
      def bump(namespace)
        without_query_cache do
          retried = false
          begin
            transaction do
              marker = where(namespace: namespace).lock.first
              marker ||= new(namespace: namespace, generation: 0)
              marker.generation += 1
              marker.save!
              marker.generation
            end
          rescue ActiveRecord::RecordNotUnique
            raise if retried

            retried = true
            retry
          end
        end
      end

      private
        def without_query_cache(&block)
          uncached(dirties: false, &block)
        end
    end
  end
end

ActiveSupport.run_load_hooks :solid_cache_generation, SolidCache::Generation
