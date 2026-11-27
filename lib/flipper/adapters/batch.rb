require "flipper/adapter"
require "flipper/typecast"

module Flipper
  module Adapters
    # Public: An in-memory adapter that buffers a group of changes during an
    # Adapter#transaction. It starts as a copy of the adapter's committed state,
    # records every write made inside the transaction and exposes the resulting
    # (not yet committed) state for validation. Reads against the real adapter
    # are unaffected until the batch is committed.
    class Batch
      include ::Flipper::Adapter

      # Internal: Frozen config returned for features the batch doesn't know.
      DEFAULT_CONFIG = default_config.each_value(&:freeze).freeze

      # Public: Build a batch seeded with an adapter's committed state.
      #
      # adapter - An adapter, DSL or export whose flags seed the working copy.
      def self.from(adapter)
        source =
          if adapter.is_a?(Flipper::Adapter)
            adapter
          elsif adapter.respond_to?(:adapter)
            adapter.adapter
          else
            adapter
          end
        new(source.get_all)
      end

      def initialize(source = nil)
        @source = deep_dup(Typecast.features_hash(source))
        @changes = Set.new
      end

      # Public: The keys of every feature touched by a write in this batch.
      attr_reader :changes

      # Public: Did this batch change the given feature from its seeded state?
      def changed?(feature)
        @changes.include?(feature.key)
      end

      # Public: The working state as a hash usable by Adapter#import. Gate sets
      # and JSON values are duplicated so committing the batch never shares
      # mutable state with it.
      def get_all(**_kwargs)
        deep_dup(@source)
      end

      # Public: The set of known features in the working state.
      def features
        @source.keys.to_set
      end

      # Public: Adds a feature to the set of known features.
      def add(feature)
        @source[feature.key] ||= default_config
        @changes << feature.key
        true
      end

      # Public: Removes a feature from the set of known features.
      def remove(feature)
        @source.delete(feature.key)
        @changes << feature.key
        true
      end

      # Public: Clears all the gate values for a feature.
      def clear(feature)
        @source[feature.key] = default_config
        @changes << feature.key
        true
      end

      # Public
      def get(feature)
        @source[feature.key] || DEFAULT_CONFIG
      end

      # Public
      def get_multi(features)
        features.each_with_object({}) do |feature, result|
          result[feature.key] = get(feature)
        end
      end

      # Public
      def enable(feature, gate, thing)
        @source[feature.key] ||= default_config
        @changes << feature.key

        case gate.data_type
        when :boolean
          @source[feature.key] = default_config
          @source[feature.key][gate.key] = thing.value.to_s
        when :integer
          @source[feature.key][gate.key] = thing.value.to_s
        when :set
          @source[feature.key][gate.key] << thing.value.to_s
        when :json
          @source[feature.key][gate.key] = thing.value
        else
          raise "#{gate} is not supported by this adapter yet"
        end

        true
      end

      # Public
      def disable(feature, gate, thing)
        @source[feature.key] ||= default_config
        @changes << feature.key

        case gate.data_type
        when :boolean
          @source[feature.key] = default_config
        when :integer
          @source[feature.key][gate.key] = thing.value.to_s
        when :set
          @source[feature.key][gate.key].delete(thing.value.to_s)
        when :json
          @source[feature.key].delete(gate.key)
        else
          raise "#{gate} is not supported by this adapter yet"
        end

        true
      end

      # Public: Batches are never read-only; they only buffer writes.
      def read_only?
        false
      end

      def inspect
        "#<#{self.class.name}:0x#{(object_id * 2).to_s(16)} changes=#{@changes.to_a.inspect} source=#{@source.inspect}>"
      end

      private

      # Internal: Recursively copy the features hash so the working copy never
      # shares a mutable Set, Hash or Array with the source adapter or with the
      # snapshot handed to Adapter#import on commit.
      def deep_dup(value)
        case value
        when Set
          value.each_with_object(Set.new) { |element, set| set << deep_dup(element) }
        when Hash
          value.each_with_object({}) { |(key, element), hash| hash[key] = deep_dup(element) }
        when Array
          value.map { |element| deep_dup(element) }
        else
          value
        end
      end
    end
  end
end
