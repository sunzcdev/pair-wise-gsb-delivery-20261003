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

      # @!attribute [r] name
      #   @return [Symbol, nil] Explicit identifier used to reference the rule as a dependency
      #   @api private
      option :name, default: proc { nil }, optional: true

      # @!attribute [r] keys
      #   @return [Array<Symbol, String, Hash>]
      #   @api private
      option :keys

      # @!attribute [r] purposes
      #   @return [Array<Symbol>] Purposes for which this rule is eligible
      #   @api private
      option :purposes, default: proc { EMPTY_ARRAY.dup }

      # @!attribute [r] depends_on
      #   @return [Array<Symbol>] Names of rules that must run before this one
      #   @api private
      option :depends_on, default: proc { EMPTY_ARRAY.dup }

      # @!attribute [r] macros
      #   @return [Array<Symbol>]
      #   @api private
      option :macros, default: proc { EMPTY_ARRAY.dup }

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

      # Stable identifier used to resolve cross-purpose rule dependencies
      #
      # @return [Symbol]
      #
      # @api private
      def id
        return name unless name.nil?

        key = keys.first
        return :"rule_#{object_id}" unless key.is_a?(::Symbol) || key.is_a?(::String)

        Schema::Path[key].to_a.map(&:to_s).join(DOT).to_sym
      end

      # Annotate the rule with the business purposes it serves
      #
      # A rule without any purpose is a base rule that runs for every
      # purpose combination.
      #
      # @example
      #   rule(:terms).purposes(:submit) do
      #     key.failure("must be accepted") unless values[:terms]
      #   end
      #
      # @return [Rule]
      #
      # @api public
      def for_purposes(*names)
        @purposes = purposes.concat(names.flatten.map(&:to_sym)).uniq
        self
      end

      # Declare rules that must be compiled and run before this one
      #
      # Dependencies are pulled in across purposes, so a selected rule can
      # rely on a rule that belongs to a purpose that was not requested.
      #
      # @example
      #   rule(:total).depends_on(:line_items).purposes(:submit) do
      #     key.failure("mismatch") if values[:total] < 0
      #   end
      #
      # @return [Rule]
      #
      # @api public
      def depends(*names)
        @depends_on = depends_on.concat(names.flatten.map(&:to_sym)).uniq
        self
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
    end
  end
end
