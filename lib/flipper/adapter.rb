module Flipper
  # Adding a module include so we have some hooks for stuff down the road
  module Adapter
    def self.included(base)
      base.extend(ClassMethods)
    end

    module ClassMethods
      # Public: Default config for a feature's gate values.
      def default_config
        {
          boolean: nil,
          groups: Set.new,
          actors: Set.new,
          expression: nil,
          percentage_of_actors: nil,
          percentage_of_time: nil,
        }
      end

      def from(source)
        return source if source.is_a?(Flipper::Adapter)
        source.adapter
      end
    end

    def read_only?
      false
    end

    # Public: Get all features and gate values in one call. Defaults to one call
    # to features and another to get_multi. Feel free to override per adapter to
    # make this more efficient.
    def get_all(**kwargs)
      instances = features.map { |key| Flipper::Feature.new(key, self) }
      get_multi(instances)
    end

    # Public: Get multiple features in one call. Defaults to one get per
    # feature. Feel free to override per adapter to make this more efficient and
    # reduce network calls.
    def get_multi(features)
      result = {}
      features.each do |feature|
        result[feature.key] = get(feature)
      end
      result
    end

    # Public: Ensure that adapter is in sync with source adapter provided.
    #
    # source - The source dsl, adapter or export to import.
    #
    # Returns true if successful.
    def import(source)
      Adapters::Sync::Synchronizer.new(self, self.class.from(source), raise: true).call
      true
    end

    # Public: Exports the adapter in a given format for a given format version.
    #
    # Returns a Flipper::Export instance.
    def export(format: :json, version: 1)
      Flipper::Exporter.build(format: format, version: version).call(self)
    end

    # Public: Apply a group of changes together instead of one write at a time.
    #
    # The block receives an Flipper::Adapters::Batch seeded with the adapter's
    # committed state. Writes made through the batch are buffered and stay
    # invisible to readers using the adapter's normal get/get_multi/get_all
    # methods (the read barrier) until the block returns. After the block, the
    # optional validator runs against the batch's final combined state; if it
    # raises, the whole batch is discarded and the committed flags are left
    # untouched. Otherwise the batch is committed in one import.
    #
    # validate - Optional callable receiving the batch after the block runs.
    #            Raise (e.g. Flipper::InvalidBatch) to reject the combination.
    #
    # Yields the Flipper::Adapters::Batch.
    # Returns the block's return value on a successful commit.
    # Raises if the validator rejects the final state; nothing is committed.
    def transaction(validate: nil)
      batch = Adapters::Batch.from(self)
      result = yield batch
      validate&.call(batch)
      import(batch)
      result
    end

    # Public: Default config for a feature's gate values.
    def default_config
      self.class.default_config
    end

    # Public: The Flipper::SyncVersion of the synced flags this adapter holds,
    # or nil if it doesn't know. Syncs use it to never replace newer flags with
    # older ones. Adapters that wrap another adapter delegate to it. Storage
    # adapters that can keep it override this and #sync_version=.
    def sync_version
      wrapped = respond_to?(:adapter) ? adapter : nil
      wrapped.sync_version if wrapped.respond_to?(:sync_version)
    end

    # Public: Records the Flipper::SyncVersion of the flags just synced into
    # this adapter. Does nothing for adapters that can't keep it.
    def sync_version=(version)
      wrapped = respond_to?(:adapter) ? adapter : nil
      wrapped.sync_version = version if wrapped.respond_to?(:sync_version=)
    end

    # Public: default name of the adapter
    def name
      @name ||= self.class.name.split('::').last.split(/(?=[A-Z])/).join('_').downcase.to_sym
    end

    # Public: Returns a string representation of the adapter stack for debugging.
    # Shows the full chain of wrapped adapters.
    #
    # Examples:
    #   "memoizable -> active_support_cache_store -> active_record"
    #   "memoizable -> failover(primary: redis, secondary: memory)"
    #
    # Returns a String.
    def adapter_stack
      if respond_to?(:adapter) && adapter
        "#{name} -> #{adapter.adapter_stack}"
      else
        name.to_s
      end
    end

    private

    # Internal: Raises for feature names reserved for Flipper's own data. Adapters
    # that store that data next to features call it before every write.
    def ensure_not_reserved_key!(feature)
      raise ReservedKeyError.new(feature.key) if Flipper.reserved_key?(feature.key)
    end
  end
end

require "set"
require "flipper/exporter"
require "flipper/feature"
require "flipper/adapters/batch"
require "flipper/adapters/sync/synchronizer"
