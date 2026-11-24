# frozen_string_literal: true

module Dry
  module Configurable
    # A Patch expresses configuration change intent as a set of paths.
    #
    # Each entry targets a setting path (dot-separated for nested settings) and
    # carries one of three actions:
    #
    #   * a plain value, including +nil+ — explicitly pins the setting to that
    #     value. Explicit +nil+ is a real value and is never confused with an
    #     action sentinel;
    #   * {INHERIT} — releases any local override so the setting follows its
    #     defined default (and later default changes) again;
    #   * {REMOVE_OVERRIDE} — removes the local override and any cached value,
    #     returning the setting to its pristine, default-following state.
    #
    # A patch is validated against a target {Config} before any change is made.
    # An invalid path or a value rejected by a constructor aborts the whole
    # application, so no entry of a failing patch ever becomes visible.
    #
    # @example
    #
    #   patch = Dry::Configurable::Patch.new(
    #     "db.host" => "example.com",
    #     "db.port" => Dry::Configurable::REMOVE_OVERRIDE,
    #     "db.user" => Dry::Configurable::INHERIT,
    #     "db.flag" => nil
    #   )
    #
    #   App.config.apply_patch(patch)
    #
    # @api public
    class Patch
      # @return [Array<Array<String, Object>>] the ordered path/action entries
      #
      # @api public
      attr_reader :entries

      # Builds a patch from path/action pairs.
      #
      # @param entries (see #add)
      #
      # @return [Patch]
      #
      # @api public
      def self.[](**entries)
        new(**entries)
      end

      # @api public
      def initialize(**entries)
        @entries = []
        entries.each { |path, action| add(path, action) }
      end

      # Adds an entry to the patch.
      #
      # @param path [String, Symbol] a dot-separated setting path
      # @param action [Object] a plain value (including +nil+), {INHERIT} or
      #   {REMOVE_OVERRIDE}
      #
      # @return [Patch] self
      #
      # @api public
      def add(path, action)
        @entries << [path.to_s, action]
        self
      end

      # Adds an explicit write entry.
      #
      # @param path [String, Symbol]
      # @param value [Object] any value, including +nil+
      #
      # @return [Patch] self
      #
      # @api public
      def set(path, value)
        add(path, value)
      end

      # Adds an inherit-default entry.
      #
      # @param path [String, Symbol]
      #
      # @return [Patch] self
      #
      # @api public
      def inherit(path)
        add(path, INHERIT)
      end

      # Adds a remove-override entry.
      #
      # @param path [String, Symbol]
      #
      # @return [Patch] self
      #
      # @api public
      def remove_override(path)
        add(path, REMOVE_OVERRIDE)
      end

      # Applies the patch to a config, atomically.
      #
      # Every entry is validated first; if any path is unknown or any value is
      # rejected by its constructor, an error is raised without touching the
      # config.
      #
      # @param config [Config]
      #
      # @return [Config] the same config
      #
      # @raise [InvalidPatchPathError] if a path does not map to a setting
      # @raise [InvalidPatchValueError] if a value or action is invalid
      #
      # @api public
      def apply(config)
        # Validate every entry against the static settings tree first. This
        # walk never touches the live config, so a rejected entry leaves no
        # observable trace behind.
        planned = entries.map { |path, action| plan(config, path, action) }

        planned.each do |segments, setting, action, value|
          node_config = materialize(config, segments)

          case action
          when SET
            node_config.__send__(:commit_value, setting.name, value)
          when INHERIT, REMOVE_OVERRIDE
            node_config.__send__(:release_override, setting.name)
          end
        end

        config
      end

      # @return [Boolean]
      #
      # @api public
      def ==(other)
        other.is_a?(self.class) && other.entries == entries
      end
      alias_method :eql?, :==

      # @return [String]
      #
      # @api public
      def inspect
        "#<#{self.class} #{entries.map { |path, action| "#{path.inspect}=>#{action.inspect}" }.join(", ")}>"
      end

      private

      # Resolves and validates a single entry, returning the data needed to
      # commit it later without touching the live config yet.
      def plan(config, path, action)
        segments, setting = resolve(config, path)

        case action
        when INHERIT
          ensure_inheritable!(path, setting)
          [segments, setting, INHERIT, nil]
        when REMOVE_OVERRIDE
          [segments, setting, REMOVE_OVERRIDE, nil]
        else
          ensure_writable!(path, setting)
          value = validate_value(path, setting, action)
          [segments, setting, SET, value]
        end
      end

      # Walks the static settings tree following the path segments. This never
      # reads the live config, so it has no side effects and stays safe when an
      # entry later in the patch turns out to be invalid.
      def resolve(config, path)
        segments = path.to_s.split(".")
        raise InvalidPatchPathError, "patch path cannot be empty" if segments.empty?

        settings = config._settings
        setting = nil

        segments.each_with_index do |segment, index|
          name = segment.to_sym
          setting = settings[name]

          unless setting
            raise InvalidPatchPathError,
              "+#{path}+ is not a valid setting path: +#{segment}+ is not a setting name"
          end

          if index < segments.size - 1
            unless setting.children.any?
              raise InvalidPatchPathError,
                "+#{path}+ is not a valid setting path: +#{segment}+ is not a nested setting"
            end

            settings = setting.children
          end
        end

        [segments, setting]
      end

      # Walks the live config down to the node holding the target setting,
      # materializing nested configs as needed. Only run during the commit
      # phase, after every entry has been validated.
      def materialize(config, segments)
        segments[0...-1].reduce(config) { |node, segment| node.__send__(:nested_config, segment.to_sym) }
      end

      def ensure_writable!(path, setting)
        return unless setting.children.any?

        raise InvalidPatchValueError,
          "+#{path}+ is a nested setting and cannot be pinned to a single value"
      end

      def ensure_inheritable!(path, setting)
        return unless setting.default.eql?(Undefined)

        raise InvalidPatchValueError,
          "+#{path}+ has no defined default to inherit"
      end

      # Runs the constructor up front so a rejected value aborts validation
      # before any entry is committed.
      def validate_value(path, setting, value)
        setting.constructor.(value)
      rescue StandardError => e
        raise InvalidPatchValueError,
          "value #{value.inspect} for +#{path}+ was rejected by its constructor: #{e.message}"
      end
    end
  end
end
