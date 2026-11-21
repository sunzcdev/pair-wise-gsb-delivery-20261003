# frozen_string_literal: true

module Dry
  module Configurable
    # Setting DSL used by the class API
    #
    # @api private
    class DSL
      VALID_NAME = /\A[a-z_]\w*\z/i

      # Method names defined on `Data` (and its ancestors) that cannot be used as setting
      # names because `Data.define` would reject them, which would break {Config#to_data}.
      DATA_RESERVED_NAMES = ::Data.instance_methods.to_set.freeze

      attr_reader :compiler

      attr_reader :ast

      # The configurable class that owns the settings declared through this DSL.
      #
      # @api private
      attr_reader :owner

      # The path of the enclosing nested settings, e.g. `[:db, :cred]` for settings declared
      # inside `setting :db { setting :cred { ... } }`.
      #
      # @api private
      attr_reader :path

      def initialize(default_undefined: false, owner: nil, path: EMPTY_ARRAY, &block)
        @compiler = Compiler.new
        @ast = []
        @default_undefined = default_undefined
        @owner = owner
        @path = path
        instance_exec(&block) if block
      end

      # Registers a new setting node and compile it into a setting object
      #
      # Compilation happens before the node is added to the AST, so a setting that fails to
      # compile is never registered. This keeps a failed definition from leaving a partial
      # setting (or a partially built nested schema) behind for the owning class.
      #
      # @see ClassMethods.setting
      # @api private
      # @return Setting
      def setting(name, **options, &block)
        unless VALID_NAME.match?(name.to_s)
          raise ArgumentError, "#{name} is not a valid setting name"
        end

        name = name.to_sym

        if DATA_RESERVED_NAMES.include?(name)
          raise ArgumentError,
            "#{name.inspect} is not a valid setting name: it conflicts with a Data " \
            "instance method, which would break Config#to_data"
        end

        ensure_valid_options(options)

        node = [:setting, [name, setting_options(name, options)]]

        node = [:nested, [node, nested_ast(name, &block)]] if block

        compiled = compiler.visit(node)

        ast << node

        compiled
      end

      def default
        @default_undefined ? Undefined : nil
      end

      private

      # Assembles the options handed to {Setting#initialize}, recording the owner and the
      # full path so failures can name both the plugin and the field.
      def setting_options(name, options)
        {
          default: options.fetch(:default) { default },
          owner: owner,
          path: [*path, name].freeze,
          **options
        }
      end

      # Builds the AST for a nested setting's block, carrying the owner and accumulated path
      # into the child DSL.
      def nested_ast(name, &block)
        DSL.new(
          default_undefined: @default_undefined,
          owner: owner,
          path: [*path, name].freeze,
          &block
        ).ast
      end

      def ensure_valid_options(options)
        return if options.none?

        invalid_keys = options.keys - Setting::OPTIONS

        raise ArgumentError, "Invalid options: #{invalid_keys.inspect}" unless invalid_keys.empty?
      end

      # Returns a tuple of valid and invalid options hashes derived from the options hash
      # given to the setting
      def valid_and_invalid_options(options)
        options.partition { |k, _| Setting::OPTIONS.include?(k) }.map(&:to_h)
      end
    end
  end
end
