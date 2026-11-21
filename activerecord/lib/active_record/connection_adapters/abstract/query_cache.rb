# frozen_string_literal: true

require "concurrent/map"
require "concurrent/atomic/atomic_fixnum"

module ActiveRecord
  module ConnectionAdapters # :nodoc:
    module QueryCache
      DEFAULT_SIZE = 100 # :nodoc:

      class << self
        def enabled_by_default # :nodoc:
          ActiveSupport::IsolatedExecutionState[:active_record_query_cache_enabled] || false
        end

        def enabled_by_default=(enabled) # :nodoc:
          ActiveSupport::IsolatedExecutionState[:active_record_query_cache_enabled] = enabled
        end

        def included(base) # :nodoc:
          dirties_query_cache base, :exec_query, :execute, :create, :insert, :update, :update_with_result,
            :delete, :truncate, :truncate_tables, :rollback_to_savepoint, :rollback_db_transaction,
            :restart_db_transaction, :exec_insert_all
        end

        def dirties_query_cache(base, *method_names)
          method_names.each do |method_name|
            base.class_eval <<-end_code, __FILE__, __LINE__ + 1
              def #{method_name}(...)
                if pool.dirties_query_cache
                  ActiveRecord::Base.clear_query_caches_for_current_thread
                end
                super
              end
            end_code
          end
        end
      end

      # This is the actual query cache store.
      #
      # It has an internal hash whose keys are either SQL strings, or arrays of
      # two elements [SQL string, binds], if there are binds. The hash values
      # are Entry objects, which pair their corresponding ActiveRecord::Result
      # with the transaction scope that produced it.
      #
      # Keeping the hash size under max size is achieved with LRU eviction.
      #
      # The store gets passed a version object, which is shared among the query
      # cache stores of a given connection pool (see ConnectionPoolConfiguration
      # down below). The version value may be externally changed as a way to
      # signal cache invalidation, that is why all methods have a guard for it.
      #
      # Transaction visibility
      # ---------------------
      #
      # A cached result is only reusable while the transaction scope that
      # produced it is still part of the connection's view. Every transaction
      # and savepoint registers itself as a scope (#begin_scope). Writes
      # invalidate the current view (#clear), checkpointing it against each open
      # scope first. Rolling a scope back therefore restores what the enclosing
      # scope was seeing (#rollback_scope) instead of dropping the whole cache,
      # and committing one adopts the entries it produced for the enclosing
      # scope while discarding its checkpoint (#commit_scope).
      class Store # :nodoc:
        # Describes where a cached result was produced: the statement that
        # originally ran and the transaction or savepoint it ran in, if any.
        class Provenance # :nodoc:
          attr_accessor :scope
          attr_reader :sql, :name, :savepoint, :transaction_uuid

          def initialize(sql, name, scope, savepoint, transaction_uuid)
            @sql = sql
            @name = name
            @scope = scope
            @savepoint = savepoint
            @transaction_uuid = transaction_uuid
          end

          def to_h
            { sql: sql, name: name, savepoint: savepoint, transaction_uuid: transaction_uuid }
          end
        end

        Entry = Struct.new(:result, :provenance) # :nodoc:
        Scope = Struct.new(:token, :snapshot) # :nodoc:

        attr_accessor :enabled, :dirties
        alias_method :enabled?, :enabled
        alias_method :dirties?, :dirties

        def initialize(version, max_size)
          @version = version
          @current_version = version.value
          @map = {}
          @max_size = max_size
          @enabled = false
          @dirties = true
          @scopes = []
        end

        def size
          check_version
          @map.size
        end

        def empty?
          check_version
          @map.empty?
        end

        # Returns the cached ActiveRecord::Result for +key+, or nil.
        def [](key)
          lookup(key)&.result
        end

        # Returns the Entry for +key+, or nil.
        #
        # Entries are served in LRU order, but only when the transaction scope
        # that produced them is still visible.
        def lookup(key)
          check_version
          return unless @enabled

          entry = @map.delete(key)
          return unless entry && visible?(entry)

          @map[key] = entry
        end

        # Returns a two element array with the Entry for +key+ and whether it was
        # a hit. The block must return the Entry to cache.
        def fetch(key)
          check_version

          if entry = lookup(key)
            [entry, true]
          else
            [store(key, yield), false]
          end
        end

        # Returns the cached ActiveRecord::Result for +key+, computing it with the
        # given block unless a visible one is already cached.
        def compute_if_absent(key)
          check_version

          if (entry = lookup(key))
            entry.result
          else
            store(key, Entry.new(yield)).result
          end
        end

        # Stores +entry+ under +key+. It's a no-op when the cache is disabled.
        def store(key, entry)
          check_version
          return entry unless @enabled

          @map.delete(key)
          @map.shift if @max_size && @map.size >= @max_size # evict the oldest entry
          @map[key] = entry
        end

        # The transaction scope that results cached right now belong to, if any.
        def current_scope
          @scopes.last&.token
        end

        # Registers +token+ as the innermost open transaction scope.
        #
        # The checkpoint itself is captured lazily, the first time any open scope
        # invalidates the cache (see #clear). At that point the cache still holds
        # exactly what that scope was seeing, which is what a rollback of the
        # scope has to give back.
        def begin_scope(token)
          check_version
          @scopes.push(Scope.new(token, nil)) unless scope_index(token)
          self
        end

        # The scope was committed: the view it leaves behind is still the
        # correct one for the enclosing scope, so its entries are adopted and
        # the checkpoint is dropped.
        def commit_scope(token)
          check_version
          if (index = scope_index(token))
            adopt_scopes_from(index)
            drop_scopes_from(index)
          end
          self
        end

        # The scope was rolled back: the enclosing scope's view is visible
        # again, so the checkpoint is restored. Anything cached by the rolled
        # back scope itself is meaningless now, whether or not it invalidated
        # the cache first.
        def rollback_scope(token)
          check_version
          if (index = scope_index(token))
            dropped = @scopes[index..].map(&:token)
            restore(@scopes[index])
            @map.delete_if { |_, entry| dropped.any? { |scope| scope.equal?(entry.provenance&.scope) } }
            drop_scopes_from(index)
          end
          self
        end

        # The scope's work was discarded but the scope itself is still open, as
        # when the parent transaction is restarted. The enclosing view is
        # restored and the checkpoint dropped, so later writes can track a fresh
        # one for the scope's eventual outcome.
        def restart_scope(token)
          check_version
          if (index = scope_index(token))
            restore(@scopes[index])
            @scopes[index].snapshot = nil
          end
          self
        end

        # Invalidates the current view, whether because of a write or an
        # explicit #clear_query_cache.
        #
        # Every open scope that hasn't got a checkpoint yet takes one now, since
        # the entries about to be dropped are still what that scope could see.
        # A later rollback of any of them can then restore its own view instead
        # of leaving the whole cache unusable.
        def clear
          @scopes.each { |scope| scope.snapshot ||= @map.dup }
          reset
        end

        # Discards everything, including the registered scopes. Used when their
        # transactions were abandoned without a decision, so no later rollback
        # can resurrect entries.
        def purge
          @scopes.clear
          reset
        end

        private
          def visible?(entry)
            scope = entry.provenance&.scope
            scope.nil? || !scope_index(scope).nil?
          end

          def restore(scope)
            @map = scope.snapshot.dup if scope.snapshot
            self
          end

          # Entries cached by the committed scope become part of the enclosing
          # scope's view. Without this they would be treated as orphaned once
          # their scope disappears.
          def adopt_scopes_from(index)
            adopting = index.zero? ? nil : @scopes[index - 1].token
            dropped = @scopes[index..].map(&:token)
            @map.each_value do |entry|
              provenance = entry.provenance
              provenance.scope = adopting if provenance && dropped.any? { |token| token.equal?(provenance.scope) }
            end
          end

          def scope_index(token)
            @scopes.rindex { |scope| scope.token.equal?(token) }
          end

          def drop_scopes_from(index)
            @scopes.slice!(index, @scopes.size - index)
          end

          def reset
            @map.clear
            self
          end

          # Another thread sharing this cache invalidated it, so no view we kept
          # describes it anymore. The open scopes themselves are still valid
          # though, they just need to checkpoint again on their next write.
          def check_version
            if @current_version != @version.value
              reset
              @scopes.each { |scope| scope.snapshot = nil }
              @current_version = @version.value
            end
          end
      end

      module ConnectionPoolConfiguration # :nodoc:
        def initialize(...)
          super
          @query_cache_version = Concurrent::AtomicFixnum.new
          @query_cache_max_size = \
            case query_cache = db_config&.query_cache
            when 0, false
              nil
            when Integer
              query_cache
            when nil
              DEFAULT_SIZE
            end
        end

        def checkout_and_verify(connection)
          super
          connection.query_cache ||= query_cache
          connection
        end

        # Disable the query cache within the block.
        def disable_query_cache(dirties: true)
          cache = query_cache
          old_enabled, cache.enabled, old_dirties, cache.dirties = cache.enabled, false, cache.dirties, dirties
          begin
            yield
          ensure
            cache.enabled, cache.dirties = old_enabled, old_dirties
          end
        end

        def enable_query_cache
          cache = query_cache
          old_enabled, cache.enabled, old_dirties, cache.dirties = cache.enabled, true, cache.dirties, true
          begin
            yield
          ensure
            cache.enabled, cache.dirties = old_enabled, old_dirties
          end
        end

        def enable_query_cache!
          query_cache.enabled = true
          query_cache.dirties = true
        end

        def disable_query_cache!
          query_cache.enabled = false
          query_cache.dirties = true
        end

        def reset_query_cache! # :nodoc:
          prepare_query_cache(query_cache)
          clear_query_cache
        end

        def query_cache_enabled
          query_cache.enabled
        end

        def dirties_query_cache
          query_cache.dirties
        end

        def clear_query_cache
          if @pinned_connection
            # With transactional fixtures, and especially systems test
            # another thread may use the same connection, but with a different
            # query cache. So we must clear them all.
            @query_cache_version.increment
          end
          query_cache.clear
        end

        def query_cache
          ConnectionPool.used_pools!.query_cache_for(self)
        end

        def build_query_cache # :nodoc:
          Store.new(@query_cache_version, @query_cache_max_size).tap { |cache| prepare_query_cache(cache) }
        end

        def prepare_query_cache(cache) # :nodoc:
          cache.enabled = QueryCache.enabled_by_default && db_config&.query_cache != false
          cache.dirties = true
          cache.purge
        end
      end

      def initialize(*)
        super
        @query_cache = nil
      end

      attr_writer :query_cache

      def query_cache
        if @pinned && @owner != ActiveSupport::IsolatedExecutionState.context
          # With transactional tests, if the connection is pinned, any thread
          # other than the one that pinned the connection need to go through the
          # query cache pool, so each thread get a different cache.
          pool.query_cache
        else
          @query_cache
        end
      end

      def query_cache_enabled
        query_cache&.enabled?
      end

      # Enable the query cache within the block.
      def cache(&block)
        pool.enable_query_cache(&block)
      end

      def enable_query_cache!
        pool.enable_query_cache!
      end

      # Disable the query cache within the block.
      #
      # Set `dirties: false` to prevent query caches on all connections from being cleared by write operations.
      # (By default, write operations dirty all connections' query caches in case they are replicas whose cache would now be outdated.)
      def uncached(dirties: true, &block)
        pool.disable_query_cache(dirties: dirties, &block)
      end

      def disable_query_cache!
        pool.disable_query_cache!
      end

      # Clears the query cache.
      #
      # One reason you may wish to call this method explicitly is between queries
      # that ask the database to randomize results. Otherwise the cache would see
      # the same SQL query and repeatedly return the same result each time, silently
      # undermining the randomness you were expecting.
      def clear_query_cache
        pool.clear_query_cache
      end

      def select_all(arel, name = nil, binds = [], preparable: nil, async: false, allow_retry: false) # :nodoc:
        arel = arel_from_relation(arel)

        # If arel is locked this is a SELECT ... FOR UPDATE or somesuch.
        # Such queries should not be cached.
        if query_cache_enabled && !(arel.respond_to?(:locked) && arel.locked)
          sql, binds, preparable, allow_retry = to_sql_and_binds(arel, binds, preparable, allow_retry)

          if async
            result = lookup_sql_cache(sql, name, binds) || super(sql, name, binds, preparable: preparable, async: async, allow_retry: allow_retry)
            FutureResult.wrap(result)
          else
            result = cache_sql(sql, name, binds) { super(sql, name, binds, preparable: preparable, async: async, allow_retry: allow_retry) }
            result.shuffle_rows(arel)
          end
        else
          super
        end
      end

      private
        def unset_query_cache!
          @query_cache = nil
        end

        def lookup_sql_cache(sql, name, binds)
          key = binds.empty? ? sql : [sql, binds]

          entry = nil
          @lock.synchronize do
            entry = query_cache.lookup(key)
          end

          if entry
            ActiveSupport::Notifications.instrument(
              "sql.active_record",
              cache_notification_info_result(sql, name, binds, entry)
            )
          end

          entry&.result
        end

        def cache_sql(sql, name, binds)
          key = binds.empty? ? sql : [sql, binds]
          entry = nil
          hit = true

          @lock.synchronize do
            entry, hit = query_cache.fetch(key) do
              result = yield
              Store::Entry.new(result, cache_provenance(sql, name))
            end
          end

          if hit
            ActiveSupport::Notifications.instrument(
              "sql.active_record",
              cache_notification_info_result(sql, name, binds, entry)
            )
          end

          entry.result.dup
        end

        # Describes where a cached result came from, so cache hits can report the
        # statement that actually produced them and the transaction view it was
        # produced in. Must be called while holding the connection lock, so the
        # scope matches the one the store files the entry under.
        def cache_provenance(sql, name)
          current = current_transaction
          Store::Provenance.new(sql, name, query_cache.current_scope, current.savepoint_name, current.user_transaction.uuid)
        end

        def cache_notification_info_result(sql, name, binds, entry)
          payload = cache_notification_info(sql, name, binds)
          payload[:row_count] = entry.result.length
          payload[:cached_in] = entry.provenance.to_h if entry.provenance
          payload
        end

        # Database adapters can override this method to
        # provide custom cache information.
        def cache_notification_info(sql, name, binds)
          {
            sql: sql,
            binds: binds,
            type_casted_binds: -> { type_casted_binds(binds) },
            name: name,
            connection: self,
            transaction: current_transaction.user_transaction.presence,
            cached: true
          }
        end
    end
  end
end
