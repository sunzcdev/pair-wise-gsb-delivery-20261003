# frozen_string_literal: true

module Dry
  module Configurable
    # Imports setting values from an env-like hash (e.g. `ENV`), based on the `env:` names
    # declared on settings.
    #
    # The import is atomic: all values are resolved through their setting constructors before
    # any assignment takes place, so a failure leaves the config untouched.
    #
    # @api private
    class EnvImporter
      # @api private
      Mapping = Data.define(:key, :path, :setting) do
        def path_string
          path.join(".")
        end
      end

      # @api private
      attr_reader :source

      # @api private
      attr_reader :prefix

      # @api private
      def initialize(source, prefix: nil)
        @source = source.each_with_object({}) { |(key, value), hsh| hsh[key.to_s] = value }
        @prefix = prefix.to_s
      end

      # Imports the mapped values into the given config.
      #
      # @return [Array<String>] the env keys used for the import (values not included)
      #
      # @api private
      def call(config)
        mappings = collect_mappings(config._settings)

        ensure_no_conflicts!(mappings)
        ensure_writable!(config, mappings)

        assignments = mappings.filter_map { |mapping|
          [mapping, construct(mapping)] if source.key?(mapping.key)
        }

        assignments.each do |mapping, value|
          target_config(config, mapping)._write(mapping.setting.name, value)
        end

        assignments.map { |mapping, _| mapping.key }
      end

      private

      def collect_mappings(settings, path = [])
        settings.each_with_object([]) { |setting, mappings|
          setting_path = [*path, setting.name]

          if setting.children.any?
            mappings.concat(collect_mappings(setting.children, setting_path))
          elsif setting.env_name
            mappings << Mapping.new(
              key: "#{prefix}#{setting.env_name}",
              path: setting_path,
              setting: setting
            )
          end
        }
      end

      def ensure_no_conflicts!(mappings)
        conflicts = mappings.group_by(&:key).select { |_, group| group.size > 1 }
                          .transform_values { |group| group.map(&:path_string) }

        raise EnvConflictError, conflicts unless conflicts.empty?
      end

      def ensure_writable!(config, mappings)
        mappings.each do |mapping|
          if target_config(config, mapping).frozen?
            raise FrozenConfigError, "Cannot modify frozen config"
          end
        end
      end

      def construct(mapping)
        mapping.setting.constructor.(source[mapping.key])
      rescue StandardError => error
        raise EnvImportError.new(
          env_key: mapping.key,
          path: mapping.path_string,
          message: "Failed to import #{mapping.key} into #{mapping.path_string}: #{error.message}"
        )
      end

      def target_config(config, mapping)
        mapping.path[0..-2].reduce(config) { |target, name| target[name] }
      end
    end
  end
end
