# frozen_string_literal: true

require "dry/validation/constants"

module Dry
  module Validation
    # Rules capture configuration and evaluator blocks
    #
    # When a rule is applied, it creates an `Evaluator` using schema result and its
    # block will be evaluated in the context of the evaluator.
    #
    # @see Contract#rule
    #
    # @api public
    class Rule < Function
      include ::Dry::Equalizer(:keys, :block, inspect: false)

      # @!attribute [r] keys
      #   @return [Array<Symbol, String, Hash>]
      #   @api private
      option :keys

      # @!attribute [r] macros
      #   @return [Array<Symbol>]
      #   @api private
      option :macros, default: proc { EMPTY_ARRAY.dup }

      # @!attribute [r] purposes
      #   Purposes for which this rule is selected
      #
      # An empty set means the rule is a base rule that runs regardless of the
      # selected purpose(s).
      #
      #   @return [Array<Symbol>]
      #   @api private
      option :purposes, default: proc { EMPTY_ARRAY.dup }

      # @!attribute [r] mandatory
      #   Purposes for which this rule is forced into the selection even if it
      #   was not requested directly
      #
      #   @return [Array<Symbol>]
      #   @api private
      option :mandatory, default: proc { EMPTY_ARRAY.dup }

      # @!attribute [r] depends_on
      #   Root keys of other rules this rule depends on
      #
      #   @return [Array<Symbol>]
      #   @api private
      option :depends_on, default: proc { EMPTY_ARRAY.dup }

      # Evaluate the rule within the provided context
      #
      # @param [Contract] contract
      # @param [Result] result
      #
      # @api private
      def call(contract, result)
        Evaluator.new(
          contract,
          keys: keys,
          macros: macros,
          block_options: block_options,
          result: result,
          values: result.values,
          _context: result.context,
          &block
        )
      end

      # Define which macros should be executed
      #
      # @see Contract#rule
      # @return [Rule]
      #
      # @api public
      def validate(*macros, &block)
        @macros = parse_macros(*macros)
        @block = block if block
        self
      end

      # Tag the rule with one or more purposes
      #
      # A tagged rule is only selected when one of its purposes is requested.
      #
      # @example
      #   rule(:consent).for(:submit) do
      #     key.failure("must be accepted") unless values[:consent]
      #   end
      #
      # @param [Array<Symbol>] names
      #
      # @return [Rule]
      #
      # @api public
      def for(*names, &block)
        @purposes = (purposes + names.flatten).uniq
        @block = block if block
        self
      end

      # Declare that the rule must always run for the given purposes
      #
      # Such a rule is forced into a combination whenever one of the given
      # purposes is selected, even if it is not tagged with that purpose.
      #
      # @example
      #   rule(:terms_accepted).required_when(:submit) do
      #     key.failure("must accept the terms") unless values[:terms]
      #   end
      #
      # @param [Array<Symbol>] names
      #
      # @return [Rule]
      #
      # @api public
      def required_when(*names, &block)
        @mandatory = (mandatory + names.flatten).uniq
        @block = block if block
        self
      end

      # Declare an explicit dependency on one or more other rules
      #
      # The referenced rules are pulled into the selection transitively and are
      # guaranteed to be evaluated before this rule.
      #
      # @example
      #   rule(:summary).depends(:details) do
      #     key.failure("inconsistent") unless values[:summary].valid?
      #   end
      #
      # @param [Array<Symbol>] keys
      #
      # @return [Rule]
      #
      # @api public
      def depends(*keys, &block)
        @depends_on = (depends_on + keys.flatten.map { root_key(_1) }).uniq
        @block = block if block
        self
      end

      # Stable identifier used by the compiler to wire dependencies
      #
      # @return [Symbol]
      #
      # @api private
      def id
        keys.empty? ? object_id : root_key(keys.first)
      end

      # Return true when the rule is a base rule (not tagged with any purpose)
      #
      # @return [Boolean]
      #
      # @api private
      def base?
        purposes.empty?
      end

      # Define a validation function for each element of an array
      #
      # The function will be applied only if schema checks passed
      # for a given array item.
      #
      # @example
      #   rule(:nums).each do |index:|
      #     key([:number, index]).failure("must be greater than 0") if value < 0
      #   end
      #   rule(:nums).each(min: 3)
      #   rule(address: :city) do
      #      key.failure("oops") if value != 'Munich'
      #   end
      #
      # @return [Rule]
      #
      # @api public
      #
      # rubocop:disable Metrics/AbcSize
      def each(*macros, &block)
        root = keys[0]
        macros = parse_macros(*macros)
        @keys = []

        @block = proc do
          unless result.base_error?(root) || !values.key?(root) || values[root].nil?
            values[root].each_with_index do |_, idx|
              path = [*Schema::Path[root].to_a, idx]

              next if result.schema_error?(path)

              evaluator = with(macros: macros, keys: [path], index: idx, &block)

              failures.concat(evaluator.failures)
            end
          end
        end

        @block_options = map_keywords(block) if block

        self
      end
      # rubocop:enable Metrics/AbcSize

      # Return a nice string representation
      #
      # @return [String]
      #
      # @api public
      def inspect
        %(#<#{self.class} keys=#{keys.inspect}>)
      end

      # Parse function arguments into macros structure
      #
      # @return [Array]
      #
      # @api private
      def parse_macros(*args)
        args.each_with_object([]) do |spec, macros|
          case spec
          when Hash
            add_macro_from_hash(macros, spec)
          else
            macros << Array(spec)
          end
        end
      end

      def add_macro_from_hash(macros, spec)
        spec.each do |k, v|
          macros << [k, v.is_a?(::Array) ? v : [v]]
        end
      end

      private

      # Normalize a key spec into its root symbol identifier
      #
      # @return [Symbol]
      #
      # @api private
      def root_key(spec)
        head = Array(spec).first

        case head
        when ::Hash
          root_key(head.keys.first)
        when ::Symbol, ::String
          head.to_sym
        else
          Schema::Path[spec].keys.first.to_sym
        end
      end
    end
  end
end
