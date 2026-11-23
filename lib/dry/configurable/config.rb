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
      attr_reader :_followed
      protected :_followed

      # @api private
      def initialize(settings, values: {})
        @_settings = settings
        @_values = values
        @_configured = Set.new
        @_assigned = Set.new
        @_followed = {}
      end

      # @api private
      private def initialize_copy(source)
        super
        @_values = source.__send__(:dup_values)
        @_configured = source._configured.dup
        @_assigned = source._assigned.dup
        @_followed = source._followed.dup
      end

      # @api private
      def dup_for_settings(settings)
        dup.tap { |config| config.instance_variable_set(:@_settings, settings) }
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
        _followed.delete(name)

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

      # Re-follow the given (ancestor) configs for every setting that has not been
      # overridden on this config.
      #
      # This restores the "defaults flow down" behavior that inheritance provides when a
      # config is first copied, but for configs whose ancestors changed their values
      # afterwards:
      #
      # - Settings explicitly assigned on this config keep their values. This includes
      #   assignments of the default value and explicit `nil`s, which are overrides even
      #   though they may compare equally to a default.
      # - Cloneable (mutable) values changed in place after being read also remain
      #   overrides.
      # - Every other setting — including cloneable defaults that were only ever read —
      #   follows the nearest given config in which the setting is itself overridden,
      #   falling back to a fresh default when none of the given configs overrides it.
      #
      # Nested configs are re-inherited per setting, so overridden nested values are kept
      # while the rest follow. The given configs are only read, never modified, and
      # followed cloneable values are duplicated, so this config keeps its own mutable
      # data. A followed setting keeps following on later re-inherits (until it is
      # assigned or mutated here), so repeated ancestor updates keep propagating.
      #
      # @param sources [Config, Array<Config>] configs to follow, nearest ancestor first
      #
      # @return [Config] self
      #
      # @raise [FrozenConfigError] if the config is finalized
      #
      # @api public
      def reinherit!(sources)
        raise FrozenConfigError, "Cannot modify frozen config" if frozen?

        sources = Array(sources)

        _settings.each do |setting|
          name = setting.name

          # Explicitly assigned values (including nil and values equal to the default)
          # remain local overrides
          next if _assigned.include?(name)

          # Settings unknown to every source have nothing to follow
          setting_sources = sources.select { |source| source._settings.key?(name) }
          next if setting_sources.empty?

          current = _values[name]

          if current.is_a?(Config)
            # Re-inherit nested configs per setting, so overridden nested values are
            # kept while the rest follow
            _followed.delete(name)

            nested_sources = setting_sources.filter_map { |source|
              value = source[name]
              value if value.is_a?(Config)
            }
            current.reinherit!(nested_sources)
          elsif _overridden?(name)
            # Cloneable values changed in place after being read remain overrides
            next
          else
            source = setting_sources.find { |src| src._overridden?(name) }

            _values[name] =
              if source
                value = source[name]
                setting.cloneable? ? value.dup : value
              else
                setting.to_value
              end

            # Remember the followed value as the baseline for detecting later in-place
            # mutations, so the setting can keep following until it is truly overridden
            _followed[name] = _values[name].dup if setting.cloneable?
            _configured.delete(name)
          end
        end

        self
      end

      # Returns true if the value for the given key counts as a local override when
      # re-inheriting: explicitly assigned on this config, or (for cloneable values)
      # changed in place since it was captured on read or last followed.
      #
      # @api private
      def _overridden?(name)
        return true if _assigned.include?(name)

        if _followed.key?(name)
          if !_values.key?(name) || _values[name] != _followed[name]
            # Changed in place since it last followed: an override from now on
            _followed.delete(name)
            return true
          end

          return false
        end

        configured?(name)
      end
      protected :_overridden?

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
    end
  end
end
