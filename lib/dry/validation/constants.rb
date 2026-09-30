# frozen_string_literal: true

require "pathname"
require "dry/core/constants"

module Dry
  module Validation
    include ::Dry::Core::Constants

    DOT = "."

    # Root path is used for base errors in hash representation of error messages
    ROOT_PATH = [nil].freeze

    # Path to the default errors locale file
    DEFAULT_ERRORS_NAMESPACE = "dry_validation"

    # Path to the default errors locale file
    DEFAULT_ERRORS_PATH = Pathname(__FILE__).join("../../../../config/errors.yml").realpath.freeze

    # Mapping for block kwarg options used by block_options
    #
    # @see Rule#block_options
    BLOCK_OPTIONS_MAPPINGS = ::Hash.new { |_, key| key }.update(context: :_context).freeze

    # Error raised when `rule` specifies one or more keys that the schema doesn't specify
    InvalidKeysError = ::Class.new(::StandardError)

    # Error raised when a localized message was not found
    MissingMessageError = ::Class.new(::StandardError)

    # Error raised when trying to define a schema in a contract class that already has a schema
    DuplicateSchemaError = ::Class.new(::StandardError)

    # Error raised when a macro call doesn't match the macro's declared signature
    #
    # @see Macro::Signature
    InvalidMacroCallError = ::Class.new(::StandardError) do
      # @api private
      def initialize(contract:, rule:, macro:, reason:)
        super(
          "#{MacroCallContext.(contract, rule)} calls macro #{macro.inspect} " \
          "with invalid arguments: #{reason}"
        )
      end
    end

    # Error raised when macro expansion detects a cycle
    MacroCycleError = ::Class.new(::StandardError) do
      # @api private
      def initialize(contract:, rule:, chain:)
        super(
          "#{MacroCallContext.(contract, rule)} expands macros cyclically: " \
          "#{chain.map(&:inspect).join(' -> ')}"
        )
      end
    end

    # Builds a human-readable "Contract.rule(:key)" location for macro errors
    #
    # @api private
    MacroCallContext = lambda do |contract, rule|
      label = contract.respond_to?(:name) ? (contract.name || contract.inspect) : contract
      keys = Array(rule).map(&:inspect).join(", ")
      "#{label}.rule(#{keys})"
    end

    # Error raised during initialization of a contract that has no schema defined
    SchemaMissingError = ::Class.new(::StandardError) do
      # @api private
      def initialize(klass)
        super("#{klass} cannot be instantiated without a schema defined")
      end
    end
  end
end
