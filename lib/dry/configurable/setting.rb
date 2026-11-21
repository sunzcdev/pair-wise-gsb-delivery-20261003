# frozen_string_literal: true

module Dry
  module Configurable
    # A defined setting.
    #
    # @api public
    class Setting
      include Dry::Equalizer(:name, :default, :constructor, :children, :options, inspect: false)

      OPTIONS = %i[default reader constructor mutable cloneable settings config_class depends_on].freeze

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

      # Names of the sibling settings this setting's derived default depends on
      #
      # @api public
      attr_reader :dependencies

      # @api public
      attr_reader :options

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
        @options = options
        @dependencies = Array(options[:depends_on]).map(&:to_sym).freeze

        if derived? && !default.respond_to?(:call)
          raise ArgumentError,
            "Default for +#{name}+ must be callable when +depends_on+ is given"
        end
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

      # Returns true when the setting's default is derived from other settings
      # (declared via `depends_on:`) and must be evaluated at read time.
      #
      # @api private
      def derived?
        dependencies.any?
      end

      # @api private
      def to_value
        if children.any?
          (options[:config_class] || Config).new(children)
        else
          value = default
          value = constructor.(value) unless value.eql?(Undefined)

          mutable? ? value.dup : value
        end
      end
    end
  end
end
