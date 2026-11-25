# frozen_string_literal: true

require "active_support/cache/strategy/local_cache"

module ActiveSupport
  module Cache
    module Strategy
      # The stock LocalCache strategy returns the result of clearing its
      # in-memory hash rather than the value returned by the underlying store.
      # SolidCache::Store#clear returns a structured report for namespace-scoped,
      # point-in-time clears, so that value has to be preserved.
      module LocalCacheWithClearResult
        def clear(options = nil)
          # Clear the local cache ourselves and bypass the stock strategy's
          # #clear, which would otherwise discard the underlying store's return
          # value in favour of the result of clearing the in-memory hash.
          local_cache&.clear
          clear_cache(options)
        end
      end
    end
  end
end
