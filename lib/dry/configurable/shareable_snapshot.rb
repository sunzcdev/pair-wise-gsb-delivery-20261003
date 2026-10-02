# frozen_string_literal: true

module Dry
  module Configurable
    # Deeply copies and freezes resolved config values into an object graph safe to share
    # across Ractors.
    #
    # Duplicate references and cycles within Array, Hash, and Set containers are preserved in
    # the copies: shared nodes are never duplicated, which also prevents infinite recursion.
    #
    # Supported values: nested {Config}, +String+, +Array+, +Hash+, +Set+, +Time+, and
    # Ractor-shareable immutable scalars.
    #
    # Hashes with a +default_proc+, +Proc+s, +IO+s, and unsupported custom objects are rejected
    # with an {UnsupportedValueError} whose message identifies the offending config path.
    #
    # @api private
    class ShareableSnapshot
      # @api private
      def initialize
        # Maps object_id of a source mutable value to its in-progress or finished frozen copy
        @copies = {}
        # Tracks configs whose Data is currently being built, to detect config-level cycles
        @building_configs = {}
      end

      # Builds a frozen {Data} snapshot of a finalized {Config}.
      #
      # @return [Data]
      #
      # @api private
      def call(config)
        copy_config(config, "config")
      end

      private

      def copy_value(value, path)
        case value
        when Config
          copy_config(value, path)
        when Array
          copy_array(value, path)
        when Hash
          copy_hash(value, path)
        when Set
          copy_set(value, path)
        when String, Time
          copy_scalar(value, path)
        when Proc
          reject!(path, "Procs cannot be included in a shareable snapshot")
        when IO
          reject!(path, "IOs cannot be included in a shareable snapshot")
        else
          if ractor_shareable?(value)
            value
          else
            reject!(
              path,
              "unsupported #{value.class} values cannot be included in a shareable snapshot"
            )
          end
        end
      end

      def copy_config(config, path)
        if (data = @copies[config.object_id])
          return data
        end

        if @building_configs.key?(config.object_id)
          raise UnsupportedValueError,
                "#{path}: cyclic nested configs cannot be represented as a shareable snapshot"
        end

        @building_configs[config.object_id] = true

        begin
          attrs = config._settings.each_with_object({}) do |setting, attrs|
            value = config._values.fetch(setting.name)
            attrs[setting.name] = copy_value(value, "#{path}.#{setting.name}")
          end
        ensure
          @building_configs.delete(config.object_id)
        end

        @copies[config.object_id] = config._settings.data_class.new(**attrs)
      end

      def copy_array(array, path)
        return @copies[array.object_id] if @copies.key?(array.object_id)

        copy = []
        @copies[array.object_id] = copy

        array.each_with_index do |item, index|
          copy << copy_value(item, "#{path}[#{index}]")
        end

        copy.freeze
      end

      def copy_hash(hash, path)
        return @copies[hash.object_id] if @copies.key?(hash.object_id)

        if hash.default_proc
          raise UnsupportedValueError,
                "#{path}: hashes with a default_proc cannot be included in a shareable snapshot"
        end

        copy = {}
        @copies[hash.object_id] = copy
        copy.compare_by_identity if hash.compare_by_identity?

        hash.each do |key, value|
          copied_key = copy_value(key, "#{path} key #{key.inspect}")
          copy[copied_key] = copy_value(value, "#{path}[#{key.inspect}]")
        end

        unless hash.default.nil?
          copy.default = copy_value(hash.default, "#{path} default")
        end

        copy.freeze
      end

      def copy_set(set, path)
        return @copies[set.object_id] if @copies.key?(set.object_id)

        copy = Set.new
        @copies[set.object_id] = copy

        set.each_with_index do |item, index|
          copy.add(copy_value(item, "#{path}{#{index}}"))
        end

        copy.freeze
      end

      def copy_scalar(value, path)
        return value if ractor_shareable?(value)

        copy = value.dup.freeze

        unless ractor_shareable?(copy)
          raise UnsupportedValueError,
                "#{path}: unsupported #{value.class} values cannot be included in a " \
                  "shareable snapshot"
        end

        copy
      end

      def reject!(path, reason)
        raise UnsupportedValueError, "#{path}: #{reason}"
      end

      # `Ractor.shareable?` can raise on unfrozen cyclic graphs; treat those as not shareable.
      def ractor_shareable?(value)
        Ractor.shareable?(value)
      rescue StandardError
        false
      end
    end
  end
end
