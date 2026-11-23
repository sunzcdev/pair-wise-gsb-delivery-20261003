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
      def initialize(settings, values: {})
        @_settings = settings
        @_values = values
        @_configured = Set.new
      end

      # @api private
      private def initialize_copy(source)
        super
        @_values = source.__send__(:dup_values)
        @_configured = source._configured.dup
      end

      # @api private
      def dup_for_settings(settings)
        dup.tap { |config| config.instance_variable_set(:@_settings, settings) }
      end

      # Get config value by a key
      #
      # Migration aliases are resolved to their canonical setting.
      #
      # @param [String,Symbol] name
      #
      # @return Config value
      def [](name)
        setting = _settings.resolve(name)

        _values.fetch(setting.name) {
          # Mutable settings may be configured after read
          _configured.add(setting.name) if setting.cloneable?

          setting.to_value.tap { |value|
            _values[setting.name] = value
          }
        }
      end

      # Set config value.
      # Note that finalized configs cannot be changed.
      #
      # Migration aliases are resolved to their canonical setting, so assigning through
      # an alias applies the same constructor and stores the value under the current key.
      #
      # @param [String,Symbol] name
      # @param [Object] value
      def []=(name, value)
        raise FrozenConfigError, "Cannot modify frozen config" if frozen?

        setting = _settings.resolve(name)

        _configured.add(setting.name)

        _values[setting.name] = setting.constructor.(value)
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

      # Imports a hash of values, migrating legacy keys through the aliases declared on
      # the settings.
      #
      # Alias resolution is scoped to each settings section: a key is first matched
      # against the section's setting names, then against its declared aliases. Values
      # arriving through an alias go through exactly the same constructor (and therefore
      # the same type checks) as values set through the canonical key.
      #
      # When the same setting is provided under both its canonical key and an alias (or
      # under several aliases), the constructed values must compare equally (`#==`);
      # otherwise an {AmbiguousValueError} is raised naming the section and the setting.
      #
      # The import is atomic: all alias resolution, constructor calls and ambiguity
      # checks run on a detached copy of the config. If anything fails, this config is
      # left untouched, so the caller can keep using its pre-import settings.
      #
      # @param values [Hash, #to_hash] A hash with new values, possibly using legacy keys
      #
      # @return [Config] self, updated in place
      #
      # @raise [AmbiguousValueError] if a setting is given conflicting values under
      #   multiple keys
      # @raise [ArgumentError] if a key is neither a setting name nor a declared alias
      #   in its section, or a nested section receives a non-hash value
      #
      # @api public
      def import(values)
        raise FrozenConfigError, "Cannot modify frozen config" if frozen?

        values = values.to_hash

        draft = dup
        draft.apply_import(values, [])

        @_settings = draft._settings
        @_values = draft._values
        @_configured = draft._configured

        self
      end

      # Exports the current config as a plain hash containing canonical setting keys only.
      #
      # Migration aliases are never exported; nested configs are converted recursively
      # into hashes, and every value is the fully constructed value produced by the
      # setting's constructor.
      #
      # @return [Hash]
      #
      # @api public
      def export
        to_h
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

      # Recursively applies a (possibly legacy-keyed) hash onto this draft config.
      #
      # `path` holds the canonical setting names of the sections above this one, used to
      # name the section in error messages. All work happens on the draft; the caller is
      # responsible for swapping the state over only once the whole import succeeded.
      #
      # @api private
      def apply_import(entries, path)
        # Groups every provided key by the canonical setting it resolves to in this section
        grouped = entries.group_by do |key, _value|
          _settings.resolve(key).name
        end

        grouped.each do |setting_name, pairs|
          setting = _settings[setting_name]
          section_path = path + [setting_name]

          if setting.children.any?
            apply_nested_import(setting, pairs, section_path)
          else
            apply_leaf_import(setting, pairs, section_path)
          end
        end
      end

      # @api private
      def apply_nested_import(setting, pairs, section_path)
        # Multiple provided keys (canonical key plus aliases) for a nested section must
        # describe the same sub-hash before it is merged one level deeper.
        hashes = pairs.map do |_key, value|
          unless value.respond_to?(:to_hash)
            raise ArgumentError, "#{value.inspect} is not a valid setting value"
          end

          value.to_hash
        end

        reference = hashes.first

        hashes.drop(1).each do |hash|
          unless hash == reference
            raise AmbiguousValueError, ambiguity_message(section_path, pairs.map(&:first))
          end
        end

        self[setting.name].apply_import(reference, section_path)
      end

      # @api private
      def apply_leaf_import(setting, pairs, section_path)
        constructed = pairs.map { |_key, value| setting.constructor.(value) }

        reference = constructed.first

        constructed.drop(1).each do |value|
          unless value == reference
            raise AmbiguousValueError, ambiguity_message(section_path, pairs.map(&:first))
          end
        end

        # The constructor already ran above, so store the constructed value directly and
        # mark the setting as configured rather than constructing a second time.
        @_configured.add(setting.name)
        _values[setting.name] = reference
      end

      # @api private
      def ambiguity_message(section_path, provided_keys)
        "ambiguous values for setting :#{section_path.join(".")}: keys " \
          "#{provided_keys.map(&:inspect).join(", ")} resolve to the same setting " \
          "but do not produce the same constructed value"
      end

      private

      def to_data_attrs
        _values.each_with_object({}) do |(name, value), hsh|
          hsh[name] = value.is_a?(self.class) ? value.to_data : value
        end
      end

      def method_missing(name, *args)
        setting_name = setting_name_from_method(name)
        setting =
          begin
            _settings.resolve(setting_name)
          rescue ArgumentError
            nil
          end

        super unless setting

        if name.end_with?("=")
          self[setting.name] = args[0]
        else
          self[setting.name]
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
