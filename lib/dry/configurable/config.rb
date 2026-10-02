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
      attr_reader :_explicit, :_sources
      protected :_explicit, :_sources

      # @api private
      def initialize(settings, values: {})
        @_settings = settings
        @_values = values
        @_configured = Set.new
        # Setting names that were explicitly assigned through a writer. This is tracked
        # separately from `_configured`, which is also populated when a mutable value is read.
        @_explicit = Set.new
        # Setting name => Source, for settings whose origin differs from the default
        @_sources = {}
      end

      # @api private
      private def initialize_copy(source)
        super
        @_values = source.__send__(:dup_values)
        @_configured = source._configured.dup
        @_explicit = source._explicit.dup
        @_sources = source._sources.dup
      end

      # @api private
      def dup_for_settings(settings, inherited: false)
        if inherited
          __send__(:build_inherited_copy, settings)
        else
          dup.tap { |config| config.instance_variable_set(:@_settings, settings) }
        end
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
        _explicit.add(name)
        _sources[name] = Source[Source::LOCAL, true]

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
      # This is not the same as `source(...).explicit?`:
      #
      # - Explicitly assigning a value that compares equally to the default (for a cloneable
      #   setting) keeps `explicit?` true but makes `configured?` false.
      # - Mutating a cloneable default in place (e.g. `config.tags << :a`) makes `configured?`
      #   true even though there was no explicit assignment, so `explicit?` stays false and the
      #   source remains `:default`.
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

      # Returns the {Source} of the value at the given setting path.
      #
      # The path may be given as a list of keys (`config.source(:db, :user)`) or a single
      # dot-separated string (`config.source("db.user")`). Each step in a nested path is resolved
      # against the corresponding nested config.
      #
      # The source only reports the origin category and whether the value was explicitly assigned;
      # it never records file names or call stacks.
      #
      # @example
      #
      #   class App
      #     extend Dry::Configurable
      #
      #     setting :timeout, default: 30
      #   end
      #
      #   App.config.source(:timeout)
      #   # => #<Dry::Configurable::Config::Source origin=:default explicit=false>
      #
      #   App.config.timeout = 30
      #   App.config.source(:timeout)
      #   # => #<Dry::Configurable::Config::Source origin=:local explicit=true>
      #
      # @param path [Array<String,Symbol>] one or more setting names forming a nested path
      #
      # @return [Source]
      #
      # @raise [ArgumentError] if any segment of the path is not a defined setting
      #
      # @api public
      def source(*path)
        path = path.flat_map { |segment| segment.to_s.split(".") }.map(&:to_sym) unless path.empty?

        raise ArgumentError, "a setting path is required" if path.empty?

        name = path.first
        setting = _settings[name] or
          raise ArgumentError, "+#{name}+ is not a setting name"

        if path.size == 1
          _source_for(name)
        else
          unless setting.children.any?
            raise ArgumentError, "+#{name}+ is not a nested setting"
          end

          _nested_value(name).source(*path[1..])
        end
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

      # Returns a fresh config built from the same settings, with all values reset to their
      # defaults and every setting's {#source source} reset to `:default`/non-explicit. This
      # means no value is treated as configured, locally assigned, or inherited.
      #
      # @return [Config]
      #
      # @api private
      def pristine
        self.class.new(_settings)
      end

      private

      def _source_for(name)
        _sources.fetch(name) { Source[Source::DEFAULT, false] }
      end

      # Resolves a nested config without recording the access in `_configured`, so querying
      # {#source} never changes the answer given by {#configured?}.
      def _nested_value(name)
        _values.fetch(name) { _settings[name].to_value }
      end

      # Builds the config handed to a subclass. Only values that were explicitly assigned or
      # effectively configured (e.g. a mutated mutable default) are carried over, and each is
      # marked as inherited. Read-only materialized values are left behind and re-derived from
      # the subclass's own defaults.
      def build_inherited_copy(settings)
        dup.tap do |copy|
          copy.instance_variable_set(:@_settings, settings)

          values = {}
          configured = Set.new
          sources = {}

          _values.each do |name, value|
            next unless settings.key?(name)
            next unless _explicit.include?(name) || configured?(name)

            configured.add(name)
            sources[name] = Source[Source::INHERITED, _explicit.include?(name)]

            if value.is_a?(self.class)
              values[name] = value.__send__(:build_inherited_copy, value._settings)
            else
              values[name] = copy._values[name]
            end
          end

          copy.instance_variable_set(:@_values, values)
          copy.instance_variable_set(:@_configured, configured)
          copy.instance_variable_set(:@_explicit, Set.new)
          copy.instance_variable_set(:@_sources, sources)
        end
      end

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
