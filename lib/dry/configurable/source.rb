# frozen_string_literal: true

module Dry
  module Configurable
    # Describes where the value of a single setting (or a nested group of settings) came from.
    #
    # Returned by {Config#source}. It records only the provenance category, not a filename or a
    # call stack.
    #
    # @api public
    class Source
      include Dry::Equalizer(:origin, :assigned, :mutated, inspect: false)

      # Provenance categories, ordered from least to most specific.
      #
      # - `:default` – the value is the setting's default; it was never explicitly assigned.
      # - `:inherited` – the value was explicitly assigned on an ancestor class and inherited.
      # - `:local` – the value was explicitly assigned on this config itself.
      #
      # @api public
      ORIGINS = %i[default inherited local].freeze

      # @api private
      def self.default
        new(origin: :default, assigned: false, mutated: false)
      end

      # @api private
      def self.local
        new(origin: :local, assigned: true, mutated: false)
      end

      # @return [Symbol] one of {ORIGINS}
      #
      # @api public
      attr_reader :origin
      alias_method :category, :origin

      # @return [Boolean]
      #
      # @api public
      attr_reader :assigned

      # @return [Boolean]
      #
      # @api public
      attr_reader :mutated

      # @api private
      def initialize(origin:, assigned:, mutated: false)
        @origin = origin
        @assigned = assigned
        @mutated = mutated
      end

      # Returns true if the value was explicitly assigned (either locally or on an ancestor).
      #
      # Assigning a value that happens to equal the default still counts as an assignment.
      #
      # @return [Boolean]
      #
      # @api public
      def assigned?
        @assigned
      end

      # Returns true if the value is still the default-sourced object but its contents have been
      # mutated so that it no longer compares equally to a fresh default.
      #
      # This can only be true when {#origin} is `:default`: a mutable default (e.g. an `Array`)
      # that was read and then changed in place, without ever being explicitly assigned.
      #
      # @return [Boolean]
      #
      # @api public
      def mutated?
        @mutated
      end

      # @return [Boolean]
      #
      # @api public
      def default?
        origin.equal?(:default)
      end

      # @return [Boolean]
      #
      # @api public
      def inherited?
        origin.equal?(:inherited)
      end

      # @return [Boolean]
      #
      # @api public
      def local?
        origin.equal?(:local)
      end

      # @return [Hash]
      #
      # @api public
      def to_h
        {origin: origin, assigned: assigned?, mutated: mutated?}
      end
      alias_method :to_hash, :to_h

      # @api public
      def inspect
        "#<#{self.class.name} #{to_h.inspect}>"
      end
    end
  end
end
