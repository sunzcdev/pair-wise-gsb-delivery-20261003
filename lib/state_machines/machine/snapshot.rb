# frozen_string_literal: true

module StateMachines
  class Machine
    # Adds support for safely exporting and restoring the state of an object
    # as plain, JSON-compatible data.  This is useful for persisting state
    # machine data across process restarts without loading arbitrary Ruby
    # objects (e.g. via Marshal or YAML).
    #
    # Each snapshot entry contains:
    # * <tt>machine</tt> - The identifier (name) of the machine
    # * <tt>version</tt> - The definition version of the machine
    # * <tt>value</tt> - The current state value stored in the attribute
    #
    # Since state names may change between releases, machines can be given an
    # explicit definition +version+ and applications can register migrations
    # between adjacent versions via +migrate_version+.  When a snapshot with
    # an older version is restored, the registered migrations are applied in
    # sequence until the current version is reached.
    #
    # For example,
    #
    #   class Vehicle
    #     state_machine :state, initial: :parked, version: 2 do
    #       migrate_version 1 do |value|
    #         value == 'waiting' ? 'parked' : value
    #       end
    #
    #       event :ignite do
    #         transition parked: :idling
    #       end
    #     end
    #   end
    #
    #   vehicle = Vehicle.new
    #   vehicle.state_machines_snapshot
    #   # => [{"machine" => "state", "version" => 2, "value" => "parked"}]
    #
    # Restoring a snapshot validates *every* machine entry before writing
    # anything to the object: either all entries are applied or none are.
    # Restoration writes the attribute directly -- no events are fired and no
    # transition callbacks or actions are run.
    module Snapshot
      # The keys required in each snapshot entry
      SNAPSHOT_KEYS = %w[machine version value].freeze

      # The definition version of this machine.  When called with an argument,
      # sets the version; otherwise returns the current one.  Versions are
      # positive integers and default to 1.
      def version(number = nil)
        number.nil? ? @version : self.version = number
      end

      # Sets the definition version of this machine
      def version=(number)
        unless number.is_a?(Integer) && number >= 1
          raise ArgumentError, "Machine definition version must be a positive integer, got #{number.inspect}"
        end

        @version = number
      end

      # Registers a migration that converts a snapshot state value from the
      # given definition version to the next one (+from_version+ + 1).  The
      # block receives the JSON-compatible state value stored in the snapshot
      # and must return the new JSON-compatible value.
      #
      # Migrations are only registered between adjacent versions; restoring an
      # older snapshot applies each migration in turn until the machine's
      # current version is reached.
      def migrate_version(from_version, &block)
        raise ArgumentError, 'migrate_version requires a block' unless block

        unless from_version.is_a?(Integer) && from_version >= 1
          raise ArgumentError, "Migration version must be a positive integer, got #{from_version.inspect}"
        end

        if @version_migrations.key?(from_version)
          raise ArgumentError, "A migration from version #{from_version} is already registered for #{name.inspect}"
        end

        @version_migrations[from_version] = block
      end

      # Builds a JSON-compatible snapshot entry for the current state of the
      # given object.  Raises StateMachines::SnapshotError if the current
      # state value cannot be represented as JSON-compatible data.
      def snapshot(object)
        value = read(object, :state)
        assert_json_compatible!(object, value)

        { 'machine' => name.to_s, 'version' => version, 'value' => value }
      end

      # Validates the given snapshot entry against this machine's current
      # definition and resolves the attribute value that should be written to
      # the object, applying any registered version migrations.  This performs
      # no writes -- it only validates and computes the target value so that
      # a collection of machines can be validated fully before any state is
      # changed.
      #
      # Raises StateMachines::SnapshotError if the entry is malformed, was
      # created by a newer definition, cannot be migrated to the current
      # version, contains non-JSON-compatible data, or resolves to an unknown
      # state value.
      def resolve_snapshot(object, entry)
        validate_snapshot_entry!(object, entry)

        snapshot_version = entry['version']
        value = entry['value']
        assert_json_compatible!(object, value)

        if snapshot_version > version
          raise SnapshotError.new(object, "Snapshot for #{name.inspect} was created by newer definition version #{snapshot_version} (current version is #{version})")
        end

        while snapshot_version < version
          migration = @version_migrations[snapshot_version]
          unless migration
            raise SnapshotError.new(object, "No migration registered for #{name.inspect} from definition version #{snapshot_version} to #{snapshot_version + 1}")
          end

          value = migration.call(value)
          assert_json_compatible!(object, value)
          snapshot_version += 1
        end

        unless states[value, :value]
          raise SnapshotError.new(object, "#{value.inspect} is not a known #{name} state value")
        end

        value
      end

      private

      # Validates the structural shape of a single snapshot entry
      def validate_snapshot_entry!(object, entry)
        unless entry.is_a?(Hash) && entry.keys.all? { |key| key.is_a?(String) }
          raise SnapshotError.new(object, "Invalid snapshot for #{name.inspect}: expected a Hash with String keys")
        end

        unknown_keys = entry.keys - SNAPSHOT_KEYS
        missing_keys = SNAPSHOT_KEYS - entry.keys
        unless unknown_keys.empty? && missing_keys.empty?
          problems = []
          problems << "unknown keys #{unknown_keys.inspect}" unless unknown_keys.empty?
          problems << "missing keys #{missing_keys.inspect}" unless missing_keys.empty?
          raise SnapshotError.new(object, "Invalid snapshot for #{name.inspect}: #{problems.join(', ')}")
        end

        snapshot_version = entry['version']
        return if snapshot_version.is_a?(Integer) && snapshot_version >= 1

        raise SnapshotError.new(object, "Invalid snapshot for #{name.inspect}: version must be a positive integer, got #{snapshot_version.inspect}")
      end

      # Raises StateMachines::SnapshotError unless the given value can be
      # represented as JSON-compatible data
      def assert_json_compatible!(object, value)
        return if json_compatible_value?(value)

        raise SnapshotError.new(object, "Snapshot for #{name.inspect} contains a value that is not JSON-compatible: #{value.inspect}")
      end

      # Determines whether the given value consists only of JSON-compatible
      # data: nil, true, false, strings, integers, finite floats, arrays, and
      # hashes with string keys
      def json_compatible_value?(value)
        case value
        when nil, true, false, String, Integer
          true
        when Float
          value.finite?
        when Array
          value.all? { |element| json_compatible_value?(element) }
        when Hash
          value.all? { |key, element| key.is_a?(String) && json_compatible_value?(element) }
        else
          false
        end
      end
    end
  end
end
