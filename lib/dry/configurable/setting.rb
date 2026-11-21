# frozen_string_literal: true

module Dry
  module Configurable
    # A defined setting.
    #
    # @api public
    class Setting
      include Dry::Equalizer(:name, :default, :constructor, :children, :options, inspect: false)

      OPTIONS = %i[default reader constructor mutable cloneable settings config_class].freeze

      DEFAULT_CONSTRUCTOR = -> v { v }.freeze

      MUTABLE_VALUE_TYPES = [Array, Hash, Set, Config].freeze

      # @api public
      attr_reader :name

      # @api public
      attr_reader :default

      # @api public
      attr_reader :mutable

      # @api public
      attr_reader :constructor

      # @api public
      attr_reader :children

      # @api public
      attr_reader :options

      # The configurable class (or module) that declared this setting, if known.
      #
      # @api private
      attr_reader :owner

      # The path of setting names leading to this setting, e.g. `[:db, :cred, :user]`.
      #
      # @api private
      attr_reader :path

      # @api private
      def self.mutable_value?(value)
        MUTABLE_VALUE_TYPES.any? { |type| value.is_a?(type) }
      end

      # @api private
      def initialize(
        name,
        default:,
        constructor: DEFAULT_CONSTRUCTOR,
        children: EMPTY_ARRAY,
        owner: nil,
        path: nil,
        **options
      )
        @name = name
        @default = default
        @mutable = children.any? || options.fetch(:mutable) {
          # Allow `cloneable` as an option alias for `mutable`
          options.fetch(:cloneable) { Setting.mutable_value?(default) }
        }
        @constructor = constructor
        @children = children
        @owner = owner
        @path = path || [name.to_sym].freeze
        @options = options
      end

      # The dotted name of this setting, including the names of any enclosing settings.
      #
      # @api private
      def full_name
        path.join(".")
      end

      # Returns a description of this setting for use in error messages, naming both the
      # declaring class (when known) and the setting's field path.
      #
      # @api private
      def description
        field = full_name.inspect
        owner_name ? "setting #{field} for #{owner_name}" : "setting #{field}"
      end

      # Runs the setting's constructor, adding the setting's owner and field path to any
      # error raised by the constructor.
      #
      # The original exception class and backtrace are preserved, so callers can still rescue
      # the same errors (and see where they originated) as before.
      #
      # @api private
      def construct(value)
        constructor.(value)
      rescue StandardError => e
        raise contextual_error(e, "cannot construct #{description} from #{value.inspect}")
      end

      # @api private
      def reader?
        options[:reader].equal?(true)
      end

      # @api public
      def mutable?
        mutable
      end
      alias_method :cloneable?, :mutable?

      # Returns the setting's value.
      #
      # Nested settings are built as config objects of the given config class (the class of
      # the config that contains them), unless the setting itself declares an explicit
      # `config_class` override. Building nested values through the runtime config class keeps
      # classes that share setting definitions, but use different config classes, fully
      # separate.
      #
      # @api private
      def to_value(config_class: Config)
        if children.any?
          nested_config_class = options[:config_class] || config_class

          begin
            nested_config_class.new(children)
          rescue StandardError => e
            raise contextual_error(
              e, "cannot initialize #{description} using #{nested_config_class}"
            )
          end
        else
          value = default
          value = construct(value) unless value.eql?(Undefined)

          mutable? ? value.dup : value
        end
      end

      private

      # Re-raises the given error with additional context prepended to its message. The
      # original class and backtrace are preserved so existing rescues keep working and the
      # failure still points at the constructor that raised it.
      def contextual_error(error, context)
        contextual = error.exception("#{context}: #{error.message}")
        contextual.set_backtrace(error.backtrace)
        contextual
      end

      def owner_name
        return unless owner

        owner.respond_to?(:name) && owner.name ? owner.name : owner.inspect
      end
    end
  end
end
