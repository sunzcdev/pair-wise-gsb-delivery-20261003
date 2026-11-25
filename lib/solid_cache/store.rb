# frozen_string_literal: true

require "active_support/cache/strategy/local_cache_with_clear_result"

module SolidCache
  class Store < ActiveSupport::Cache::Store
    include Api, Connections, Entries, Execution, Expiry, Failsafe, NamespaceClear, Stats
    prepend ActiveSupport::Cache::Strategy::LocalCache
    prepend ActiveSupport::Cache::Strategy::LocalCacheWithClearResult

    def initialize(options = {})
      super(SolidCache.configuration.store_options.merge(options))
    end

    def self.supports_cache_versioning?
      true
    end

    def setup!
      super
    end

    # Entry point for both full clears and namespace-scoped, point-in-time
    # clears. The prepended LocalCacheWithClearResult strategy resets the
    # in-memory local cache and then calls this method so that the scoped clear
    # report is preserved as the return value of #clear. See NamespaceClear for
    # the semantics of the namespace options.
    def clear_cache(options = nil)
      return entry_clear unless scoped_clear?(options)

      namespace = resolve_clear_namespace(options)
      clear_namespace(namespace, gate: options[:gate], sweep_pending: options[:sweep_pending] == true)
    end
  end
end
