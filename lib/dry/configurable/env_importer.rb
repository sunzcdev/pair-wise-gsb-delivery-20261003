# frozen_string_literal: true

module Dry
  module Configurable
    # Imports values from an environment-like hash into a {Config}, based on the
    # `env` names declared by its settings.
    #
    # @api private
    class EnvImporter
      # @api private
      Mapping = Struct.new(:env_key, :path, :leaf)

      # @api private
      def self.call(config, values, prefix)
        new(config, values, prefix).call
      end

      # @api private
      def initialize(config, values, prefix)
        @config = config
        @values = values
        @prefix = normalize_prefix(prefix)
      end

      # @api private
      def call
        mappings = build_mappings
        assert_unique_env_keys(mappings)

        selected = mappings.select { |mapping| present?(@values, mapping.env_key) }

        targets = resolve_targets(selected)
        assert_mutable(selected, targets)

        snapshots = snapshot_tree(@config)

        transformed = transform_all(selected, snapshots)
        assign_all(selected, targets, transformed, snapshots)

        selected.map(&:env_key)
      end

      private

      def build_mappings
        collect(@config._settings, [], @prefix)
      end

      def collect(settings, path, key_prefix)
        settings.flat_map do |setting|
          setting_path = path + [setting.name]

          if setting.children.any?
            nested_prefix =
              if setting.env
                [key_prefix, setting.env].compact.reject(&:empty?).join("_")
              else
                key_prefix
              end

            collect(setting.children, setting_path, nested_prefix)
          elsif setting.env.nil?
            []
          else
            env_key = [key_prefix, setting.env].compact.reject(&:empty?).join("_")
            [Mapping.new(env_key, setting_path, setting)]
          end
        end
      end

      def assert_unique_env_keys(mappings)
        duplicates = mappings.group_by(&:env_key).select { |_, group| group.size > 1 }

        return if duplicates.empty?

        details = duplicates.map { |env_key, group|
          paths = group.map { |mapping| format_path(mapping.path) }.join(", ")
          "#{env_key.inspect} => #{paths}"
        }.join("; ")

        raise DuplicateEnvKeyError,
          "multiple settings map to the same environment key: #{details}"
      end

      def transform_all(mappings, snapshots)
        mappings.each_with_object({}) do |mapping, transformed|
          raw = fetch(@values, mapping.env_key)

          begin
            transformed[mapping] = mapping.leaf.constructor.(raw)
          rescue => e # rubocop:disable Style/RescueStandardError
            restore(snapshots)
            raise EnvImportError, import_failure_message(mapping, e), cause: e
          end
        end
      end

      def assign_all(mappings, targets, transformed, snapshots)
        mappings.each do |mapping|
          begin
            assign(targets[mapping], mapping.path.last, transformed[mapping])
          rescue => e # rubocop:disable Style/RescueStandardError
            restore(snapshots)
            raise EnvImportError, import_failure_message(mapping, e), cause: e
          end
        end
      end

      def resolve_targets(mappings)
        mappings.each_with_object({}) do |mapping, targets|
          targets[mapping] =
            mapping.path[0..-2].reduce(@config) { |cfg, name|
              values = values_of(cfg)

              values.fetch(name) {
                nested = cfg._settings[name].to_value
                values[name] = nested unless cfg.frozen?
                nested
              }
            }
        end
      end

      def assert_mutable(mappings, targets)
        mappings.each do |mapping|
          next unless targets[mapping].frozen?

          error = FrozenConfigError.new("Cannot modify frozen config")
          raise EnvImportError, import_failure_message(mapping, error), cause: error
        end
      end

      def assign(target, name, value)
        raise FrozenConfigError, "Cannot modify frozen config" if target.frozen?

        configured_of(target).add(name)
        values_of(target)[name] = value
      end

      def snapshot_tree(config)
        {
          config.object_id => {
            config: config,
            values: values_of(config).dup,
            configured: configured_of(config).dup
          }
        }.merge(
          values_of(config).each_value.with_object({}) { |value, snapshots|
            next snapshots unless value.is_a?(Config)

            snapshots.merge!(snapshot_tree(value))
          }
        )
      end

      def restore(snapshots)
        snapshots.each_value do |snapshot|
          config = snapshot[:config]
          next if config.frozen?

          config.instance_variable_set(:@_values, snapshot[:values])
          config.instance_variable_set(:@_configured, snapshot[:configured])
        end
      end

      def present?(values, key)
        values.key?(key) || values.key?(key.to_sym)
      end

      def fetch(values, key)
        return values[key] if values.key?(key)

        values[key.to_sym]
      end

      def import_failure_message(mapping, error)
        "failed to import environment key #{mapping.env_key.inspect} " \
          "into setting #{format_path(mapping.path)}: #{error.message}"
      end

      def format_path(path)
        path.map(&:to_s).join(".")
      end

      def values_of(config)
        config.instance_variable_get(:@_values)
      end

      def configured_of(config)
        config.instance_variable_get(:@_configured)
      end

      def normalize_prefix(prefix)
        return nil if prefix.nil?

        key = prefix.to_s.sub(/_+\z/, "")
        key.empty? ? nil : key
      end
    end
  end
end
