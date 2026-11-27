require "set"
require "flipper/feature"
require "flipper/gate_values"
require "flipper/adapters/sync/feature_synchronizer"

module Flipper
  module Adapters
    class Sync
      # Public: A readable, write-free plan for importing a source (a DSL,
      # adapter or export) into a target adapter that cannot represent every
      # gate. The plan lists which features migrate in full and which ones use
      # gates the target cannot store, in which case the target's existing
      # settings for those features are left alone on apply.
      class CompatibilityPlan
        # Internal: Gate keys that count as "enabled" when their value is not
        # blank. Boolean is handled separately because nil means disabled.
        NON_BOOLEAN_GATE_KEYS = %i[
          actors
          groups
          expression
          percentage_of_actors
          percentage_of_time
        ].freeze

        # Public: Initializes a new plan. Nothing is written by this method.
        #
        # target - The adapter that will receive the compatible flags.
        # source - The DSL, adapter or Flipper::Export holding the flags to
        #          migrate.
        def initialize(target, source)
          @target = target
          @source_adapter = target.class.from(source)
        end

        # Public: Returns the Set of feature keys (Strings) whose enabled gates
        # are all expressible by the target adapter.
        def compatible_features
          @compatible_features ||= feature_gates.reject do |_feature_key, gates|
            unsupported_gates(gates).any?
          end.keys.to_set
        end

        # Public: Returns the Set of feature keys (Strings) that need at least
        # one gate the target adapter cannot store.
        def incompatible_features
          @incompatible_features ||= feature_gates.select do |_feature_key, gates|
            unsupported_gates(gates).any?
          end.keys.to_set
        end

        # Public: Returns the Set of unsupported gate keys (Symbols) that the
        # given feature relies on. Empty for fully compatible features.
        def unsupported_gates_for(feature_key)
          unsupported_gates(feature_gates.fetch(feature_key.to_s, default_config))
        end

        # Public: Returns true when every source feature can be migrated in
        # full by the target adapter.
        def fully_migratable?
          incompatible_features.empty?
        end

        # Public: Applies exactly the range the precheck marked compatible.
        # Each compatible feature is synced in full (added first if the target
        # does not know it yet). Features with unsupported gates are skipped
        # entirely so the target's existing settings for them are preserved,
        # and features the target holds but the source does not are left alone.
        # Exact mirroring, including removals, stays the job of the regular
        # full-batch import. Returns self so the plan stays inspectable.
        def apply
          local_get_all = @target.get_all
          source_get_all = @source_adapter.get_all

          compatible_features.each do |feature_key|
            feature = Feature.new(feature_key, @target)
            feature.add unless local_get_all.key?(feature_key)

            local_gates_hash = local_get_all.key?(feature_key) ? local_get_all[feature_key] : default_config
            local_gate_values = GateValues.new(local_gates_hash)
            source_gate_values = GateValues.new(source_get_all.fetch(feature_key))
            FeatureSynchronizer.new(feature, local_gate_values, source_gate_values).call
          end

          self
        end

        # Public: Human-readable summary of the migration scope.
        def to_s
          lines = []
          lines << "Compatible features (#{compatible_features.size}): " \
                   "#{compatible_features.to_a.sort.inspect}"
          incompatible_features.each do |feature_key|
            lines << "#{feature_key}: unsupported gates " \
                     "#{unsupported_gates_for(feature_key).to_a.inspect}; " \
                     "target's existing settings will be kept"
          end
          lines.join("\n")
        end
        alias_method :inspect, :to_s

        private

        def feature_gates
          @feature_gates ||= @source_adapter.get_all
        end

        def default_config
          @target.default_config
        end

        def supported_gates
          @supported_gates ||=
            if @target.respond_to?(:supported_gates)
              @target.supported_gates.map(&:to_sym).to_set
            else
              # Adapters that don't declare capabilities are assumed, like the
              # stock storage adapters, to express every gate Flipper knows.
              default_config.keys.to_set
            end
        end

        def unsupported_gates(gates_hash)
          enabled_gates(gates_hash) - supported_gates
        end

        def enabled_gates(gates_hash)
          gates = Set.new
          gates << :boolean if gates_hash[:boolean]

          NON_BOOLEAN_GATE_KEYS.each do |gate_key|
            value = gates_hash[gate_key]
            next if value.nil?

            case value
            when Set, Array then gates << gate_key unless value.empty?
            when String then gates << gate_key unless value.empty?
            else gates << gate_key
            end
          end

          gates
        end
      end
    end
  end
end
