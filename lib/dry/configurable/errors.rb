# frozen_string_literal: true

module Dry
  # Shared errors
  #
  # @api public
  module Configurable
    extend Dry::Core::Deprecations["dry-configurable"]

    Error = Class.new(::StandardError)

    FrozenConfigError = Class.new(Error)

    # Raised when importing an env value into a setting fails.
    #
    # Carries the env key and the dot-separated config path of the setting.
    #
    # @api public
    class EnvImportError < Error
      # @api public
      attr_reader :env_key

      # @api public
      attr_reader :path

      def initialize(env_key:, path:, message: nil)
        @env_key = env_key
        @path = path
        super(message || "Failed to import #{env_key} into #{path}")
      end
    end

    # Raised when multiple settings map to the same env key during an import.
    #
    # @api public
    class EnvConflictError < Error
      # @return [Hash{String => Array<String>}] env keys mapped to their conflicting config paths
      #
      # @api public
      attr_reader :conflicts

      def initialize(conflicts)
        @conflicts = conflicts
        details = conflicts.map { |key, paths| "#{key} (#{paths.join(', ')})" }.join(", ")
        super("Conflicting env keys: #{details}")
      end
    end

    AlreadyIncludedError = Class.new(Error)
    deprecate_constant(
      :AlreadyIncludedError,
      message: "`include Dry::Configurable` more than once (if needed)"
    )
  end
end
