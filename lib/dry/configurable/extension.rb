# frozen_string_literal: true

module Dry
  module Configurable
    class Extension < Module
      # @api private
      attr_reader :config_class

      # @api private
      attr_reader :default_undefined

      # @api private
      def initialize(config_class: Configurable::Config, default_undefined: false)
        super()
        @config_class = config_class
        @default_undefined = default_undefined
        freeze
      end

      # @api private
      def extended(klass)
        super
        previous = klass.instance_variable_get(:@__config_extension__)
        klass.extend(ClassMethods)
        klass.instance_variable_set(:@__config_extension__, self)
        reset_state_if_extension_changed(klass, previous)
      end

      # @api private
      def included(klass)
        super

        previous = klass.instance_variable_get(:@__config_extension__)

        klass.class_eval do
          extend(ClassMethods)
          include(InstanceMethods)
          prepend(Initializer)

          class << self
            undef :config if method_defined?(:config)
            undef :configure if method_defined?(:configure)
          end
        end

        klass.instance_variable_set(:@__config_extension__, self)
        reset_state_if_extension_changed(klass, previous)
      end

      private

      # Drops the config object and DSL memoized for a *different* extension, so the class
      # rebuilds its config (and therefore its nested configs) with this extension's config
      # class and default behavior.
      #
      # This only happens when the extension's behavior actually changes. Re-applying an
      # equivalent extension is a no-op here, so a subclass that re-extends or re-includes
      # `Dry::Configurable` keeps the values it inherited from its parent.
      #
      # Previously defined settings keep their readers: readers resolve against `config` at
      # call time, so they follow the class to its newly built config automatically.
      #
      # @api private
      def reset_state_if_extension_changed(klass, previous)
        return if previous.equal?(self)
        return if previous && same_behavior?(previous)

        klass.instance_variable_set(:@__config_dsl__, nil)

        if klass.instance_variable_defined?(:@__config__)
          klass.remove_instance_variable(:@__config__)
        end
      end

      # @api private
      def same_behavior?(other)
        other.config_class.equal?(config_class) &&
          other.default_undefined == default_undefined
      end
    end
  end
end
