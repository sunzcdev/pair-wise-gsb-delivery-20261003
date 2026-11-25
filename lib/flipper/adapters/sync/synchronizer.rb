require "set"
require "flipper/feature"
require "flipper/gate_values"
require "flipper/adapters/actor_limit"
require "flipper/adapters/strict"
require "flipper/adapters/sync/change_set"
require "flipper/adapters/sync/feature_synchronizer"

module Flipper
  module Adapters
    class Sync
      # Public: Given a local and remote adapter, it can update the local to
      # match the remote doing only the necessary enable/disable operations.
      #
      # It also applies single updates with #apply: a complete mirror (full
      # change set, overwrites) or an increment (only what it mentions, with
      # deletions stated explicitly). Every update settles at one version, so
      # its additions, changes and deletions become visible together, and an
      # older update arriving late or replayed never resurrects a removed gate.
      class Synchronizer
        # Internal: Sentinel for "no change set was given, read the remote".
        NO_CHANGES = Object.new.freeze
        private_constant :NO_CHANGES

        # Public: The version the last applied update settled at, or nil when
        # nothing settled.
        attr_reader :applied_version

        # Public: Initializes a new synchronizer.
        #
        # local - The Flipper adapter to get in sync with the remote.
        # remote - The Flipper adapter that is source of truth that the local
        #          adapter should be brought in line with.
        # options - The Hash of options.
        #           :instrumenter - The instrumenter used to instrument.
        #           :raise - Should errors be raised (default: true).
        #           :cache_bust - Should cache busting be used for remote get_all (default: false).
        def initialize(local, remote, options = {})
          @local = local
          @remote = remote
          @instrumenter = options.fetch(:instrumenter, Instrumenters::Noop)
          @raise = options.fetch(:raise, true)
          @cache_bust = options.fetch(:cache_bust, false)
        end

        # Public: Forces a sync, or applies one update.
        #
        # changes - Optional. A ChangeSet, or anything ChangeSet.from
        #           understands, describing one update. When omitted a complete
        #           mirror is read from the remote, exactly as before.
        #
        # Returns nothing.
        def call(changes = NO_CHANGES, **options)
          @instrumenter.instrument("synchronizer_call.flipper") do
            Flipper::Adapters::Strict.with_sync_mode do
              Flipper::Adapters::ActorLimit.with_sync_mode do
                if changes.equal?(NO_CHANGES) && options.empty?
                  sync_from_remote
                elsif changes.equal?(NO_CHANGES)
                  apply_change_set(ChangeSet.from({}, options))
                else
                  apply_change_set(ChangeSet.from(changes, options))
                end
              end
            end
          end

          nil
        rescue => exception
          @instrumenter.instrument("synchronizer_exception.flipper", exception: exception)
          raise if @raise
          nil
        end

        # Public: Applies one update (a complete mirror or an increment) to the
        # local adapter. See #call.
        def apply(changes, **options)
          call(changes, **options)
        end

        private

        # Internal: Reads a complete mirror from the remote and applies it.
        # Features and gates the remote does not report are removed, which is
        # the original sync contract.
        def sync_from_remote
          remote_get_all = @remote.get_all(cache_bust: @cache_bust)
          remote_version = @remote.sync_version
          apply_change_set(ChangeSet.full(remote_get_all, version: remote_version))
        end

        # Internal: Settles one change set against the local adapter.
        #
        # The update is decided in full before anything is written: the new
        # state is built in memory from the current one, and only then applied.
        # A failure while deciding it leaves the previous complete version
        # untouched, and a failure while writing it is rolled back, so readers
        # only ever see complete versions.
        def apply_change_set(change_set)
          local_version = @local.sync_version
          version = change_set.sync_version(local_version)
          @applied_version = nil

          if version && local_version && version.older_than?(local_version)
            # A slow or replayed older update must never be applied: it would
            # resurrect gates a newer version already removed.
            @instrumenter.instrument("synchronizer_stale.flipper", {
              local_version: local_version,
              remote_version: version,
            })
            return
          end

          if version && local_version && version == local_version && change_set.incremental?
            # The local adapter already settled this version, and an increment
            # only describes a delta from before it. Applying it now could only
            # put back values that version settled away (a mirror that removed a
            # gate, then the increment that preceded it arriving again), so it
            # is treated as already settled.
            @instrumenter.instrument("synchronizer_replay.flipper", {
              local_version: local_version,
              remote_version: version,
            })
            return
          end

          before = @local.get_all
          settled = settle(change_set, before)

          record_version = version && version != local_version ? version : nil
          publish(before, settled, record_version)
          @applied_version = version || local_version
        end

        # Internal: Decides the complete state the change set settles to,
        # without writing anything to the local adapter.
        #
        # Returns the Hash of feature key => gate values hash.
        def settle(change_set, before)
          gates_by_key = before.each_with_object({}) do |(key, gates), result|
            result[key] = gates
          end

          if change_set.full?
            # A complete mirror drops the local features the remote stopped
            # reporting. An increment only drops what it explicitly deletes.
            (before.keys - change_set.keys).each { |key| gates_by_key.delete(key) }
          end

          change_set.features.each do |key, change|
            if change.deleted?
              gates_by_key.delete(key)
              next
            end

            if change_set.full?
              # Complete feature: mirror it exactly (overwrite semantics).
              gates_by_key[key] = default_gates.merge(change.gates)
            else
              # Increment: only the mentioned gates settle. Everything else
              # keeps exactly what the local adapter already had.
              local_values = gates_by_key[key]
              if local_values.nil?
                # Clearing gates of a feature the mirror never had has nothing
                # to settle, and must not create the feature.
                next if change.gates.empty?
                # A mentioned feature with values exists remotely.
                local_values = default_gates
              end

              gates_by_key[key] = change.merged_gate_values(local_values)
            end
          end

          gates_by_key
        end

        # Internal: Writes the settled state and its version to the local
        # adapter as one settle.
        #
        # The version is written last, so the version readers see always
        # describes the flags they see. If a write fails, the previous complete
        # state is put back before the error leaves this method.
        def publish(before, after, version)
          begin
            write_snapshot(before, after)
            @local.sync_version = version if version
          rescue
            restore_snapshot(before)
            raise
          end
        end

        # Internal: Writes only the features and gates that differ.
        #
        # The settled state is written gate by gate rather than through the
        # complete mirror shortcut, because a settled state can legitimately
        # hold an enabled boolean gate next to other gates that the update did
        # not change.
        def write_snapshot(before, after)
          after.each do |key, gates|
            known = before.key?(key)
            local_gates = before[key] || default_gates

            # GateValues typecasts both sides, so equivalent values written in
            # different shapes (true vs "true", nil vs []) are not rewritten.
            next if known && GateValues.new(local_gates) == GateValues.new(gates)

            feature = Feature.new(key, @local, instrumenter: @instrumenter)

            # Adding keeps a feature the remote reports but this adapter never
            # had, even when every one of its gates is at the default value.
            feature.add unless known

            settle_gates(feature, local_gates, gates)
          end

          (before.keys - after.keys).each do |key|
            Feature.new(key, @local, instrumenter: @instrumenter).remove
          end
        end

        # Internal: Writes one feature to an exactly known state. Every gate is
        # named as changed, so nothing keeps a value the settled state dropped.
        def settle_gates(feature, local_gates, gates)
          FeatureSynchronizer.new(
            feature,
            GateValues.new(local_gates),
            GateValues.new(gates),
            changed_gates: FeatureSynchronizer::GATE_KEYS,
          ).call
        end

        # Internal: Best effort put back of the previous complete state after a
        # failed write. Each difference is restored independently so a single
        # stubborn write cannot leave the rest of the update behind.
        def restore_snapshot(before)
          current = @local.get_all

          before.each do |key, gates|
            local_gate_values = GateValues.new(current[key] || default_gates)
            previous_gate_values = GateValues.new(gates)
            next if local_gate_values == previous_gate_values

            feature = Feature.new(key, @local, instrumenter: @instrumenter)
            begin
              feature.add unless current.key?(key)
              settle_gates(feature, current[key] || default_gates, gates)
            rescue
              nil
            end
          end

          (current.keys - before.keys).each do |key|
            begin
              Feature.new(key, @local, instrumenter: @instrumenter).remove
            rescue
              nil
            end
          end
        rescue
          nil
        end

        def default_gates
          @default_gates ||= @local.default_config
        end
      end
    end
  end
end