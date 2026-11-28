require 'flipper'

module Flipper
  module Adapters
    class Failsafe
      include ::Flipper::Adapter

      # Public: Build a new Failsafe instance.
      #
      # adapter   - Flipper adapter to guard.
      # options   - Hash of options:
      #             :errors - Array of exception types for which to fail safe

      def initialize(adapter, options = {})
        @adapter = adapter
        @errors = options.fetch(:errors, [StandardError])
      end

      def read_only?
        @adapter.read_only?
      end

      def features
        @adapter.features
      rescue *@errors
        Set.new
      end

      def sync_version
        @adapter.sync_version
      rescue *@errors
        nil
      end

      def sync_version=(version)
        @adapter.sync_version = version
      rescue *@errors
        nil
      end

      def add(feature)
        @adapter.add(feature)
      rescue *@errors
        false
      end

      def remove(feature)
        @adapter.remove(feature)
      rescue *@errors
        false
      end

      def clear(feature)
        @adapter.clear(feature)
      rescue *@errors
        false
      end

      def get(feature)
        @adapter.get(feature)
      rescue *@errors
        {}
      end

      def get_multi(features)
        @adapter.get_multi(features)
      rescue *@errors
        {}
      end

      def get_all(**kwargs)
        @adapter.get_all(**kwargs)
      rescue *@errors
        {}
      end

      def enable(feature, gate, thing)
        @adapter.enable(feature, gate, thing)
      rescue *@errors
        false
      end

      def disable(feature, gate, thing)
        @adapter.disable(feature, gate, thing)
      rescue *@errors
        false
      end

      # Public: Replaces one member of a set gate with another.
      #
      # Unlike the writes above, an actor set conflict or an exceeded actor
      # limit is re-raised rather than turned into false: "another change won,
      # re-read and retry" and "this change would exceed the limit" are answers
      # the caller can act on, not the same as "the store is down". Only store
      # failures are failed safe.
      def replace(feature, gate, old_thing, new_thing, expected: nil)
        @adapter.replace(feature, gate, old_thing, new_thing, expected: expected)
      rescue ActorSetConflict, Adapters::ActorLimit::LimitExceeded
        raise
      rescue *@errors
        false
      end
    end
  end
end
