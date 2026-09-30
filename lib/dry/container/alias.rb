# frozen_string_literal: true

module Dry
  class Container
    # An alias entry pointing at another key (which may itself be an alias).
    #
    # Aliases are resolved lazily so that the target may be registered after
    # the alias itself. Every alias in a chain resolves to the same terminal
    # item, sharing its object and its memoization/cache state.
    #
    # @api private
    class Alias
      # @return [String] the key this alias points at
      attr_reader :target

      # @param [String, Symbol] target
      #   The key this alias points at
      #
      # @return [Dry::Container::Alias]
      #
      # @api private
      def initialize(target)
        @target = target.to_s.dup.freeze
      end
    end
  end
end
