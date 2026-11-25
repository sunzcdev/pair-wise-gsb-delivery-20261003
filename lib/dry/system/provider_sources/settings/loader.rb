# frozen_string_literal: true

module Dry
  module System
    module ProviderSources
      module Settings
        # @api private
        #
        # A single source of setting values together with the values it provides.
        #
        # Sources are ordered from the highest precedence (the process environment) down to
        # the lowest precedence `.env` files. Values are never merged into the live {ENV};
        # each source keeps its own values, which allows settings to be resolved "by source"
        # instead of from one flattened hash.
        class ValueSource
          # @api private
          attr_reader :name

          # @api private
          attr_reader :values

          # @api private
          def initialize(name, values = {})
            @name = name
            @values = values.transform_keys(&:to_s)
            freeze
          end

          # @api private
          def key?(key)
            values.key?(key.to_s)
          end

          # @api private
          def [](key)
            values[key.to_s]
          end
        end

        # @api private
        #
        # A value offered for a setting key by a particular {ValueSource}.
        class Candidate
          # @api private
          attr_reader :key

          # @api private
          attr_reader :value

          # @api private
          attr_reader :source

          # @api private
          def initialize(key, value, source)
            @key = key.to_s
            @value = value
            @source = source
            freeze
          end

          # @api private
          def source_name
            source.name
          end

          # @api private
          def blank?
            value.nil? || (value.respond_to?(:empty?) && value.empty?)
          end
        end

        # @api private
        class Loader
          # @api private
          attr_reader :store

          # @api private
          attr_reader :sources

          # @api private
          def initialize(root:, env:, roots: nil, store: ENV)
            @store = store
            @sources = build_sources(roots || root, env)
          end

          # Returns the value for the key from the highest precedence source that provides
          # one, or nil when no source has the key.
          #
          # @api private
          def [](key)
            candidate(key)&.value
          end

          # Returns every source that carries the key, ordered from highest to lowest
          # precedence, each paired with the value it provides.
          #
          # Explicitly empty values are included: they are genuine values from a source and
          # must mask lower precedence candidates.
          #
          # @api private
          def candidates(key)
            key = key.to_s
            sources.each_with_object([]) { |source, found|
              found << Candidate.new(key, source[key], source) if source.key?(key)
            }
          end

          # @api private
          def candidate(key)
            candidates(key).first
          end

          private

          def build_sources(roots, env)
            [ValueSource.new(:env, env_snapshot)].concat(dotenv_sources(Array(roots), env.to_sym))
          end

          # Snapshot the environment at load time so later mutations (including dotenv files
          # parsed by another loader) cannot leak into this loader's resolution.
          def env_snapshot
            store.to_h
          end

          def dotenv_sources(roots, env)
            multi_root = roots.length > 1

            roots.each_with_index.flat_map { |root, root_index|
              label = multi_root ? "root#{root_index}:" : ""

              dotenv_files(root, env).filter_map { |path|
                values = parse_dotenv_file(path)
                ValueSource.new("#{label}#{File.basename(path)}", values) unless values.empty?
              }
            }
          end

          def parse_dotenv_file(path)
            require "dotenv"
            Dotenv::Environment.new(path, overwrite: true).to_h
          rescue LoadError
            # Do nothing if dotenv is unavailable
            {}
          rescue Errno::ENOENT, Errno::EISDIR
            {}
          end

          def dotenv_files(root, env)
            [
              File.join(root, ".env.#{env}.local"),
              (File.join(root, ".env.local") unless env == :test),
              File.join(root, ".env.#{env}"),
              File.join(root, ".env")
            ].compact
          end
        end
      end
    end
  end
end