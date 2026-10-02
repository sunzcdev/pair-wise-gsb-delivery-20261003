# frozen_string_literal: true

module Dry
  module Configurable
    # Config exposes setting values through a convenient API
    #
    # @api public
    class Config
      include Dry::Equalizer(:values)

      # @api private
      attr_reader :_settings

      # @api private
      attr_reader :_values

      # @api private
      attr_reader :_configured
      protected :_configured

      # @api private
      attr_reader :_assigned
      protected :_assigned

      # @api private
      attr_reader :_origins
      protected :_origins

      # @api private
      def initialize(settings, values: {})
        @_settings = settings
        @_values = values
        @_configured = Set.new
        @_assigned = Set.new
        @_origins = {}
      end

      # @api private
      private def initialize_copy(source)
        super
        @_values = source.__send__(:dup_values)
        @_configured = source._configured.dup
        @_assigned = source._assigned.dup
        @_origins = source._origins.dup
      end

      # @api private
      def dup_for_settings(settings)
        dup.tap { |config|
          config.instance_variable_set(:@_settings, settings)
          config.__send__(:mark_inherited!)
        }
      end

      # Get config value by a key
      #
      # @param [String,Symbol] name
      #
      # @return Config value
      def [](name)
        name = name.to_sym

        unless (setting = _settings[name])
          raise ArgumentError, "+#{name}+ is not a setting name"
        end

        _values.fetch(name) {
          # Mutable settings may be configured after read
          _configured.add(name) if setting.cloneable?

          setting.to_value.tap { |value|
            _values[name] = value
          }
        }
      end

      # Set config value.
      # Note that finalized configs cannot be changed.
      #
      # @param [String,Symbol] name
      # @param [Object] value
      def []=(name, value)
        raise FrozenConfigError, "Cannot modify frozen config" if frozen?

        name = name.to_sym

        unless (setting = _settings[name])
          raise ArgumentError, "+#{name}+ is not a setting name"
        end

        _configured.add(name)

        _assigned.add(name)
        _origins[name] = :local

        _values[name] = setting.constructor.(value)
      end

      # Update config with new values
      #
      # @param values [Hash, #to_hash] A hash with new values
      #
      # @return [Config]
      #
      # @api public
      def update(values)
        values.each do |key, value|
          if self[key].is_a?(self.class)
            unless value.respond_to?(:to_hash)
              raise ArgumentError, "#{value.inspect} is not a valid setting value"
            end

            self[key].update(value.to_hash)
          else
            self[key] = value
          end
        end
        self
      end

      # Returns true if the value for the given key has been set on this config.
      #
      # For simple values, this returns true if the value has been explicitly assigned.
      #
      # For cloneable (mutable) values, since these are captured on read, returns true if the value
      # does not compare equally to its corresdponing default value. This relies on these objects
      # having functioning `#==` checks.
      #
      # @return [Bool]
      #
      # @api public
      def configured?(key)
        if _configured.include?(key) && _settings[key].cloneable?
          return _values[key] != _settings[key].to_value
        end

        _configured.include?(key)
      end

      # Returns the {Source provenance} of the value at the given config path.
      #
      # Unlike {#configured?}, this answers "where did this value come from?" rather than "does
      # the current value still look like the default?". The two can differ:
      #
      # - Explicitly assigning a value that happens to equal the default is reported here as
      #   `local` and `assigned?`, while {#configured?} may report `false` for a mutable value
      #   it compares against a fresh default.
      # - Reading a mutable default (e.g. an `Array`) and then changing its contents in place is
      #   not an assignment, so the origin stays `default` and `assigned?` stays `false`; the
      #   change is surfaced via `Source#mutated?`.
      #
      # For a nested config path, the result aggregates the provenance of the whole group:
      # the origin is the most specific category present (`local` over `inherited` over
      # `default`), `assigned?` is true if any setting was assigned, and `mutated?` is true if
      # any default-sourced mutable value was changed in place.
      #
      # @param path [Array<String,Symbol>] one or more nested setting names
      #
      # @return [Source]
      #
      # @raise [ArgumentError] if any segment of the path is not a defined setting
      #
      # @api public
      def source(*path)
        config = self

        path.each_with_index do |segment, index|
          name = segment.to_sym

          unless (setting = config._settings[name])
            raise ArgumentError, "+#{name}+ is not a setting name"
          end

          if index == path.length - 1
            return config.__send__(:source_for, name)
          end

          unless setting.children.any?
            raise ArgumentError, "+#{name}+ is not a nested setting"
          end

          nested = config._values[name]
          nested = setting.to_value unless nested.is_a?(self.class)
          config = nested
        end

        # No path given: aggregate provenance across the whole config
        Config.aggregate_sources(self)
      end

      # Returns the current config values.
      #
      # Nested configs remain in their {Config} instances.
      #
      # @return [Hash]
      #
      # @api public
      def values
        # Ensure all settings are represented in values
        _settings.each { |setting| self[setting.name] unless _values.key?(setting.name) }

        _values
      end

      # Returns config values as a hash, with nested values also converted from {Config} instances
      # into hashes.
      #
      # @return [Hash]
      #
      # @api public
      def to_h
        values.transform_values { |value| value.is_a?(self.class) ? value.to_h : value }
      end

      # @api private
      alias_method :_dry_equalizer_hash, :hash

      # @api public
      def hash
        return @__hash__ if instance_variable_defined?(:@__hash__)

        _dry_equalizer_hash
      end

      # Returns a frozen {Data} representation of the config's resolved values.
      #
      # The Data exposes each setting as a real method, sidestepping the {method_missing}
      # dispatch on the read path. Intended for performance-sensitive code that reads the same
      # config repeatedly (e.g. per-request render hot paths). A new object is returned per call,
      # so consumers should memoize the result.
      #
      # Only available on a finalized config. Nested configs are converted recursively to nested
      # Data instances. Values are captured by reference, so in-place mutation of a captured
      # value remains visible through the Data; finalize with `freeze_values: true` if you want
      # to prevent that.
      #
      # The returned Data uses Ruby's default structural `==`/`eql?`/`hash`. If you want to use
      # it as an identity-based cache key (e.g. to skip walking N members on every lookup),
      # memoize the reference yourself and either:
      #
      #   1. key it by `object_id` directly:
      #
      #        @cached = view_class.config.to_data
      #        cache[@cached.object_id] = ...
      #
      #   2. use a `compare_by_identity` cache:
      #
      #        cache = {}.compare_by_identity
      #        cache[@cached] = ...
      #
      # Either avoids the contract-breaking `hash = object_id` override and keeps Data's
      # structural equality available for general use.
      #
      # @return [Data]
      #
      # @raise [FrozenConfigError] if the config is not yet finalized
      #
      # @api public
      def to_data
        unless frozen?
          raise FrozenConfigError, "config must be finalized before #to_data can be called"
        end

        _settings.data_class.new(**to_data_attrs)
      end

      # @api public
      def finalize!(freeze_values: false)
        return self if frozen?

        values.each_value do |value|
          if value.is_a?(self.class)
            value.finalize!(freeze_values: freeze_values)
          elsif freeze_values
            value.freeze
          end
        end

        # Memoize the hash for the object when finalizing (regardless of whether values themselves
        # are to be frozen; the intention of finalization is that no further changes should be
        # made). The benefit of freezing the hash at this point is that it saves repeated expensive
        # computation (through Dry::Equalizer's hash implementation) if that hash is to be used
        # later in performance-sensitive situations, such as when serving as a cache key or similar.
        @__hash__ = _dry_equalizer_hash

        freeze
      end

      # @api private
      def pristine
        self.class.new(_settings)
      end

      private

      def to_data_attrs
        _values.each_with_object({}) do |(name, value), hsh|
          hsh[name] = value.is_a?(self.class) ? value.to_data : value
        end
      end

      def method_missing(name, *args)
        setting_name = setting_name_from_method(name)
        setting = _settings[setting_name]

        super unless setting

        if name.end_with?("=")
          self[setting_name] = args[0]
        else
          self[setting_name]
        end
      end

      def respond_to_missing?(meth, include_private = false)
        _settings.key?(setting_name_from_method(meth)) || super
      end

      def setting_name_from_method(method_name)
        method_name.to_s.tr("=", "").to_sym
      end

      def dup_values
        _values.each_with_object({}) { |(key, val), dup_hsh|
          dup_hsh[key] = _settings[key].cloneable? ? val.dup : val
        }
      end

      # Recursively re-label locally-assigned values as inherited. Used when deriving a subclass
      # config from a superclass config (via {#dup_for_settings}).
      def mark_inherited!
        _assigned.each { |name| _origins[name] = :inherited }

        _values.each_value do |value|
          value.__send__(:mark_inherited!) if value.is_a?(self.class)
        end
      end

      def source_for(name)
        setting = _settings[name]

        if setting.children.any?
          nested = _values[name]
          nested = setting.to_value unless nested.is_a?(self.class)
          return Config.aggregate_sources(nested)
        end

        origin = _assigned.include?(name) ? _origins.fetch(name, :local) : :default

        if origin.equal?(:default) &&
           setting.cloneable? &&
           _values.key?(name) &&
           _values[name] != setting.to_value
          Source.new(origin: :default, assigned: false, mutated: true)
        else
          Source.new(
            origin: origin,
            assigned: origin.equal?(:local) || origin.equal?(:inherited),
            mutated: false
          )
        end
      end

      class << self
        # Aggregate provenance across every setting in the given config (recursively for nested
        # configs).
        def aggregate_sources(config)
          sources = config._settings.map { |setting|
            config.__send__(:source_for, setting.name)
          }

          origins = sources.map(&:origin)

          origin =
            if origins.include?(:local)
              :local
            elsif origins.include?(:inherited)
              :inherited
            else
              :default
            end

          Source.new(
            origin: origin,
            assigned: sources.any?(&:assigned?),
            mutated: sources.any?(&:mutated?)
          )
        end
      end
    end
  end
end
