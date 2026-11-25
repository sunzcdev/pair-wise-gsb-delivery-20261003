require "flipper/sync_version"

module Flipper
  module Adapters
    class Sync
      # Internal: One update coming from the remote side.
      #
      # A full change set is a complete mirror of the remote: features and
      # gates it does not mention are removed. This is the original sync
      # contract, it overwrites.
      #
      # An incremental change set only mentions what changed. Features and
      # gates it does not mention keep whatever the local adapter already has.
      # Deletions are always explicit:
      #
      #   ChangeSet.incremental(
      #     {
      #       "search" => {boolean: true},          # settle this gate
      #       "stats"  => {removed_gates: [:actors]}, # clear just this gate
      #       "old"    => {deleted: true},          # remove the whole feature
      #     },
      #     version: Flipper::SyncVersion.new("scope", 11),
      #   )
      #
      # Every change in a change set settles at the change set's version, so a
      # replayed or slow older change set never resurrects a removed gate.
      class ChangeSet
        # Internal: The gates of one feature inside a change set.
        class FeatureChange
          # Public: The Hash of gate key => value mentioned by the change. A
          # mentioned gate is settled to the given value, so nil or an empty
          # set explicitly clears it.
          attr_reader :gates

          # Public: The Array of gate keys the change clears.
          attr_reader :removed_gates

          # Public: Builds a change for one feature from whatever the remote
          # sent: nil (mentioned but unchanged), true or :deleted (removed), a
          # bare hash of gate values, or a hash with :gates, :removed_gates or
          # :deleted keys.
          def self.build(value)
            case value
            when self
              value
            when nil
              new
            when true, :deleted, "deleted"
              new(deleted: true)
            when Hash
              from_hash(value)
            else
              raise ArgumentError, "cannot build a feature change from #{value.inspect}"
            end
          end

          def self.from_hash(hash)
            attributes = {}
            hash.each do |key, value|
              attributes[key.respond_to?(:to_sym) ? key.to_sym : key] = value
            end

            return new(deleted: true) if attributes[:deleted]

            if attributes.key?(:gates) || attributes.key?(:removed_gates) || attributes.key?(:removed)
              # Anything that is not a control key is a gate of this change.
              gates = attributes[:gates] || attributes.reject do |key, _|
                %i[gates removed_gates removed deleted].include?(key)
              end

              new(
                gates: gates,
                removed_gates: attributes[:removed_gates] || attributes[:removed] || [],
              )
            else
              new(gates: attributes)
            end
          end

          def initialize(gates: {}, removed_gates: [], deleted: false)
            @gates = gates.each_with_object({}) do |(key, value), result|
              result[key.respond_to?(:to_sym) ? key.to_sym : key] = value
            end
            @removed_gates = Array(removed_gates).map { |key| key.to_sym }
            @deleted = !!deleted
          end

          # Public: Was the whole feature removed by this change?
          def deleted?
            @deleted
          end

          # Public: The Array of gate keys this change settles, including the
          # ones it explicitly clears.
          def changed_gates
            (gates.keys + removed_gates).uniq
          end

          # Public: The full gate values this change settles to, given the gate
          # values the local feature already has. Gates the change does not
          # mention keep their local value; removed gates become nil so they
          # are cleared.
          def merged_gate_values(local_gate_values)
            merged = local_gate_values.dup
            gates.each { |key, value| merged[key] = value }
            removed_gates.each { |key| merged[key] = nil }
            merged
          end
        end

        # Public: Builds a complete mirror of the remote.
        def self.full(features, version: nil)
          new(features, version: version, full: true)
        end

        # Public: Builds an incremental update that only touches what it
        # mentions.
        def self.incremental(features, version: nil)
          new(features, version: version, full: false)
        end

        # Public: Builds a change set from a ChangeSet, a Hash describing
        # features (optionally wrapping them under :features or :changes, and
        # optionally carrying :version, :deleted, :full or :incremental), or
        # anything responding to #to_change_set.
        #
        # options - The Hash of options.
        #           :version - Version of this update, when not in value.
        #           :deleted - Array of feature keys this update removes.
        #           :full - Treat value as a complete mirror (default: true for
        #                   ChangeSet.full, false otherwise).
        #           :incremental - Treat value as an increment.
        #
        # Returns a ChangeSet.
        def self.from(value, options = {})
          return value if value.is_a?(self)

          if value.respond_to?(:to_change_set)
            return from(value.to_change_set, options)
          end

          if value.is_a?(Hash)
            attributes = {}
            value.each do |key, item|
              attributes[key.respond_to?(:to_sym) ? key.to_sym : key] = item
            end

            features = attributes[:features] || attributes[:changes]
            features = attributes.reject { |key, _| %i[version deleted full incremental mirror partial delta].include?(key) } if features.nil?
            features ||= {}

            deleted = Array(options[:deleted] || attributes[:deleted]).map(&:to_s)
            change_version = options[:version] || attributes[:version]
            full = if options.key?(:full)
              options[:full]
            elsif options.key?(:incremental)
              !options[:incremental]
            elsif attributes.key?(:incremental)
              !attributes[:incremental]
            else
              !!(attributes[:full] || attributes[:mirror])
            end

            change_set = new(features, version: change_version, full: full)
            deleted.each { |key| change_set.delete(key) }
            return change_set
          end

          raise ArgumentError, "cannot build a change set from #{value.inspect}"
        end

        # Public: The Hash of feature key => FeatureChange.
        attr_reader :features

        # Public: The raw version the change set settles at. May be a
        # Flipper::SyncVersion, an Integer, a "scope:number" String or nil.
        attr_reader :raw_version

        def initialize(features, version: nil, full: false)
          @features = normalize(features)
          @raw_version = version
          @full = !!full
        end

        # Public: Is this a complete mirror of the remote? Full change sets
        # overwrite; incremental ones merge.
        def full?
          @full
        end

        # Public: Is this an incremental update?
        def incremental?
          !@full
        end

        # Public: The feature keys mentioned by the change set.
        def keys
          features.keys
        end

        # Public: Records the removal of a whole feature.
        def delete(key)
          features[key.to_s] = FeatureChange.new(deleted: true)
          self
        end

        # Public: Was the feature explicitly removed?
        def deleted?(key)
          change = features[key.to_s]
          change ? change.deleted? : false
        end

        # Public: The FeatureChange for a feature, or nil if not mentioned.
        def change_for(key)
          features[key.to_s]
        end

        # Public: The Flipper::SyncVersion of this change set. A bare Integer
        # is interpreted in the same scope as the local version it is compared
        # with, and a "scope:number" String carries its own scope.
        #
        # local_version - The Flipper::SyncVersion currently recorded locally.
        def sync_version(local_version = nil)
          scope = local_version ? local_version.scope : ""

          case raw_version
          when nil
            nil
          when SyncVersion
            raw_version
          when Integer
            SyncVersion.new(scope, raw_version)
          else
            SyncVersion.parse(raw_version.to_s) ||
              begin
                number = Integer(raw_version.to_s, 10, exception: false)
                number && SyncVersion.new(scope, number)
              end
          end
        end

        private

        def normalize(features)
          (features || {}).each_with_object({}) do |(key, value), result|
            result[key.to_s] = FeatureChange.build(value)
          end
        end
      end
    end
  end
end