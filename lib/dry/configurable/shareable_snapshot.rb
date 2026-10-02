# frozen_string_literal: true

module Dry
  module Configurable
    # Builds a deeply-copied, frozen and Ractor-shareable snapshot of a finalized {Config}.
    #
    # Unlike {Config#to_data}, which captures mutable values by reference, every mutable value
    # (String, Array, Hash, Set, Time) is copied before being frozen, so later mutation of the
    # original config cannot affect the snapshot. Repeated references and cycles contained in
    # Arrays, Hashes and Sets are preserved (a shared node stays a single shared node).
    #
    # Nested {Config} instances become nested Data instances.
    #
    # @api private
    class ShareableSnapshot
      # Placeholder for a nested Data still being built. Resolved once every config has been
      # copied, which allows cycles nested inside containers to be preserved.
      #
      # @api private
      class Deferred
        # @return [Config]
        attr_reader :config

        # @return [Hash{Symbol => Object}]
        attr_accessor :attrs

        # @return [Class]
        attr_accessor :data_class

        # @return [Data]
        attr_accessor :data

        # @api private
        def initialize(config)
          @config = config
        end
      end

      # Values that can cross Ractor boundaries without being copied.
      IMMUTABLE_TYPES = [NilClass, TrueClass, FalseClass, Integer, Float, Rational, Complex,
                         Symbol].freeze

      # @param config [Config] a finalized config
      #
      # @return [Data]
      #
      # @api private
      def self.call(config)
        new.(config)
      end

      # @api private
      def initialize
        # Every mutable source object (and every Config) mapped to its in-progress copy. This
        # preserves repeated references and terminates cycles.
        @copies = {}
        # Every discovered Config mapped to its Deferred, in discovery order.
        @deferreds = {}
      end

      # @api private
      def call(root_config)
        root = copy_config(root_config, "")

        built = {}
        @deferreds.each_value { |deferred| build(deferred, [], built) }

        sealed = {}
        root.attrs.each_value { |value| seal(value, sealed) }

        unless Ractor.shareable?(root.data)
          raise UnsupportedValueError, "snapshot is not Ractor-shareable"
        end

        root.data
      end

      private

      # Pass 1: turn every Config into a Deferred and every mutable value into an unfrozen copy.

      def copy_config(config, parent_path)
        @copies[config] ||=
          begin
            deferred = Deferred.new(config)
            @deferreds[config] = deferred

            attrs = config._settings.each_with_object({}) do |setting, hsh|
              path = parent_path.empty? ? setting.name.to_s : "#{parent_path}.#{setting.name}"
              hsh[setting.name] = copy_value(config[setting.name], path)
            end

            deferred.attrs = attrs
            deferred.data_class = config._settings.data_class
            deferred
          end
      end

      def copy_value(value, path)
        existing = @copies[value]
        return existing if existing

        # Use strict `instance_of?` rather than `case/when`: subclasses of the supported
        # container and scalar classes are custom objects and must be explicitly rejected.
        if value.is_a?(Config)
          copy_config(value, path)
        elsif value.instance_of?(String)
          # Always dup so the copy is independent even when the source string is mutable.
          register(value, value.dup)
        elsif value.instance_of?(Array)
          copy = register(value, [])
          value.each_with_index { |item, idx| copy << copy_value(item, "#{path}[#{idx}]") }
          copy
        elsif value.instance_of?(Hash)
          copy_hash(value, path)
        elsif value.instance_of?(Set)
          copy = register(value, Set.new)
          value.each { |item| copy << copy_value(item, "#{path}[Set]") }
          copy
        elsif value.instance_of?(Time)
          register(value, value.dup)
        elsif IMMUTABLE_TYPES.any? { |type| value.instance_of?(type) }
          value
        else
          reject!(value, path)
        end
      end

      def copy_hash(hash, path)
        if hash.default_proc
          raise UnsupportedValueError,
                "cannot create a shareable snapshot at #{path}: a Hash with a default_proc is " \
                "not supported (a proc cannot cross Ractor boundaries)"
        end

        copy = register(hash, {})
        copy.compare_by_identity if hash.compare_by_identity?
        copy.default = copy_value(hash.default, "#{path}[default]") unless hash.default.nil?

        hash.each do |key, val|
          copy[copy_value(key, "#{path}[key]")] = copy_value(val, "#{path}[#{key.inspect}]")
        end

        copy
      end

      def register(source, copy)
        @copies[source] = copy
      end

      # Pass 2: resolve Deferreds nested directly in attrs into Data. Container copies are left
      # untouched, which keeps shared nodes shared. A deferred still being built when reached
      # forms a direct config cycle (not carried by a container), which cannot be represented as
      # frozen Data.

      def build(deferred, building, built)
        return built.fetch(deferred) if built.key?(deferred)

        if building.include?(deferred)
          cycle = building.drop_while { |d| d != deferred }.map { |d|
            d.config._settings.keys.join(", ")
          }
          raise UnsupportedValueError,
                "cannot create a shareable snapshot: the config contains a direct cycle " \
                "(#{cycle.join(" -> ")}) that cannot be represented as frozen Data"
        end

        building << deferred

        attrs = deferred.attrs.each_with_object({}) do |(name, value), hsh|
          hsh[name] = value.is_a?(Deferred) ? build(value, building, built) : value
        end
        data = deferred.data_class.allocate
        data.send(:initialize, **attrs)
        deferred.data = data
        built[deferred] = data

        building.pop
        data
      end

      # Pass 3: replace container-carried Deferred placeholders with their built Data and freeze
      # every copy bottom up. Traversing from the root reaches configs nested inside containers
      # too: every Deferred is either a member of another Data or carried by a container.

      def seal(value, sealed)
        return value if value.is_a?(Data)

        oid = value.object_id
        return sealed[oid] if sealed.key?(oid)

        case value
        when Deferred
          sealed[oid] = value.data
          value.attrs.each_value { |attr| seal(attr, sealed) }
          value.data
        when Array
          sealed[oid] = value
          value.map! { |item| seal(item, sealed) }.freeze
        when Hash
          sealed[oid] = value
          value.each do |key, val|
            sealed_key = seal(key, sealed)
            sealed_val = seal(val, sealed)
            if !sealed_key.equal?(key) || !sealed_val.equal?(val)
              value.delete(key)
              value[sealed_key] = sealed_val
            end
          end
          seal(value.default, sealed) unless value.default.nil?
          value.freeze
        when Set
          sealed[oid] = value
          copy = Set.new
          value.each { |item| copy << seal(item, sealed) }
          value.replace(copy).freeze
        when String, Time
          sealed[oid] = value
          value.freeze
        else
          value
        end
      end

      def reject!(value, path)
        raise UnsupportedValueError,
              "cannot create a shareable snapshot at #{path}: #{value.class} values are not " \
              "supported; supported values are nested configs, String, Array, Hash, Set, Time " \
              "and immutable scalars (nil, true/false, Numeric, and Symbol)"
      end
    end
  end
end
