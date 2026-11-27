require "flipper/adapter"
require "flipper/typecast"

module Flipper
  module Adapters
    # Public: Test double adapter that stands in for an older adapter which can
    # only store a subset of Flipper's gates. It behaves like the memory
    # adapter for the gates in `supported_gates`, raises an
    # UnsupportedGateError for writes to any other gate, and preserves unknown
    # fields verbatim so data produced by a newer adapter can be inspected and
    # re-exported without being silently dropped.
    class Limited
      include ::Flipper::Adapter

      # Public: Raised when a write targets a gate this adapter declared it
      # cannot store.
      class UnsupportedGateError < Error
        def initialize(gate_key, supported_gates)
          super(
            "#{gate_key.inspect} is not supported by this adapter; " \
            "supported gates are #{supported_gates.to_a.inspect}"
          )
        end
      end

      # Public: The gate keys every adapter knows how to represent.
      KNOWN_GATES = default_config.keys.freeze

      # Public: Returns the frozen Set of gate keys this adapter can store.
      attr_reader :supported_gates

      # Public: source - A features Hash (like Adapters::Memory's) to seed from.
      # options - The Hash of options.
      #           :supported_gates - The Array/Symbol gate keys that can be
      #             written. Defaults to every known gate.
      def initialize(source = nil, supported_gates: KNOWN_GATES)
        gates = Array(supported_gates).map(&:to_sym)
        unknown = gates - KNOWN_GATES
        unless unknown.empty?
          raise ArgumentError, "unknown gate#{"s" if unknown.size > 1}: #{unknown.inspect}"
        end

        @supported_gates = gates.to_set.freeze
        @source = Typecast.features_hash(source)
      end

      # Public: The set of known features.
      def features
        @source.keys.to_set
      end

      # Public: Adds a feature to the set of known features.
      def add(feature)
        @source[feature.key] ||= default_config
        true
      end

      # Public: Removes a feature and all of its gate values.
      def remove(feature)
        @source.delete(feature.key)
        true
      end

      # Public: Clears all of the gate values for a feature.
      def clear(feature)
        @source[feature.key] = default_config
        true
      end

      # Public
      def get(feature)
        @source[feature.key] || default_config
      end

      # Public
      def get_multi(features)
        features.each_with_object({}) do |feature, result|
          result[feature.key] = get(feature)
        end
      end

      # Public: Returns a deep enough copy that callers cannot mutate stored
      # unknown fields through the returned hash. Sets are duplicated while
      # other values (including unknown fields) are passed through untouched.
      def get_all(**_kwargs)
        Typecast.features_hash(@source)
      end

      # Public: Writes supported gates and rejects the rest before the gate
      # value is stored. Feature#enable adds the empty feature shell first, so
      # a rejected write leaves that shell behind but never a gate value.
      def enable(feature, gate, thing)
        gate_key = gate.key.to_sym
        unless @supported_gates.include?(gate_key)
          raise UnsupportedGateError.new(gate_key, @supported_gates)
        end

        @source[feature.key] ||= default_config

        case gate.data_type
        when :boolean
          @source[feature.key] = default_config
          @source[feature.key][gate_key] = thing.value.to_s
        when :integer
          @source[feature.key][gate_key] = thing.value.to_s
        when :set
          @source[feature.key][gate_key] << thing.value.to_s
        when :json
          @source[feature.key][gate_key] = thing.value
        else
          raise "#{gate} is not supported by this adapter yet"
        end

        true
      end

      # Public: Disabling an unsupported gate would still be a write the target
      # cannot represent, so it is rejected just like enabling one.
      def disable(feature, gate, thing)
        gate_key = gate.key.to_sym
        unless @supported_gates.include?(gate_key)
          raise UnsupportedGateError.new(gate_key, @supported_gates)
        end

        @source[feature.key] ||= default_config

        case gate.data_type
        when :boolean
          @source[feature.key] = default_config
        when :integer
          @source[feature.key][gate_key] = thing.value.to_s
        when :set
          @source[feature.key][gate_key].delete(thing.value.to_s)
        when :json
          @source[feature.key].delete(gate_key)
        else
          raise "#{gate} is not supported by this adapter yet"
        end

        true
      end

      # Public
      def inspect
        "#<#{self.class.name}:#{object_id} supported_gates=#{@supported_gates.to_a.inspect}>"
      end
    end
  end
end
