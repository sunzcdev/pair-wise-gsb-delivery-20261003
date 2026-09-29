# frozen_string_literal: true

module StateMachines
  class Machine
    module Snapshot
      # The version of this state machine's definition.  Bump this (via the
      # :version option) whenever state names or values change, and register
      # a migration from the previous version so that persisted snapshots
      # can still be restored.
      attr_reader :version

      # Registers an explicit migration from one definition version to the
      # next.  Migrations must be between adjacent versions; restoring a
      # snapshot from an older version applies the chain of migrations one
      # step at a time.
      #
      # The block receives the persisted state value and must return the
      # value for the next version.
      #
      # == Example
      #
      #   state_machine version: 2 do
      #     migrate(1) { |value| value == 'parked' ? 'stopped' : value }
      #     ...
      #   end
      def migrate(from_version, to_version = nil, &block)
        raise ArgumentError, 'migration requires a block' unless block

        to_version ||= from_version.is_a?(Integer) ? from_version + 1 : nil
        unless from_version.is_a?(Integer) && from_version >= 1 && to_version == from_version + 1
          raise ArgumentError,
                "migrations must be between adjacent versions, got #{from_version.inspect} => #{to_version.inspect}"
        end

        migrations[from_version] = block
      end

      # Builds a JSON-compatible snapshot of this machine's current state for
      # the given object.  The snapshot captures the definition version and
      # the raw state value.
      def snapshot(object)
        value = read(object, :state)
        unless StateMachines::Snapshot.json_compatible?(value)
          raise InvalidSnapshot, "Cannot snapshot #{name.inspect}: current value #{value.inspect} is not JSON-compatible"
        end

        { 'version' => version, 'value' => value }
      end

      # Validates a snapshot entry for this machine and computes the state
      # value it maps to under the current definition, applying any
      # registered migrations.  The object is never modified and no events,
      # callbacks or actions are run.
      def restore_value(entry)
        unless entry.is_a?(Hash) && StateMachines::Snapshot.json_compatible?(entry) && entry.keys.sort == %w[value version]
          raise InvalidSnapshot, "Invalid snapshot for #{name.inspect}: expected a hash with 'version' and 'value' keys"
        end

        snapshot_version = entry['version']
        unless snapshot_version.is_a?(Integer) && snapshot_version >= 1
          raise InvalidSnapshot, "Invalid snapshot version #{snapshot_version.inspect} for #{name.inspect}"
        end

        if snapshot_version > version
          raise InvalidSnapshot,
                "Snapshot version #{snapshot_version} for #{name.inspect} is newer than the definition version #{version}"
        end

        value = migrate_value(snapshot_version, entry['value'])

        unless StateMachines::Snapshot.json_compatible?(value)
          raise InvalidSnapshot, "Migrated value #{value.inspect} for #{name.inspect} is not JSON-compatible"
        end

        unless states[value, :value] || states.detect { |state| state.matches?(value) }
          raise InvalidSnapshot, "#{value.inspect} is not a known #{name} value"
        end

        value
      end

      private

      # The registered migrations, keyed by the version they migrate *from*
      def migrations
        @migrations ||= {}
      end

      # Applies the chain of migrations needed to bring a value persisted at
      # the given version up to the current definition version
      def migrate_value(snapshot_version, value)
        (snapshot_version...version).each do |from|
          migration = migrations[from]
          unless migration
            raise InvalidSnapshot, "No migration registered for #{name.inspect} from version #{from} to #{from + 1}"
          end

          value = migration.call(value)
        end
        value
      end
    end
  end
end
