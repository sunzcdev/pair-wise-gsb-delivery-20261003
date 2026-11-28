require 'json'
require 'pstore'
require 'set'
require 'flipper'

module Flipper
  module Adapters
    # Public: Adapter based on Ruby's pstore database. Perfect for when a local
    # file is good enough for storing features.
    class PStore
      include ::Flipper::Adapter

      # Public: The path to where the file is stored.
      attr_reader :path

      # Public
      def initialize(path = 'flipper.pstore', thread_safe = true)
        @path = path
        @store = ::PStore.new(path, thread_safe)
        @features_key = :flipper_features
      end

      # Public: The set of known features.
      def features
        @store.transaction do
          read_feature_keys
        end
      end

      # Public: The Flipper::SyncVersion of the flags last synced into this
      # adapter, shared by every process using the same file.
      def sync_version
        @store.transaction do
          SyncVersion.parse(read(SyncVersion::KEY))
        end
      end

      # Public: Records the Flipper::SyncVersion of the flags just synced.
      def sync_version=(version)
        @store.transaction do
          write SyncVersion::KEY, version
        end
      end

      # Public: Adds a feature to the set of known features.
      def add(feature)
        @store.transaction do
          set_add @features_key, feature.key
        end
        true
      end

      # Public: Removes a feature from the set of known features and clears
      # all the values for the feature.
      def remove(feature)
        @store.transaction do
          set_delete @features_key, feature.key
          clear_gates(feature)
        end
        true
      end

      # Public: Clears all the gate values for a feature.
      def clear(feature)
        @store.transaction do
          clear_gates(feature)
        end
        true
      end

      # Public
      def get(feature)
        @store.transaction do
          result_for_feature(feature)
        end
      end

      def get_multi(features)
        @store.transaction do
          read_many_features(features)
        end
      end

      def get_all(**kwargs)
        @store.transaction do
          features = read_feature_keys.map { |key| Flipper::Feature.new(key, self) }
          read_many_features(features)
        end
      end

      # Public
      def enable(feature, gate, thing)
        @store.transaction do
          case gate.data_type
          when :boolean
            clear_gates(feature)
            write key(feature, gate), thing.value.to_s
          when :integer
            write key(feature, gate), thing.value.to_s
          when :set
            set_add key(feature, gate), thing.value.to_s
          when :json
            write key(feature, gate), Typecast.to_json(thing.value)
          else
            raise "#{gate} is not supported by this adapter yet"
          end
        end

        true
      end

      # Public
      def disable(feature, gate, thing)
        case gate.data_type
        when :boolean
          clear(feature)
        when :integer
          @store.transaction do
            write key(feature, gate), thing.value.to_s
          end
        when :set
          @store.transaction do
            set_delete key(feature, gate), thing.value.to_s
          end
        when :json
          @store.transaction do
            delete key(feature, gate)
          end
        else
          raise "#{gate} is not supported by this adapter yet"
        end

        true
      end

      # Public: Replaces one member of a set-type gate with another inside a
      # single store transaction, so the swap is all-or-nothing on disk.
      #
      # The plan is checked against the members read inside the transaction, so
      # a writer that committed first is visible and a caller working from a
      # stale member list raises Flipper::ActorSetConflict instead of
      # overwriting the winner. Because the pair is written together, a reader
      # never sees the set without the old member and before the new exists.
      def replace(feature, gate, old_thing, new_thing, expected: nil)
        @store.transaction do
          current = normalize_member_values(set_members(key(feature, gate)))
          prospective = plan_member_replacement(
            current,
            old_value: old_thing.value,
            new_value: new_thing.value,
            expected: expected,
            feature_key: feature.key,
          )

          next true if prospective == current

          set_add key(feature, gate), new_thing.value.to_s
          set_delete key(feature, gate), old_thing.value.to_s
          true
        end
      end

      # Public
      def inspect
        attributes = [
          "name=#{@name.inspect}",
          "path=#{@path.inspect}",
          "store=#{@store}",
        ]
        "#<#{self.class.name}:#{object_id} #{attributes.join(', ')}>"
      end

      private

      def clear_gates(feature)
        feature.gates.each do |gate|
          delete key(feature, gate)
        end
      end

      def read_feature_keys
        set_members @features_key
      end

      def read_many_features(features)
        result = {}
        features.each do |feature|
          result[feature.key] = result_for_feature(feature)
        end
        result
      end

      def result_for_feature(feature)
        result = {}

        feature.gates.each do |gate|
          key = key(feature, gate)
          result[gate.key] =
            case gate.data_type
            when :boolean, :integer
              read key
            when :set
              set_members key
            when :json
              value = read(key)
              Typecast.from_json(value)
            else
              raise "#{gate} is not supported by this adapter yet"
            end
        end

        result
      end

      # Private
      def key(feature, gate)
        "#{feature.key}/#{gate.key}"
      end

      # Private
      def read(key)
        @store[key.to_s]
      end

      # Private
      def write(key, value)
        @store[key.to_s] = value.to_s
      end

      # Private
      def delete(key)
        @store.delete(key.to_s)
      end

      # Private
      def set_add(key, value)
        set_members(key) do |members|
          members.add(value.to_s)
        end
      end

      # Private
      def set_delete(key, value)
        set_members(key) do |members|
          members.delete(value.to_s)
        end
      end

      # Private
      def set_members(key)
        key = key.to_s

        @store[key] ||= Set.new

        if block_given?
          yield @store[key]
        else
          @store[key]
        end
      end
    end
  end
end

Flipper.configure do |config|
  config.adapter { Flipper::Adapters::PStore.new }
end
