# frozen_string_literal: true

module ActiveRecord
  module ConnectionAdapters
    class StatementPool # :nodoc:
      include Enumerable

      DEFAULT_STATEMENT_LIMIT = 1000

      def initialize(statement_limit = nil)
        @cache = Hash.new { |h, pid| h[pid] = {} }
        @metadata = Hash.new { |h, pid| h[pid] = {} }
        @statement_limit = statement_limit || DEFAULT_STATEMENT_LIMIT
      end

      def each(&block)
        cache.each(&block)
      end

      def key?(key)
        cache.key?(key)
      end

      def [](key)
        cache[key]
      end

      def length
        cache.length
      end

      # The adapter specific bookkeeping attached to +sql+ (for example the set
      # of schema objects a prepared statement depends on, or +nil+ when the
      # adapter could not explain the dependencies). Interpreted only by the
      # adapter that stored it.
      def metadata(sql)
        metadata_cache[sql]
      end

      def []=(sql, stmt)
        while @statement_limit <= cache.size
          evicted_sql, evicted_stmt = cache.shift
          metadata_cache.delete(evicted_sql)
          dealloc(evicted_stmt)
        end
        cache[sql] = stmt
      end

      # Caches +stmt+ for +sql+, together with the bookkeeping the adapter needs
      # to decide later whether the statement is still valid.
      def store(sql, stmt, metadata = nil)
        self[sql] = stmt
        metadata_cache[sql] = metadata
        stmt
      end

      def clear
        cache.each_value do |stmt|
          dealloc stmt
        end
        cache.clear
        metadata_cache.clear
      end

      # Clear the pool without deallocating; this is only safe when we
      # know the server has independently deallocated all statements
      # (e.g. due to a reconnect, or a DISCARD ALL)
      def reset
        cache.clear
        metadata_cache.clear
      end

      def delete(key)
        metadata_cache.delete(key)
        if stmt = cache.delete(key)
          dealloc(stmt)
        end
        stmt
      end

      private
        def cache
          @cache[Process.pid]
        end

        def metadata_cache
          @metadata[Process.pid]
        end

        def dealloc(stmt)
          raise NotImplementedError
        end
    end
  end
end
