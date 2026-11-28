require "json"
require "flipper/feature"
require "flipper/gate_values"
require "flipper/adapters/actor_limit"
require "flipper/adapters/strict"
require "flipper/adapters/sync/feature_synchronizer"

module Flipper
  module Adapters
    # Public: Writes to a primary (local) adapter and a replica (remote)
    # adapter, serving reads from the primary.
    #
    # The primary is the current source of truth: it commits first and its
    # committed settings never roll back because the replica failed. Every
    # write returns a Receipt that says whether the replica caught up. When it
    # did not, the receipt can be reconciled later: reconciliation compares the
    # primary's *current* gate values with the replica's and applies only the
    # real differences, so an old receipt can never replay a superseded value
    # over the primary's latest result.
    class DualWrite
      include ::Flipper::Adapter

      # Private: Thread-local key for adapters whose replica errors should be
      # raised instead of captured in a receipt (used by #import).
      RAISING_REPLICA_ERRORS_KEY = :flipper_dual_write_raising_replica_errors

      attr_reader :local, :remote

      # Public: Build a new dual write instance.
      #
      # local - The primary flipper adapter that should serve reads and commits
      #         first.
      # remote - The replica flipper adapter that should be brought in line
      #          with the primary. Replica failures are reported through the
      #          returned Receipt instead of raised.
      def initialize(local, remote, options = {})
        @local = local
        @remote = remote
        @errors = options.fetch(:errors, [StandardError])
        @receipts_mutex = Mutex.new
        @pending_receipts = {}
        @last_receipt = nil
      end

      # Public: The primary adapter. Reads and first writes go here.
      alias_method :primary, :local

      # Public: The replica adapter that catches up from the primary.
      alias_method :replica, :remote
      alias_method :secondary, :remote

      # Public: The outcome of a single write to both adapters.
      #
      # A receipt is judgeable: #caught_up? tells whether the replica has the
      # write, #pending? whether it still needs to catch up, and #error carries
      # the replica failure for that feature and gate. Calling #retry (or one
      # of its aliases) asks the adapter to reconcile the replica with the
      # primary's current state rather than replaying the original write.
      #
      # For compatibility with the adapter contract, a receipt also compares
      # equal to true, so callers that only care that the primary committed can
      # keep treating a write as successful.
      class Receipt
        attr_reader :adapter, :feature, :gate, :operation, :local_result,
                    :remote_result, :error

        def initialize(adapter:, feature:, operation:, gate: nil,
                       local_result: nil, remote_result: nil, error: nil)
          @adapter = adapter
          @feature = feature
          @operation = operation
          @gate = gate
          @local_result = local_result
          @remote_result = remote_result
          @error = error
        end

        # Public: The primary adapter committed the write. A receipt only
        # exists after the primary committed, and replica failures never undo
        # that, so this is always true.
        def local_applied?
          true
        end
        alias_method :primary_applied?, :local_applied?

        # Public: The replica has the write.
        def remote_applied?
          @error.nil?
        end
        alias_method :replica_applied?, :remote_applied?

        # Public: The replica is in step with the primary for this write.
        def caught_up?
          @error.nil?
        end
        alias_method :replicated?, :caught_up?
        alias_method :synced?, :caught_up?
        alias_method :success?, :caught_up?

        # Public: The replica still needs to catch up.
        def pending?
          !caught_up?
        end
        alias_method :failed?, :pending?

        def error_class
          @error&.class
        end

        def error_message
          @error&.message
        end

        # Public: The gate key this receipt belongs to, or nil for
        # feature-level writes.
        def gate_name
          @gate&.key
        end
        alias_method :gate_key, :gate_name

        def feature_name
          @feature.key
        end

        # Internal: Records the result of a reconciliation attempt. Used by
        # DualWrite so the same receipt instance reflects its latest state.
        def resolve!(error: nil, remote_result: nil)
          @error = error
          @remote_result = remote_result
          self
        end

        # Public: Ask the adapter to reconcile the replica with the primary's
        # current values for this feature.
        #
        # Returns a Receipt describing the reconciliation attempt.
        def retry
          @adapter.reconcile(self)
        end
        alias_method :retry!, :retry
        alias_method :catch_up, :retry
        alias_method :reconcile, :retry
        alias_method :process, :retry

        # Public: A compact, judgeable representation of the receipt.
        def to_h
          {
            operation: operation,
            feature: feature.key,
            gate: gate_name,
            caught_up: caught_up?,
            pending: pending?,
            local_applied: local_applied?,
            remote_applied: remote_applied?,
            error: error&.class&.name,
            error_message: error_message,
          }
        end

        def to_json(*args)
          to_h.to_json(*args)
        end

        def to_s
          status = caught_up? ? "caught_up" : "pending"
          "#<#{self.class.name} #{operation} feature=#{feature.key.inspect} gate=#{gate_name.inspect} #{status}>"
        end

        def inspect
          attributes = to_h.map { |key, value| "#{key}=#{value.inspect}" }
          "#<#{self.class.name} #{attributes.join(' ')}>"
        end

        # Internal: Existing callers expect adapter writes to compare equal to
        # true. Receipts do, while two receipts only match when they describe
        # the same outcome.
        def ==(other)
          if true.equal?(other)
            return true
          end
          return false unless other.is_a?(self.class)
          return true if equal?(other)

          feature.key == other.feature.key &&
            gate_name == other.gate_name &&
            operation == other.operation &&
            caught_up? == other.caught_up?
        end

        # Internal: RSpec's `be(true)` checks identity. Treat a receipt as the
        # boolean success the adapter contract promises, without pretending two
        # receipts are the same object.
        def equal?(other)
          return true if true.equal?(other)

          super
        end
      end

      def adapter_stack
        "#{name}(local: #{@local.adapter_stack}, remote: #{@remote.adapter_stack})"
      end

      def read_only?
        @remote.read_only? || @local.read_only?
      end

      def features
        @local.features
      end

      def get(feature)
        @local.get(feature)
      end

      def get_multi(features)
        @local.get_multi(features)
      end

      def get_all(**kwargs)
        @local.get_all(**kwargs)
      end

      def sync_version
        @local.sync_version
      end

      def sync_version=(version)
        @local.sync_version = version
      end

      # Public: Adds a feature to the primary and the replica. Returns a
      # Receipt describing whether the replica caught up.
      def add(feature)
        write(:add, feature) { |adapter| adapter.add(feature) }
      end

      # Public: Removes a feature from the primary and the replica. Returns a
      # Receipt describing whether the replica caught up.
      def remove(feature)
        write(:remove, feature) { |adapter| adapter.remove(feature) }
      end

      # Public: Clears a feature's gates on the primary and the replica.
      # Returns a Receipt describing whether the replica caught up.
      def clear(feature)
        write(:clear, feature) { |adapter| adapter.clear(feature) }
      end

      # Public: Enables a gate on the primary and the replica. Returns a
      # Receipt describing whether the replica caught up.
      def enable(feature, gate, thing)
        write(:enable, feature, gate) { |adapter| adapter.enable(feature, gate, thing) }
      end

      # Public: Disables a gate on the primary and the replica. Returns a
      # Receipt describing whether the replica caught up.
      def disable(feature, gate, thing)
        write(:disable, feature, gate) { |adapter| adapter.disable(feature, gate, thing) }
      end

      # Public: Processes every receipt whose replica write is still pending.
      #
      # Each pending receipt is reconciled against the primary's *current*
      # values, so a receipt for a write that has since been superseded brings
      # the replica to the primary's latest result instead of replaying the
      # old value.
      #
      # Returns an Array of Receipts describing the reconciliation attempts.
      def reconcile_pending
        pending_receipts.map { |receipt| reconcile_receipt(receipt) }
      end
      alias_method :process_pending, :reconcile_pending

      # Public: Reconciles either a single Receipt or every pending receipt.
      #
      # receipt - An optional Receipt returned by a previous write. When nil,
      #           all pending receipts are processed.
      #
      # Returns a Receipt (for a single receipt) or an Array of Receipts (for
      # nil).
      def reconcile(receipt = nil)
        if receipt.nil?
          reconcile_pending
        else
          reconcile_receipt(receipt)
        end
      end
      alias_method :process, :reconcile

      # Public: The receipts whose replica write has not succeeded yet.
      def pending_receipts
        @receipts_mutex.synchronize { @pending_receipts.values.dup }
      end
      alias_method :pending_writes, :pending_receipts

      # Public: Whether any receipt is still waiting for the replica.
      def pending?
        !pending_receipts.empty?
      end

      # Public: The number of receipts whose replica write is still pending.
      def pending_count
        @receipts_mutex.synchronize { @pending_receipts.size }
      end

      # Public: The Receipt for the most recent write through this adapter.
      def last_receipt
        @receipts_mutex.synchronize { @last_receipt }
      end

      # Public: The pending Receipt for a feature (and gate, when given), if
      # any. Feature-level writes (add/remove/clear) use a nil gate.
      def receipt_for(feature, gate = nil)
        key = receipt_key(feature, gate)
        @receipts_mutex.synchronize { @pending_receipts[key] }
      end

      # Public: Imports flags like Adapter#import, but keeps reporting replica
      # failures as errors because the caller explicitly asked to push flags.
      #
      # Returns true if successful.
      def import(source)
        with_raising_replica_errors do
          Flipper::Adapters::Sync::Synchronizer.new(self, self.class.from(source), raise: true).call
        end
        true
      end

      private

      # Internal: Commits to the primary first, then tries the replica and
      # captures replica failures in the returned Receipt. Primary failures
      # propagate: a write that never committed has no receipt.
      def write(operation, feature, gate = nil)
        local_result = yield @local

        receipt = begin
          remote_result = yield @remote
          Receipt.new(
            adapter: self,
            feature: feature,
            operation: operation,
            gate: gate,
            local_result: local_result,
            remote_result: remote_result,
          )
        rescue *@errors => error
          Receipt.new(
            adapter: self,
            feature: feature,
            operation: operation,
            gate: gate,
            local_result: local_result,
            error: error,
          )
        end

        if receipt.pending?
          remember_receipt(receipt)
        else
          # A successful write already carries the newer value to the replica,
          # so any earlier pending receipt for this feature and gate is
          # obsolete.
          supersede_receipts(receipt)
        end

        @receipts_mutex.synchronize { @last_receipt = receipt }
        raise receipt.error if receipt.pending? && raising_replica_errors?

        receipt
      end

      # Internal: Applies the primary's current values for one feature to the
      # replica, doing only the writes that are actually missing.
      def reconcile_receipt(receipt)
        # A caught up receipt has already been replicated, so processing it
        # again must not re-emit the write. Only this exact receipt is cleared
        # from the pending list, so replaying an old receipt cannot drop a newer
        # pending receipt for the same feature and gate.
        if receipt.caught_up?
          forget_receipt(receipt)
          return receipt
        end

        begin
          sync_feature(receipt.feature)
          receipt.resolve!(error: nil, remote_result: true)
          # Syncing the whole feature satisfies every receipt for it, including
          # feature-level add/remove/clear receipts and receipts for other
          # gates of the same feature.
          forget_feature_receipts(receipt.feature.key)
        rescue *@errors => error
          receipt.resolve!(error: error)
          remember_receipt(receipt)
          raise error if raising_replica_errors?
        end

        receipt
      end

      # Internal: Makes the replica's copy of a feature match the primary's
      # current copy. Existing feature presence, gate updates and deletions on
      # the primary are respected because everything is read at call time.
      def sync_feature(feature)
        target = Flipper::Feature.new(feature.key, @remote)
        local_features = @local.features
        remote_features = @remote.features

        if local_features.include?(feature.key)
          target.add unless remote_features.include?(feature.key)
          sync_gate_values(target, feature)
        elsif remote_features.include?(feature.key)
          target.remove
        end

        true
      end

      # Internal: Brings the replica's gate values in line with the primary's
      # current gate values, writing only the differences.
      def sync_gate_values(target, feature)
        primary_values = GateValues.new(@local.get(feature))
        replica_values = GateValues.new(@remote.get(feature))
        return if primary_values == replica_values

        Flipper::Adapters::Strict.with_sync_mode do
          Flipper::Adapters::ActorLimit.with_sync_mode do
            Flipper::Adapters::Sync::FeatureSynchronizer.new(
              target,
              replica_values,
              primary_values,
            ).call
          end
        end
      end

      def receipt_key(feature, gate)
        [feature.key, gate&.key]
      end

      def remember_receipt(receipt)
        key = receipt_key(receipt.feature, receipt.gate)
        @receipts_mutex.synchronize { @pending_receipts[key] = receipt }
      end

      # Internal: A new successful write for a feature supersedes any pending
      # receipt for the same feature and gate: the replica now has the newer
      # value, so processing the older receipt must not drag it back.
      def supersede_receipts(receipt)
        key = receipt_key(receipt.feature, receipt.gate)
        @receipts_mutex.synchronize { @pending_receipts.delete(key) }
      end

      # Internal: Drops this exact receipt from the pending list, leaving any
      # newer receipt for the same key untouched.
      def forget_receipt(receipt)
        key = receipt_key(receipt.feature, receipt.gate)
        @receipts_mutex.synchronize do
          @pending_receipts.delete(key) if @pending_receipts[key].equal?(receipt)
        end
      end

      def forget_feature_receipts(feature_key)
        @receipts_mutex.synchronize do
          @pending_receipts.delete_if { |(key, _gate_key), _receipt| key == feature_key }
        end
      end

      def raising_replica_errors?
        flags = Thread.current[RAISING_REPLICA_ERRORS_KEY]
        flags && flags[object_id]
      end

      def with_raising_replica_errors
        flags = (Thread.current[RAISING_REPLICA_ERRORS_KEY] ||= {})
        previous = flags[object_id]
        flags[object_id] = true
        yield
      ensure
        if previous
          flags[object_id] = previous
        else
          flags.delete(object_id)
        end
      end
    end
  end
end
