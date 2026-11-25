# frozen_string_literal: true

require "dry/configurable"
require "set"

module Dry
  module System
    module ProviderSources
      # @api private
      module Settings
        # @api private
        #
        # A single failed setting resolution. Carries only the setting key, the source the
        # failing value came from (if any) and the categorized failure type. The underlying
        # exception message is never exposed.
        class Failure
          # @api private
          TYPES = %i[missing blank invalid group].freeze

          # @api private
          attr_reader :key

          # @api private
          attr_reader :source

          # @api private
          attr_reader :type

          # @api private
          def initialize(key:, type:, source: nil)
            raise ArgumentError, "unknown failure type: #{type.inspect}" unless TYPES.include?(type)

            @key = key
            @type = type
            @source = source
            freeze
          end

          # @api private
          def to_s
            location = source ? " (source: #{source})" : ""
            "#{key}#{location}: #{type}"
          end
        end

        # @api private
        class InvalidSettingsError < ArgumentError
          # @api private
          def initialize(failures)
            message = <<~STR
              Could not load settings. The following settings were invalid:

              #{failure_lines(failures).join("\n")}
            STR

            super(message)
          end

          private

          def failure_lines(failures)
            failures.sort_by(&:key).map(&:to_s)
          end
        end

        # @api private
        #
        # A named collection of settings that must be validated together. Values are only ever
        # taken from a single source for the whole group, so a group either resolves to one
        # coherent, jointly-valid set of values or fails as a whole.
        class Group
          # @api private
          attr_reader :name

          # @api private
          attr_reader :keys

          # @api private
          attr_reader :validator

          # @api private
          def initialize(name, keys:, validator: nil)
            @name = name
            @keys = keys.map(&:to_sym)
            @validator = validator
            freeze
          end

          # Returns true when the values pass the validator, otherwise the rescued exception.
          #
          # @api private
          def validate(values)
            return true unless validator

            validator.call(values)
            true
          rescue => exception # rubocop:disable Style/RescueStandardError
            exception
          end
        end

        # @api private
        class Config
          # @api private
          def self.load(root:, env:, loader: Loader, roots: nil)
            loader = loader.new(root: root, env: env, roots: roots)

            new.tap do |settings_obj|
              resolver = Resolver.new(settings_obj.config, loader, setting_groups)
              failures = resolver.resolve

              raise InvalidSettingsError, failures unless failures.empty?
            end
          end

          include Dry::Configurable

          # Declares a group of settings that are validated together.
          #
          # Every setting declared inside the block belongs to the group. When the settings are
          # loaded, values for the whole group are taken from one source at a time (highest
          # precedence first) and handed to the optional validator as a hash of setting names
          # and values. The first source whose values are all present and pass both the setting
          # constructors and the validator wins; if no source qualifies, the group fails.
          #
          # @example
          #   settings_group(:connection_pool) do
          #     setting :pool_min, constructor: Types::Integer
          #     setting :pool_max, constructor: Types::Integer
          #   end
          #
          # A group validator can be given to enforce relationships between the settings. It
          # receives a hash of setting names and their resolved values and should raise when the
          # set is jointly invalid:
          #
          # @example
          #   settings_group(:connection_pool, validate: -> values {
          #     raise "pool_max must be >= pool_min" if values[:pool_max] < values[:pool_min]
          #   }) do
          #     setting :pool_min
          #     setting :pool_max
          #   end
          #
          # @param validate [#call,nil] optional validator called with the group's resolved values
          #
          # @api public
          def self.settings_group(name, validate: nil, &block)
            group = Group.new(name, keys: [], validator: validate)
            previous = @__current_settings_group__
            @__current_settings_group__ = group

            class_eval(&block)
          ensure
            @__current_settings_group__ = previous
            setting_groups[name.to_sym] = group
          end

          # @api private
          def self.setting(name, ...)
            super

            if (group = @__current_settings_group__)
              group.keys << name.to_sym
            end

            self
          end

          # @api private
          def self.setting_groups
            @setting_groups ||= {}
          end

          # @api private
          def self.inherited(subclass)
            super
            subclass.instance_variable_set(:@setting_groups, setting_groups.transform_values { |group|
              Group.new(group.name, keys: group.keys.dup, validator: group.validator)
            })
          end

          private

          def method_missing(name, ...)
            if config.respond_to?(name)
              config.public_send(name, ...)
            else
              super
            end
          end

          def respond_to_missing?(name, include_all = false)
            config.respond_to?(name, include_all) || super
          end
        end

        # @api private
        #
        # Resolves settings against a {Loader}, keeping per-source provenance for every value.
        class Resolver
          # @api private
          attr_reader :config

          # @api private
          attr_reader :loader

          # @api private
          attr_reader :groups

          # @api private
          def initialize(config, loader, groups)
            @config = config
            @loader = loader
            @groups = groups
            @grouped_keys = groups.values.flat_map(&:keys).to_set
          end

          # @api private
          def resolve
            failures = []

            ungrouped_settings.each { |setting|
              failure = resolve_ungrouped(setting)
              failures << failure if failure
            }

            groups.each_value { |group|
              failures.concat(resolve_group(group))
            }

            failures
          end

          private

          def ungrouped_settings
            config._settings.reject { |setting| @grouped_keys.include?(setting.name) }
          end

          # Plain keys keep per-key precedence: the highest source that carries the key wins,
          # including explicit empty values.
          def resolve_ungrouped(setting)
            candidate = loader.candidate(setting.name.to_s.upcase)

            if candidate.nil?
              read_default(setting)
            else
              assign(setting, candidate)
            end
          end

          # A group resolves to one source at a time. Each source that provides a value for
          # every group key is tried in precedence order; the first whose values pass both the
          # constructors and the group validator is accepted wholesale.
          def resolve_group(group)
            env_key_map = group.keys.to_h { |name| [name, name.to_s.upcase] }

            complete_sources(group, env_key_map).each do |source, candidates|
              outcome = try_group(group, source, candidates)
              # A nil outcome means the source produced a jointly-valid group; stop searching.
              return [] if outcome.nil?
            end

            group_failure(group, env_key_map)
          end

          # Sources are only eligible when they carry every key of the group; a partial source
          # can never form a jointly-valid set.
          def complete_sources(group, env_key_map)
            loader.sources.filter_map { |source|
              candidates = group.keys.filter_map { |name|
                key = env_key_map[name]
                Candidate.new(key, source[key], source) if source.key?(key)
              }

              [source, candidates] if candidates.length == group.keys.length
            }
          end

          def try_group(group, source, candidates)
            assigned = {}

            candidates.each do |candidate|
              setting = config._settings[candidate.key.downcase.to_sym]
              failure = assign(setting, candidate)

              if failure
                rollback(group)
                return [failure]
              end

              assigned[setting.name] = config[setting.name]
            end

            result = group.validate(assigned)
            return nil if result == true

            rollback(group)
            [Failure.new(key: group.name, type: :group, source: source.name)]
          end

          # When no source yields a valid group, report any keys missing from every source,
          # alongside a single failure for the group itself.
          def group_failure(group, env_key_map)
            missing = group.keys.filter_map { |name|
              Failure.new(key: name, type: :missing) if loader.candidate(env_key_map[name]).nil?
            }

            missing + [Failure.new(key: group.name, type: :group)]
          end

          def rollback(keys)
            Array(keys).each { |name| config._values.delete(name) }
          end

          def assign(setting, candidate)
            if candidate.blank?
              return Failure.new(
                key: setting.name,
                type: :blank,
                source: candidate.source_name
              )
            end

            config.public_send(:"#{setting.name}=", candidate.value)
            nil
          rescue # rubocop:disable Style/RescueStandardError
            Failure.new(
              key: setting.name,
              type: :invalid,
              source: candidate.source_name
            )
          end

          def read_default(setting)
            config[setting.name]
            nil
          rescue # rubocop:disable Style/RescueStandardError
            Failure.new(
              key: setting.name,
              type: $!.message.start_with?("nil") ? :missing : :invalid
            )
          end
        end
      end
    end
  end
end