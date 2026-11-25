require 'delegate'

module Flipper
  module Adapters
    # Internal: Adapter that wraps another adapter with the ability to memoize
    # adapter calls in memory. Used by flipper dsl and the memoizer middleware
    # to make it possible to memoize adapter calls for the duration of a request.
    class Memoizable
      include ::Flipper::Adapter

      # Internal
      attr_reader :cache

      # Internal: The adapter this adapter is wrapping.
      attr_reader :adapter

      # Public
      def initialize(adapter, cache = nil)
        @adapter = adapter
        @cache = cache || {}
        @memoize = false
        @features_key = :flipper_features
        @get_all_key = :all_memoized
        @cache_key_by_feature_key = Hash.new { |hash, key| hash[key] = "feature/#{key}".freeze }
      end

      # Public: The read receipt collected by a wrapped failover adapter, or
      # nil if the wrapped adapter does not collect one. Memo hits are
      # restamped with the :memo source on every read so the receipt always
      # reflects where the returned value actually came from.
      def read_receipt
        @adapter.read_receipt if @adapter.respond_to?(:read_receipt)
      end

      # Public
      def features
        if memoizing?
          cache.fetch(@features_key) { cache[@features_key] = @adapter.features }
        else
          @adapter.features
        end
      end

      # Public
      def add(feature)
        @adapter.add(feature).tap { expire_features_set }
      end

      # Public
      def remove(feature)
        @adapter.remove(feature).tap do
          expire_features_set
          expire_feature(feature)
        end
      end

      # Public
      def clear(feature)
        @adapter.clear(feature).tap { expire_feature(feature) }
      end

      # Public
      def get(feature)
        if memoizing?
          cache_key = key_for(feature.key)
          if cache.key?(cache_key)
            restamp_receipt(feature.key, :get)
            return cache[cache_key]
          end

          if preloaded_missing?(feature)
            cache[cache_key] = default_config
            remember_missing_receipt(feature.key, :get)
          else
            cache[cache_key] = @adapter.get(feature)
            remember_receipt(feature.key)
          end

          cache[cache_key]
        else
          @adapter.get(feature)
        end
      end

      # Public
      def get_multi(features)
        if memoizing?
          memoized_keys = []
          uncached_features = features.reject do |feature|
            cache_key = key_for(feature.key)
            if preloaded_missing?(feature)
              cache[cache_key] = default_config
              remember_missing_receipt(feature.key, :get_multi)
            end
            hit = cache.key?(cache_key)
            memoized_keys << feature.key if hit
            hit
          end

          if uncached_features.any?
            response = @adapter.get_multi(uncached_features)
            response.each do |key, hash|
              cache[key_for(key)] = hash
            end
            uncached_features.each { |feature| remember_receipt(feature.key) }
          end

          result = {}
          features.each do |feature|
            result[feature.key] = cache[key_for(feature.key)]
          end
          # Only values reused from the cache are restamped :memo; values
          # actively read above keep their primary/secondary provenance.
          memoized_keys.each { |key| restamp_receipt(key, :get_multi) }
          result
        else
          @adapter.get_multi(features)
        end
      end

      def get_all(**kwargs)
        if memoizing?
          response = nil
          if cache[@get_all_key]
            response = {}
            cache[@features_key].each do |key|
              response[key] = cache[key_for(key)]
            end
            cache[@features_key].each { |key| restamp_receipt(key, :get_all) }
          else
            response = @adapter.get_all(**kwargs)
            response.each do |key, value|
              cache[key_for(key)] = value
            end
            cache[@features_key] = response.keys.to_set
            cache[@get_all_key] = true
            response.keys.each { |key| remember_receipt(key) }
          end

          # Ensures that looking up other features that do not exist doesn't
          # result in N+1 adapter calls.
          response.default_proc = ->(memo, key) { memo[key] = default_config }
          response
        else
          @adapter.get_all(**kwargs)
        end
      end

      # Public
      def enable(feature, gate, thing)
        @adapter.enable(feature, gate, thing).tap { expire_feature(feature) }
      end

      # Public
      def disable(feature, gate, thing)
        @adapter.disable(feature, gate, thing).tap { expire_feature(feature) }
      end

      # Public
      def read_only?
        @adapter.read_only?
      end

      def import(source)
        @adapter.import(source).tap do
          if memoizing?
            cache.clear
          end
        end
      end

      def export(format: :json, version: 1)
        @adapter.export(format: format, version: version)
      end

      # Internal: Turns local caching on/off.
      #
      # value - The Boolean that decides if local caching is on.
      def memoize=(value)
        cache.clear
        @memoize = value
      end

      # Internal: Returns true for using local cache, false for not.
      def memoizing?
        !!@memoize
      end

      if RUBY_VERSION >= '3.0'
        def method_missing(name, *args, **kwargs, &block)
          @adapter.send name, *args, **kwargs, &block
        end
      else
        def method_missing(name, *args, &block)
          @adapter.send name, *args, &block
        end
      end

      private

      # Internal: Snapshot the failover provenance for a key right after it
      # was actively read through the wrapped adapter. If the key had been
      # served from the memo earlier, restore its pre-memo provenance first.
      def remember_receipt(key)
        return unless read_receipt
        read_receipt.restore(key)
      end

      # Internal: Remember that a preloaded-missing default was synthesized by
      # the memoizer rather than read from any adapter.
      def remember_missing_receipt(key, operation)
        return unless read_receipt
        read_receipt.record(
          key,
          source: :memo,
          status: :missing,
          operation: operation,
          values: { key.to_s => default_config },
        )
      end

      # Internal: Re-stamp the receipt when a cached value is reused. The
      # original primary/secondary provenance is preserved on the entry and
      # restored the next time the key is actively read.
      def restamp_receipt(key, operation)
        return unless read_receipt
        read_receipt.restamp(key, operation)
      end

      def preloaded_missing?(feature)
        cache[@get_all_key] && cache[@features_key] && !cache[@features_key].include?(feature.key)
      end

      def key_for(key)
        @cache_key_by_feature_key[key]
      end

      def expire_feature(feature)
        if memoizing?
          cache.delete(key_for(feature.key))
          cache.delete(@get_all_key)
        end
      end

      def expire_features_set
        if memoizing?
          cache.delete(@features_key)
          cache.delete(@get_all_key)
        end
      end
    end
  end
end
