# frozen_string_literal: true

module Dry
  module Configurable
    # A collection of defined settings on a given class.
    #
    # @api private
    class Settings
      include Dry::Equalizer(:settings)

      include Enumerable

      # @api private
      attr_reader :settings

      # @api private
      def initialize(settings = EMPTY_ARRAY)
        @settings = {}
        settings.each { |setting| self << setting }
      end

      # @api private
      private def initialize_copy(source)
        @settings = source.settings.dup
        @data_class = nil
        @alias_map = nil
      end

      # @api private
      def <<(setting)
        ensure_unique_aliases(setting)

        @data_class = nil
        @alias_map = nil
        settings[setting.name] = setting
        self
      end

      # Resolves a setting name within this settings section, following migration
      # aliases declared on the settings.
      #
      # Both canonical names and aliases are accepted; the returned setting is always
      # the canonical one. Alias resolution is scoped to this section, so the same
      # alias may be declared on settings of different nested sections.
      #
      # @param [String,Symbol] name
      #
      # @return [Setting] the canonical setting for the name or one of its aliases
      #
      # @raise [ArgumentError] if the name is neither a setting name nor a declared alias
      #
      # @api private
      def resolve(name)
        name = name.to_sym

        settings[name] || alias_map.fetch(name)
      rescue KeyError
        raise ArgumentError, "+#{name}+ is not a setting name or a declared alias"
      end

      # Returns true if a setting (or a migration alias pointing to one) with the given
      # name is defined in this section.
      #
      # @return [Boolean]
      #
      # @api public
      def key?(name)
        name = name.to_sym
        settings.key?(name) || alias_map.key?(name)
      end

      # Returns a {Data} class with one member per defined setting, memoized for the lifetime of
      # the schema. Invalidated when new settings are added.
      #
      # @return [Class]
      #
      # @api private
      def data_class
        @data_class ||= Data.define(*keys)
      end

      # Returns the setting for the given name, if found.
      #
      # Aliases are not resolved here; use {#resolve} to follow migration aliases.
      #
      # @return [Setting, nil] the setting, or nil if not found
      #
      # @api public
      def [](name)
        settings[name]
      end

      # Returns the list of defined setting names.
      #
      # @return [Array<Symbol>]
      #
      # @api public
      def keys
        settings.keys
      end

      # @api public
      def each(&)
        settings.each_value(&)
      end

      private

      # Maps every declared alias in this section to its canonical setting.
      # Memoized for the lifetime of the schema and invalidated when settings are added.
      def alias_map
        @alias_map ||= settings.each_value.with_object({}) do |setting, map|
          setting.aliases.each { |aliased_name| map[aliased_name] = setting }
        end
      end

      # Ensures the setting's aliases do not shadow an existing setting name or alias in
      # the same section, and that it does not alias itself.
      def ensure_unique_aliases(setting)
        setting.aliases.each do |aliased_name|
          if aliased_name.equal?(setting.name)
            raise ArgumentError,
                  "setting #{setting.name.inspect} cannot declare itself as an alias"
          elsif settings.key?(aliased_name)
            raise ArgumentError,
                  "alias #{aliased_name.inspect} of setting #{setting.name.inspect} " \
                  "conflicts with an existing setting name"
          elsif (owner = alias_map[aliased_name])
            raise ArgumentError,
                  "alias #{aliased_name.inspect} of setting #{setting.name.inspect} " \
                  "is already declared by setting #{owner.name.inspect}"
          end
        end
      end
    end
  end
end
