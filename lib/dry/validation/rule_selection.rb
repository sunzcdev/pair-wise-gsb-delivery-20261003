# frozen_string_literal: true

require "dry/initializer"
require "dry/validation/constants"

module Dry
  module Validation
    # Describes which rules a contract call selected and which were skipped
    #
    # Returned by {Result#rule_selection}, it explains why each rule ran (it is
    # a base rule, matched a requested purpose, was forced as mandatory, or was
    # pulled in as a dependency) and why others were omitted.
    #
    # @api public
    class RuleSelection
      extend Dry::Initializer

      # Explains why a single rule was selected or omitted
      #
      # @api public
      class Entry
        include Dry::Equalizer(:rule, :reason, :source)

        # @!attribute [r] rule
        #   @return [Rule]
        #   @api public
        attr_reader :rule

        # @!attribute [r] reason
        #   One of +:base+, +:purpose+, +:mandatory+, +:dependency+ or +:purpose+
        #   for omitted rules.
        #   @return [Symbol]
        #   @api public
        attr_reader :reason

        # @!attribute [r] source
        #   When +reason+ is +:dependency+, the id of the rule that required it
        #   @return [Symbol, nil]
        #   @api public
        attr_reader :source

        # @api private
        def initialize(rule:, reason:, source: nil)
          @rule = rule
          @reason = reason
          @source = source
          freeze
        end

        # Root key identifying the rule
        #
        # @return [Symbol]
        #
        # @api public
        def key
          rule.id
        end

        # @api private
        def inspect
          "#<#{self.class.name} key=#{key.inspect} reason=#{reason.inspect}#{source ? " source=#{source.inspect}" : ""}>"
        end
      end

      # @!attribute [r] purposes
      #   Sorted, de-duplicated purposes the selection was built for
      #   @return [Array<Symbol>]
      #   @api public
      option :purposes

      # @!attribute [r] applied
      #   Rules that ran, in evaluation order
      #   @return [Array<Entry>]
      #   @api public
      option :applied

      # @!attribute [r] omitted
      #   Rules that were skipped because no requested purpose selected them
      #   @return [Array<Entry>]
      #   @api public
      option :omitted

      # Root keys of the rules that ran, in evaluation order
      #
      # @return [Array<Symbol>]
      #
      # @api public
      def applied_keys
        applied.map(&:key)
      end

      # Root keys of the rules that were skipped
      #
      # @return [Array<Symbol>]
      #
      # @api public
      def omitted_keys
        omitted.map(&:key)
      end

      # Return the entry describing a rule by its root key
      #
      # @param [Symbol] key
      #
      # @return [Entry, nil]
      #
      # @api public
      def entry_for(key)
        (applied + omitted).find { |entry| entry.key == key.to_sym }
      end

      # Return true when the named rule ran
      #
      # @param [Symbol] key
      #
      # @return [Boolean]
      #
      # @api public
      def applies?(key)
        applied_keys.include?(key.to_sym)
      end

      # Return true when the named rule was skipped because of purpose selection
      #
      # @param [Symbol] key
      #
      # @return [Boolean]
      #
      # @api public
      def omitted?(key)
        omitted_keys.include?(key.to_sym)
      end

      # @api private
      def inspect
        "#<#{self.class.name} purposes=#{purposes.inspect} applied=#{applied_keys.inspect} omitted=#{omitted_keys.inspect}>"
      end
    end
  end
end
