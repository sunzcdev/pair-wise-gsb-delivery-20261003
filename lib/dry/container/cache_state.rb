# frozen_string_literal: true

require "concurrent/hash"
require "concurrent/atomic/atomic_fixnum"

module Dry
  class Container
    # Internal bookkeeping that lets memoized items detect when one of their
    # declared dependencies has been replaced.
    #
    # Every registered key has a monotonically increasing version. Replacing a
    # key bumps its version and the versions of all items that (transitively)
    # depend on it, so stale memoized values are rebuilt on next resolution
    # while unrelated memoized values keep their identity.
    #
    # @api private
    class CacheState
      # Serializes replacements so version bumps stay consistent
      #
      # @api private
      attr_reader :replace_lock

      def initialize
        @versions = ::Concurrent::Hash.new(0)
        @epoch = ::Concurrent::AtomicFixnum.new(0)
        @replace_lock = ::Mutex.new
      end

      # Global epoch, bumped on every replacement so that an in-flight
      # resolution can detect it must retry instead of returning a mix of
      # pre- and post-replacement values
      #
      # @api private
      def epoch
        @epoch.value
      end

      # @api private
      def increment_epoch
        @epoch.increment
      end

      # Current versions for the given dependency keys
      #
      # @api private
      def versions_for(keys)
        keys.each_with_object({}) do |key, versions|
          versions[key.to_s] = @versions[key.to_s]
        end
      end

      # Bump the version of a single key. Must be called under +replace_lock+
      #
      # @api private
      def bump(key)
        key = key.to_s
        @versions[key] = @versions[key] + 1
      end
    end
  end
end
